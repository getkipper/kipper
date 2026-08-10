package migration

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/domain"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// disposition is what happens to a migrated app's route host.
type disposition string

const (
	// dispositionMove: a custom domain the user owns. Its route is saved and
	// reapplied on the target at cutover, and its host is stripped during the
	// run so the target serves a temporary derived host for verification.
	dispositionMove disposition = "move"
	// dispositionCoexist: a platform subdomain (or a custom domain the operator
	// chose to keep on the source). The host is stripped and never saved, so the
	// target keeps its own derived host and the source keeps serving its own.
	dispositionCoexist disposition = "coexist"
	// dispositionGateway: a *.kipper.run host. Stripped and never saved; the
	// target derives its own gateway host.
	dispositionGateway disposition = "gateway"
)

// domainResolution is the session-wide decision about every migrated app's
// route host, computed once before any transfer so the app secrets (phase 1)
// and the app specs (phase 3) rewrite consistently.
type domainResolution struct {
	// byApp maps "namespace/name" to the app's route disposition. Only apps
	// with a route appear.
	byApp map[string]disposition
	// rewrites maps an exact source host to its target equivalent, for the
	// coexisting platform apps whose derived host changes on the target. Empty
	// in Mode B, where adopting the base domain keeps the source hosts valid.
	rewrites map[string]string
	// appSecrets is the set of "namespace/app-<app>-secrets" names across the
	// migration, so the rewrite touches only the migrated apps' own sensitive
	// secrets and not TLS, registry, git, or user-owned ones.
	appSecrets map[string]bool
}

func appKey(namespace, name string) string { return namespace + "/" + name }

// classifyAppRoute returns the domain class and the resulting disposition for
// one app's route host. effectiveHost is the stored host, or the derived host
// when the stored one is empty. A custom domain the operator chose to keep
// coexists instead of moving. Shared by the plan and the run so what the report
// shows and what the migration does can never drift.
func classifyAppRoute(effectiveHost, derived, key string, keep map[string]bool) (domain.DomainClass, disposition) {
	class := domain.ClassifyHost(effectiveHost, derived)
	switch class {
	case domain.DomainClassGateway:
		return class, dispositionGateway
	case domain.DomainClassPlatform:
		return class, dispositionCoexist
	default:
		if keep[key] {
			return class, dispositionCoexist
		}
		return class, dispositionMove
	}
}

// resolveDomains classifies every app across the session's projects against the
// source base domain and the operator's keep/move choices, and builds the env
// rewrite table for the coexisting apps.
func (h *Handler) resolveDomains(ctx context.Context, session *Session) (*domainResolution, error) {
	res := &domainResolution{
		byApp:      map[string]disposition{},
		rewrites:   map[string]string{},
		appSecrets: map[string]bool{},
	}
	sourceBase := h.Domain
	targetBase := session.TargetBaseDomain

	for _, project := range session.Projects {
		namespaces, err := h.getProjectNamespaces(ctx, project)
		if err != nil {
			return nil, err
		}
		for _, ns := range namespaces {
			// A failed namespace read leaves the environment unknown, and
			// guessing env="" would derive the wrong host and misclassify a
			// platform subdomain as a mover (leaking it onto the DNS screen), so
			// fail closed rather than continue.
			nsObj, nsErr := h.Client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
			if nsErr != nil {
				return nil, fmt.Errorf("reading namespace %s for domain classification: %w", ns, nsErr)
			}
			env := nsObj.Labels["kipper.run/environment"]
			var appList kipperv1.AppList
			if err := h.CRClient.List(ctx, &appList, crclient.InNamespace(ns)); err != nil {
				return nil, err
			}
			for i := range appList.Items {
				app := &appList.Items[i]
				// Every migrated app carries its own sensitive secret, whether
				// or not it has a public route.
				res.appSecrets[appKey(ns, secretname.Secrets(secretname.KindApp, app.Name))] = true

				if app.Spec.Route == nil {
					continue
				}
				prefix := domain.AppRoutePrefix(app.Name, env)
				derived := domain.SubdomainFor(prefix, sourceBase)
				effective := app.Spec.Route.Host
				if effective == "" {
					effective = derived
				}
				key := appKey(ns, app.Name)
				class, disp := classifyAppRoute(effective, derived, key, session.KeepDomains)
				res.byApp[key] = disp

				// A coexisting platform app lands on the target's own derived
				// host, so references to its source host move to the target
				// equivalent — unless the base domain itself is adopted (Mode B).
				if class == domain.DomainClassPlatform && !session.MoveBaseDomain && targetBase != "" {
					if tgt := domain.TargetEquivalent(prefix, targetBase); tgt != "" {
						res.rewrites[derived] = tgt
					}
				}
			}
		}
	}
	return res, nil
}

// isHostChar reports whether c can appear inside a DNS hostname. It bounds the
// rewrite so only a complete hostname is matched, never a substring of a longer
// one.
func isHostChar(c byte) bool {
	switch {
	case c == '.' || c == '-':
		return true
	case c >= 'a' && c <= 'z':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9':
		return true
	default:
		return false
	}
}

// rewriteSpecEnv rewrites coexisting source-host references in a resource's
// spec.env (App, Function, or Job — all carry a map[string]string env that
// marshals to specMap["env"]). An empty rewrite table (Mode B) is a no-op.
func rewriteSpecEnv(specMap map[string]interface{}, res *domainResolution) {
	if res == nil {
		return
	}
	envMap, ok := specMap["env"].(map[string]interface{})
	if !ok {
		return
	}
	for k, v := range envMap {
		if s, isStr := v.(string); isStr {
			envMap[k] = rewriteHostRefs(s, res.rewrites)
		}
	}
}

// rewriteHostRefs replaces each exact source host in value with its target
// equivalent. Only complete hostnames match (bounded by non-hostname chars), so
// a longer hostname (storefront.community) or an unrelated substring is never
// rewritten. Values with no rewrite pair pass through untouched.
func rewriteHostRefs(value string, rewrites map[string]string) string {
	for from, to := range rewrites {
		value = rewriteOneHost(value, from, to)
	}
	return value
}

func rewriteOneHost(value, from, to string) string {
	n, m := len(value), len(from)
	if m == 0 || from == to || n < m {
		return value
	}
	var b strings.Builder
	i := 0
	for i <= n-m {
		// DNS names are case-insensitive, so match the host fold-insensitively;
		// `from` is already lowercase.
		if strings.EqualFold(value[i:i+m], from) {
			end := i + m
			// userinfo@host (a DATABASE_URL / DSN, or an authenticated HTTP URL)
			// is a real route reference and the common case, so a preceding '@'
			// still matches. An exact app-derived host used as an email domain
			// is implausible, so we accept that tradeoff.
			beforeOK := i == 0 || !isHostChar(value[i-1])
			// A trailing root dot ends the FQDN (host.storefront.com. is the same
			// host); a label after the dot would make this a shorter suffix of a
			// longer host, which must not match.
			afterOK := end == n || !isHostChar(value[end]) ||
				(value[end] == '.' && (end+1 == n || !isLabelChar(value[end+1])))
			if beforeOK && afterOK {
				b.WriteString(to)
				i = end
				continue
			}
		}
		b.WriteByte(value[i])
		i++
	}
	b.WriteString(value[i:])
	return b.String()
}

// isLabelChar reports whether c can appear inside a single DNS label (no dot).
func isLabelChar(c byte) bool {
	switch {
	case c == '-':
		return true
	case c >= 'a' && c <= 'z':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9':
		return true
	default:
		return false
	}
}
