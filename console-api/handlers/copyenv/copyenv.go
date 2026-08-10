// Package copyenv copies every Kipper-owned spec from one project
// environment (a Kubernetes namespace) into another. It is the engine
// behind "create environment with copy_from=<source>" and the future
// copy-environment wizard.
//
// Phase 1 behaviour: deep-copy App, Service, Volume, Function, Job CRs
// and user-owned Secrets. Service credentials regenerate (the service
// reconciler does that when it sees a fresh Service CR). PVC data does
// not follow — Volumes start empty in the target. Routes are intentionally
// dropped from copied apps to avoid Ingress / cert-manager conflicts with
// the source environment.
//
// Phase 2 will plug in per-resource overrides (route hostnames, env-var
// edits, secret rotation). Phase 3 will add data migration for databases
// and PVCs. The Plan/Execute split here exists to make those additions
// non-breaking — new operation kinds slot in without touching the planner
// shape.
package copyenv

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/domain"
	"github.com/getkipper/kipper/controller/pkg/applink"
	"github.com/getkipper/kipper/controller/pkg/labels"
)

// Options controls what gets copied. Phase 1 honours Source/Target plus
// the route-related fields. Phase 2 adds Overrides, which the wizard fills
// in from user input collected before submit.
type Options struct {
	// Source is the namespace to read from (e.g. "myapp-test").
	Source string
	// Target is the namespace to write into (e.g. "myapp-prod").
	Target string
	// TargetEnv is the environment name (e.g. "prod"). Used to build
	// per-env hostnames. Defaults to a derived value if empty (everything
	// after the project prefix in Target).
	TargetEnv string
	// ClusterDomain is the operator's CLUSTER_DOMAIN — example.com,
	// example.kipper.run, etc. Required when AssignDefaultRoutes is true.
	ClusterDomain string
	// AssignDefaultRoutes controls whether copied apps get a fresh
	// per-env hostname automatically. When true, each app that had a
	// route in the source env gets a route in the target env pointing
	// to <app>-<env>.<clusterDomain>. When false, copied apps have no
	// route until the user sets one. Per-app overrides in AppOverrides
	// take precedence over this default behaviour.
	AssignDefaultRoutes bool
	// AppOverrides applies per-app changes from the wizard. The map is
	// keyed by source-app name. Empty / nil means "no overrides; copy
	// verbatim (with the auto-route from AssignDefaultRoutes)".
	AppOverrides map[string]AppOverride
}

// AppOverride captures the wizard's per-app edits. Pointer fields
// distinguish "not set, leave source as-is" from a deliberate value:
//   - Route nil      → no override (auto-host or source route, per
//     AssignDefaultRoutes)
//   - Route non-nil  → use these exact route values
//   - Env nil        → no override (copy source env verbatim)
//   - Env non-nil    → replace env map wholesale with the wizard's value
//   - Replicas nil   → no override
//   - Resources nil  → no override
type AppOverride struct {
	Route     *RouteOverride         `json:"route,omitempty"`
	Env       map[string]string      `json:"env,omitempty"`
	Replicas  *int32                 `json:"replicas,omitempty"`
	Resources *kipperv1.AppResources `json:"resources,omitempty"`
}

// RouteOverride is the wizard's preferred hostname / path for an app. An
// empty Host means "drop the route entirely" (the wizard's "skip" option).
type RouteOverride struct {
	Host string `json:"host"`
	Path string `json:"path,omitempty"`
}

// Summary tells the caller (and the user) what happened.
type Summary struct {
	Apps      int      `json:"apps"`
	Services  int      `json:"services"`
	Volumes   int      `json:"volumes"`
	Functions int      `json:"functions"`
	Jobs      int      `json:"jobs"`
	Secrets   int      `json:"secrets"`
	Warnings  []string `json:"warnings,omitempty"`
}

// Copier orchestrates the copy. CR-typed clients handle the Kipper CRs;
// the typed kubernetes client handles raw Secrets.
type Copier struct {
	CRClient crclient.Client
	Client   kubernetes.Interface
}

// Run lists every supported resource in opts.Source and creates a
// deep-copied equivalent in opts.Target. Errors short-circuit the rest of
// the run — the caller is responsible for cleanup if a partial copy is
// unacceptable. For Phase 1 we accept partial state: each resource kind
// is independent, and re-running the copy is safe for kinds that AlreadyExist
// (we treat that as "fine, leave it alone").
func (c *Copier) Run(ctx context.Context, opts Options) (Summary, error) {
	var s Summary
	if opts.Source == "" || opts.Target == "" {
		return s, fmt.Errorf("source and target namespaces are required")
	}
	if opts.Source == opts.Target {
		return s, fmt.Errorf("source and target must differ")
	}

	apps, appWarnings, err := c.copyApps(ctx, opts)
	if err != nil {
		return s, fmt.Errorf("copying apps: %w", err)
	}
	s.Apps = apps
	s.Warnings = append(s.Warnings, appWarnings...)

	services, err := c.copyServices(ctx, opts)
	if err != nil {
		return s, fmt.Errorf("copying services: %w", err)
	}
	s.Services = services

	volumes, err := c.copyVolumes(ctx, opts)
	if err != nil {
		return s, fmt.Errorf("copying volumes: %w", err)
	}
	s.Volumes = volumes

	functions, err := c.copyFunctions(ctx, opts)
	if err != nil {
		return s, fmt.Errorf("copying functions: %w", err)
	}
	s.Functions = functions

	jobs, err := c.copyJobs(ctx, opts)
	if err != nil {
		return s, fmt.Errorf("copying jobs: %w", err)
	}
	s.Jobs = jobs

	secrets, warnings, err := c.copySecrets(ctx, opts)
	if err != nil {
		return s, fmt.Errorf("copying secrets: %w", err)
	}
	s.Secrets = secrets
	s.Warnings = append(s.Warnings, warnings...)

	if s.Apps > 0 && opts.AssignDefaultRoutes && opts.ClusterDomain != "" {
		s.Warnings = append(s.Warnings, fmt.Sprintf(
			"Apps with a route in %s were given fresh hostnames under *.%s. Open them in a browser to confirm. Use the route panel to switch to a custom domain when you're ready.",
			opts.Source, opts.ClusterDomain,
		))
	} else if s.Apps > 0 {
		s.Warnings = append(s.Warnings,
			"Routes were not copied. Set the public hostname for each app in the new environment via the route panel.",
		)
	}
	return s, nil
}

// copyApps lists App CRs in the source namespace and creates fresh copies
// in the target namespace. Identity/status fields are cleared so the API
// server accepts the create. The source route is replaced with a fresh
// per-env hostname (see newAppForTarget) to avoid the Ingress/cert
// conflict that a literal copy would cause.
func (c *Copier) copyApps(ctx context.Context, opts Options) (int, []string, error) {
	var src kipperv1.AppList
	if err := c.CRClient.List(ctx, &src, crclient.InNamespace(opts.Source)); err != nil {
		return 0, nil, err
	}

	count := 0
	var warnings []string
	for i := range src.Items {
		copied, dropped := newAppForTarget(&src.Items[i], opts)
		if err := c.CRClient.Create(ctx, copied); err != nil {
			if errors.IsAlreadyExists(err) {
				continue
			}
			return count, warnings, fmt.Errorf("creating app %s/%s: %w", copied.Namespace, copied.Name, err)
		}
		// A dependency the copy could not bring along is the operator's to
		// re-establish, and saying nothing leaves them to discover it as a
		// connection that stopped working.
		if len(dropped) > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"%s no longer links to %s: a link outside this project's environment is not copied; re-link it if the new environment needs it",
				copied.Name, strings.Join(dropped, ", ")))
		}
		count++
	}
	return count, warnings, nil
}

func newAppForTarget(src *kipperv1.App, opts Options) (*kipperv1.App, []string) {
	out := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{
			Name:        src.Name,
			Namespace:   opts.Target,
			Labels:      copyMap(src.Labels),
			Annotations: cleanedPromotionAnnotations(src.Annotations),
		},
		Spec: *src.Spec.DeepCopy(),
	}

	override, hasOverride := opts.AppOverrides[src.Name]

	// Env: wizard sends the full map (not a partial diff) so it overrides
	// wholesale when present.
	if hasOverride && override.Env != nil {
		out.Spec.Env = copyMap(override.Env)
	}

	// Links follow the same rule routes do: a dependency that names the source
	// environment must not silently reach back into it from the copy. One inside
	// the project moves to the target environment, which is the same app one
	// environment along. One naming another project is dropped with its URL —
	// the equivalent over there may not exist, may not have consented, and
	// guessing which environment was meant is not something a copy should do.
	// Left as they were, a fresh prod environment would depend on a test-side
	// backend and look entirely healthy doing it.
	//
	droppedLinks := rewriteLinksForTarget(out, opts)

	// Replicas / Resources: pointer-set means "use this", nil means
	// "leave source value alone".
	if hasOverride && override.Replicas != nil {
		r := *override.Replicas
		out.Spec.Replicas = &r
	}
	if hasOverride && override.Resources != nil {
		out.Spec.Resources = *override.Resources
	}

	// Routes — three cases in priority order:
	//   1. Wizard route override → use those exact values
	//   2. AssignDefaultRoutes + source had a route → fresh auto-host
	//   3. Otherwise → no route
	switch {
	case hasOverride && override.Route != nil:
		if override.Route.Host == "" {
			out.Spec.Route = nil
		} else {
			route := &kipperv1.AppRoute{}
			if src.Spec.Route != nil {
				route = src.Spec.Route.DeepCopy()
			}
			route.Host = override.Route.Host
			route.Path = override.Route.Path
			if route.Path == "" {
				route.Path = "/"
			}
			out.Spec.Route = route
		}
	case src.Spec.Route != nil && opts.AssignDefaultRoutes && opts.ClusterDomain != "":
		newHost := defaultHostFor(src.Name, opts.TargetEnv, opts.ClusterDomain)
		if newHost == "" {
			out.Spec.Route = nil
		} else {
			route := src.Spec.Route.DeepCopy()
			route.Host = newHost
			if route.Path == "" {
				route.Path = "/"
			}
			out.Spec.Route = route
		}
	default:
		out.Spec.Route = nil
	}

	return out, droppedLinks
}

// defaultHostFor builds the per-env hostname for a freshly-copied app.
// It reuses the SubdomainFor convention so kipper.run-style joins (with a
// hyphen) and custom-domain joins (with a dot) come out consistent with
// the rest of the system.
func defaultHostFor(appName, envName, clusterDomain string) string {
	if clusterDomain == "" {
		return ""
	}
	prefix := appName
	if envName != "" && envName != "default" {
		prefix = appName + "-" + envName
	}
	return domain.SubdomainFor(prefix, strings.TrimPrefix(clusterDomain, "."))
}

// copyServices deep-copies Service CRs. Status is dropped so the new
// service reconciles from a clean slate; credentials regenerate on first
// reconcile of the new resource.
func (c *Copier) copyServices(ctx context.Context, opts Options) (int, error) {
	var src kipperv1.ServiceList
	if err := c.CRClient.List(ctx, &src, crclient.InNamespace(opts.Source)); err != nil {
		return 0, err
	}

	count := 0
	for i := range src.Items {
		out := &kipperv1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:        src.Items[i].Name,
				Namespace:   opts.Target,
				Labels:      copyMap(src.Items[i].Labels),
				Annotations: copyMap(src.Items[i].Annotations),
			},
			Spec: *src.Items[i].Spec.DeepCopy(),
		}
		if err := c.CRClient.Create(ctx, out); err != nil {
			if errors.IsAlreadyExists(err) {
				continue
			}
			return count, fmt.Errorf("creating service %s/%s: %w", out.Namespace, out.Name, err)
		}
		count++
	}
	return count, nil
}

// copyVolumes deep-copies Volume CRs. The volume reconciler creates a
// fresh, empty PVC in the target namespace; PVC data does not follow
// (data migration is Phase 3).
func (c *Copier) copyVolumes(ctx context.Context, opts Options) (int, error) {
	var src kipperv1.VolumeList
	if err := c.CRClient.List(ctx, &src, crclient.InNamespace(opts.Source)); err != nil {
		return 0, err
	}

	count := 0
	for i := range src.Items {
		out := &kipperv1.Volume{
			ObjectMeta: metav1.ObjectMeta{
				Name:        src.Items[i].Name,
				Namespace:   opts.Target,
				Labels:      copyMap(src.Items[i].Labels),
				Annotations: copyMap(src.Items[i].Annotations),
			},
			Spec: *src.Items[i].Spec.DeepCopy(),
		}
		if err := c.CRClient.Create(ctx, out); err != nil {
			if errors.IsAlreadyExists(err) {
				continue
			}
			return count, fmt.Errorf("creating volume %s/%s: %w", out.Namespace, out.Name, err)
		}
		count++
	}
	return count, nil
}

func (c *Copier) copyFunctions(ctx context.Context, opts Options) (int, error) {
	var src kipperv1.FunctionList
	if err := c.CRClient.List(ctx, &src, crclient.InNamespace(opts.Source)); err != nil {
		return 0, err
	}

	count := 0
	for i := range src.Items {
		out := &kipperv1.Function{
			ObjectMeta: metav1.ObjectMeta{
				Name:        src.Items[i].Name,
				Namespace:   opts.Target,
				Labels:      copyMap(src.Items[i].Labels),
				Annotations: copyMap(src.Items[i].Annotations),
			},
			Spec: *src.Items[i].Spec.DeepCopy(),
		}
		if err := c.CRClient.Create(ctx, out); err != nil {
			if errors.IsAlreadyExists(err) {
				continue
			}
			return count, fmt.Errorf("creating function %s/%s: %w", out.Namespace, out.Name, err)
		}
		count++
	}
	return count, nil
}

func (c *Copier) copyJobs(ctx context.Context, opts Options) (int, error) {
	var src kipperv1.JobList
	if err := c.CRClient.List(ctx, &src, crclient.InNamespace(opts.Source)); err != nil {
		return 0, err
	}

	count := 0
	for i := range src.Items {
		out := &kipperv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:        src.Items[i].Name,
				Namespace:   opts.Target,
				Labels:      copyMap(src.Items[i].Labels),
				Annotations: copyMap(src.Items[i].Annotations),
			},
			Spec: *src.Items[i].Spec.DeepCopy(),
		}
		if err := c.CRClient.Create(ctx, out); err != nil {
			if errors.IsAlreadyExists(err) {
				continue
			}
			return count, fmt.Errorf("creating job %s/%s: %w", out.Namespace, out.Name, err)
		}
		count++
	}
	return count, nil
}

// copySecrets copies user-owned secrets. Skipped:
//   - service credentials (regenerated by the service reconciler)
//   - per-app binding secrets (regenerated when the app reconciles its
//     Service binding against the new service)
//   - registry pull secrets (system-managed; the workload reconcilers stage
//     them on demand in the new namespace)
//   - secrets owned by a controller (their controller will recreate them
//     in the new namespace from the corresponding spec)
func (c *Copier) copySecrets(ctx context.Context, opts Options) (int, []string, error) {
	list, err := c.Client.CoreV1().Secrets(opts.Source).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=kipper",
	})
	if err != nil {
		return 0, nil, err
	}

	var warnings []string
	count := 0
	for i := range list.Items {
		s := &list.Items[i]
		if shouldSkipSecret(s) {
			continue
		}
		out := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        s.Name,
				Namespace:   opts.Target,
				Labels:      copyMap(s.Labels),
				Annotations: copyMap(s.Annotations),
			},
			Type: s.Type,
			Data: copyByteMap(s.Data),
		}
		if _, err := c.Client.CoreV1().Secrets(opts.Target).Create(ctx, out, metav1.CreateOptions{}); err != nil {
			if errors.IsAlreadyExists(err) {
				continue
			}
			warnings = append(warnings, fmt.Sprintf("could not copy secret %s: %v", s.Name, err))
			continue
		}
		count++
	}
	return count, warnings, nil
}

func shouldSkipSecret(s *corev1.Secret) bool {
	if _, ok := s.Labels["kipper.run/service-type"]; ok {
		return true
	}
	if s.Labels[labels.Binding] == "true" {
		return true
	}
	if s.Labels["kipper.run/registry"] == "true" {
		return true
	}
	for _, ref := range s.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// cleanedPromotionAnnotations strips per-cycle promotion metadata from
// the source app — the new env hasn't promoted anything yet, so carrying
// "promoted-from=test" annotations into the freshly created prod app
// would lie to the user about provenance.
func cleanedPromotionAnnotations(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	skip := map[string]struct{}{
		"kipper.run/promoted-from":  {},
		"kipper.run/promoted-at":    {},
		"kipper.run/promoted-image": {},
		"kipper.run/deploy-history": {},
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if _, drop := skip[k]; drop {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func copyMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyByteMap(in map[string][]byte) map[string][]byte {
	if in == nil {
		return nil
	}
	out := make(map[string][]byte, len(in))
	for k, v := range in {
		cp := make([]byte, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// rewriteLinksForTarget moves this app's links into the environment it was
// copied into, dropping the ones it cannot bring along.
//
// A link into the source environment is the same dependency one environment
// over, so it follows the copy. Anything else is dropped: another project's app
// may not exist over here and may not have consented, and a sibling environment
// of this project is a deliberate cross-environment dependency that the copy has
// no basis to reproduce. Left alone, a fresh prod environment would quietly
// depend on a test-side backend and look healthy doing it.
//
// A dropped link takes any stored address with it. Nothing stores one now — the
// reconciler derives it — but an app linked before that carries one in spec.env
// and nothing migrates it, so the copy would arrive holding an address for a
// dependency it no longer declares, with no allowance to reach it and nothing
// on either surface saying why.
func rewriteLinksForTarget(app *kipperv1.App, opts Options) []string {
	if len(app.Spec.Links) == 0 {
		return nil
	}
	var dropped []string
	kept := make([]kipperv1.AppLink, 0, len(app.Spec.Links))
	for _, link := range app.Spec.Links {
		if link.Namespace == opts.Source {
			link.Namespace = opts.Target
			kept = append(kept, link)
			continue
		}
		dropped = append(dropped, fmt.Sprintf("%s/%s", link.Namespace, link.App))
		delete(app.Spec.Env, applink.EnvKey(link.App))
	}
	if len(kept) == 0 {
		app.Spec.Links = nil
	} else {
		app.Spec.Links = kept
	}
	return dropped
}
