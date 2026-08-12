// Package serving renders a cluster's serving identity — the three Ingresses
// (console, console-api, Dex), the console-api OIDC env family, and the Dex
// config — from one desired Spec, so the installer and the reconciler produce
// identical resources instead of two hand-maintained templates. Render is a
// pure function of the Spec and the live state it cannot recompute (Carry), and
// every phase of a host transition is a fixed point: re-applying the same phase
// changes nothing.
package serving

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/getkipper/kipper/controller/pkg/dexcfg"
	"github.com/getkipper/kipper/controller/pkg/hostnames"
)

const (
	consolePrefix    = "console"
	consoleAPIPrefix = "console-api"
	dexPrefix        = "dex"

	kipperSystemNS = "kipper-system"
	dexNS          = "dex"

	clusterIssuerAnnotation = "cert-manager.io/cluster-issuer"
	clusterIssuer           = "letsencrypt-prod"
	ingressClass            = "traefik"

	// dexOIDCClientSecret is the Secret console-api reads the Dex client secret
	// from, replacing the value baked into env at install time.
	dexOIDCClientSecretName = "dex-oidc-client"
	dexOIDCClientSecretKey  = "secret"
)

// Overrides pins a per-service host (a custom domain). Empty means derive from
// the cluster domain by convention.
type Overrides struct {
	Console    string
	ConsoleAPI string
	Dex        string
}

// HostSet is a resolved trio of serving hosts. The OIDC issuer and console
// callback are derived from it so they can never disagree with the Dex host.
type HostSet struct {
	Console    string
	ConsoleAPI string
	Dex        string
}

// Issuer is the OIDC issuer URL for this host set (every JWT's iss claim).
func (h HostSet) Issuer() string { return "https://" + h.Dex + "/dex" }

// ConsoleCallback is the OAuth redirect URI back to the console.
func (h HostSet) ConsoleCallback() string { return "https://" + h.Console + "/callback" }

// LogoURL is the console-hosted Dex login logo.
func (h HostSet) LogoURL() string { return "https://" + h.Console + "/logo-stacked-light.svg" }

// Phase is the stage of a host transition, mirroring the ClusterIdentity CR
// status. The empty phase is steady state (no transition in flight).
type Phase string

const (
	PhaseSteady           Phase = ""
	PhaseDualServe        Phase = "DualServe"
	PhaseAwaitingApproval Phase = "AwaitingApproval"
	PhaseCuttingOver      Phase = "CuttingOver"
	PhaseVerifying        Phase = "Verifying"
	PhaseContracting      Phase = "Contracting"
)

// Transition carries the from/to host sets of an in-flight host change.
type Transition struct {
	Phase Phase
	From  HostSet
	To    HostSet
}

// Spec is the desired serving identity plus any in-flight transition.
type Spec struct {
	Domain    string    // the cluster's base domain (bare kipper.run label or custom domain)
	Overrides Overrides // per-service host pins
	// KipperRunDomain is the original *.kipper.run domain, retained so the
	// gateway heartbeat can address the subdomain even after a custom-domain
	// move overwrites Domain. Equal to Domain for a fresh kipper.run cluster.
	KipperRunDomain string
	ClusterHost     string // CLUSTER_HOST: the public IP the gateway routes to
	// GatewayDisabled is the CR's explicit opt-out (gateway.register: false). It
	// is the one case where the render clears the gateway env instead of leaving
	// an unknown value alone: refusing to register is a decision, and it has to
	// reach the running cluster.
	GatewayDisabled bool
	Transition      *Transition // nil in steady state
}

// Carry is live state Render cannot recompute and must preserve verbatim.
type Carry struct {
	// ExistingDexConfig is the current dex-config.yaml; Render merges its edits
	// onto it so SSO connectors, the client secret, and the admin bcrypt hash
	// survive. Empty means there is nothing to merge (base generation is the
	// installer's job until it adopts this package).
	ExistingDexConfig string
}

// Objects is the rendered serving surface.
type Objects struct {
	Ingresses     []networkingv1.Ingress // console, console-api, dex
	ConsoleAPIEnv []corev1.EnvVar        // the host-derived env family console-api owns
	DexConfigYAML string                 // merged dex-config (empty when Carry has none)
	// AdminEmail is the admin login email Dex presents in this phase (the old
	// email until the issuer flips, the new one after). The reconciler MUST
	// ensure it is authorized in the kipper-users role store, atomically with
	// applying DexConfigYAML and before Dex rolls — otherwise a base-domain move
	// locks the operator out: authenticated as the new email, but not yet
	// authorized. Re-seeding kipper-users only at contraction is too late.
	AdminEmail string
}

// ResolveHosts derives the serving host set for a domain and its overrides,
// applying the override-wins convention through the shared hostnames package.
// The reconciler uses it to project the CR's domain+overrides into a HostSet
// (for transition endpoints and the approval hash) through the same code the
// render uses, so resolution can never drift from what is served.
func ResolveHosts(domain string, o Overrides) HostSet {
	return Spec{Domain: domain, Overrides: o}.hosts()
}

func (s Spec) hosts() HostSet {
	return HostSet{
		Console:    hostnames.HostFor(consolePrefix, s.Overrides.Console, s.Domain),
		ConsoleAPI: hostnames.HostFor(consoleAPIPrefix, s.Overrides.ConsoleAPI, s.Domain),
		Dex:        hostnames.HostFor(dexPrefix, s.Overrides.Dex, s.Domain),
	}
}

// resolved is the phase-projected view Render builds every object from.
type resolved struct {
	consoleHosts    []string // ingress rule + tls hosts
	consoleAPIHosts []string
	dexHosts        []string
	active          HostSet  // drives the env family and DEX_ISSUER/DEX_REDIRECT_URI
	redirects       []string // Dex kipper-console client redirectURIs
}

// resolve projects the phase table: additive dual-serve, an issuer flip that
// only moves the active set, and a contraction to the new hosts.
func (s Spec) resolve() resolved {
	newHosts := s.hosts()
	if s.Transition == nil {
		return resolved{
			consoleHosts:    []string{newHosts.Console},
			consoleAPIHosts: []string{newHosts.ConsoleAPI},
			dexHosts:        []string{newHosts.Dex},
			active:          newHosts,
			redirects:       []string{newHosts.ConsoleCallback()},
		}
	}

	from, to := s.Transition.From, s.Transition.To
	r := resolved{
		consoleHosts:    union(from.Console, to.Console),
		consoleAPIHosts: union(from.ConsoleAPI, to.ConsoleAPI),
		dexHosts:        union(from.Dex, to.Dex),
		redirects:       union(from.ConsoleCallback(), to.ConsoleCallback()),
	}
	switch s.Transition.Phase {
	case PhaseCuttingOver, PhaseVerifying:
		// Old hosts still serve, but the identity is now the new one.
		r.active = to
	case PhaseContracting:
		// Old hosts and callback are pruned; only the new identity remains.
		r.consoleHosts = []string{to.Console}
		r.consoleAPIHosts = []string{to.ConsoleAPI}
		r.dexHosts = []string{to.Dex}
		r.redirects = []string{to.ConsoleCallback()}
		r.active = to
	default: // DualServe, AwaitingApproval: additive, identity unchanged.
		r.active = from
	}
	return r
}

// Render builds the serving objects for the Spec.
func Render(s Spec, c Carry) (Objects, error) {
	r := s.resolve()
	objs := Objects{
		Ingresses: []networkingv1.Ingress{
			consoleIngress(r.consoleHosts),
			consoleAPIIngress(r.consoleAPIHosts),
			dexIngress(r.dexHosts),
		},
		ConsoleAPIEnv: s.consoleAPIEnv(r.active),
		// Fresh installs (no carry) seed exactly this email into Dex and
		// kipper-users; with a carry the merged config is authoritative.
		AdminEmail: "admin@" + s.Domain,
	}
	if c.ExistingDexConfig != "" {
		dex, adminEmail, err := s.mergeDexConfig(c.ExistingDexConfig, r)
		if err != nil {
			return Objects{}, err
		}
		objs.DexConfigYAML = dex
		objs.AdminEmail = adminEmail
	}
	return objs, nil
}

// consoleAPIEnv is the host-derived env family console-api owns. The reconciler
// patches exactly these onto the Deployment, leaving unrelated env (SIDECAR_IMAGE
// etc.) untouched. DEX_CLIENT_SECRET is a Secret reference, not a baked value.
func (s Spec) consoleAPIEnv(active HostSet) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "DEX_ISSUER", Value: active.Issuer()},
		{Name: "DEX_CLIENT_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: dexOIDCClientSecretName},
			Key:                  dexOIDCClientSecretKey,
		}}},
		{Name: "DEX_REDIRECT_URI", Value: active.ConsoleCallback()},
		{Name: "DEX_DOMAIN", Value: active.Dex},
		{Name: "CONSOLE_DOMAIN", Value: active.Console},
		{Name: "CONSOLE_API_DOMAIN", Value: active.ConsoleAPI},
		{Name: "CLUSTER_DOMAIN", Value: s.Domain},
		{Name: "UI_DOMAIN", Value: hostnames.UIDomainFor(s.Domain)},
	}
	// The gateway pair is emitted only when the spec carries it. These two are
	// the whole switch for the kipper.run heartbeat — and with it hop pinning and
	// the registration proof — and the reconciler patches this list onto a live
	// Deployment. Emitting an empty value for one the CR cannot supply would
	// silently disable a working cluster's heartbeat, which is exactly what a
	// host transition used to do. A renderer that does not know a value leaves
	// the live one alone; a cluster that has opted out is told so explicitly.
	if s.GatewayDisabled {
		return append(env,
			corev1.EnvVar{Name: "KIPPER_RUN_DOMAIN", Value: ""},
			corev1.EnvVar{Name: "CLUSTER_HOST", Value: ""})
	}
	if s.KipperRunDomain != "" {
		env = append(env, corev1.EnvVar{Name: "KIPPER_RUN_DOMAIN", Value: s.KipperRunDomain})
	}
	if s.ClusterHost != "" {
		env = append(env, corev1.EnvVar{Name: "CLUSTER_HOST", Value: s.ClusterHost})
	}
	return env
}

// mergeDexConfig edits the live Dex config for the phase and returns the
// serialised result plus the admin login email it now carries (what the
// reconciler must authorize in kipper-users).
func (s Spec) mergeDexConfig(existing string, r resolved) (yaml string, adminEmail string, err error) {
	cfg, err := dexcfg.Load(existing)
	if err != nil {
		return "", "", err
	}
	cfg.SetIssuer(r.active.Issuer())
	if err := cfg.SetConsoleRedirectURIs(r.redirects...); err != nil {
		return "", "", err
	}
	cfg.RehostConnectors(r.active.Issuer())
	cfg.SetFrontend(r.active.LogoURL())
	// The admin login email tracks the base domain. It only changes on a
	// base-domain move (a dash migration keeps the same base label), and only
	// once the identity has flipped, so the old email keeps working during
	// dual-serve. The bcrypt hash is preserved by dexcfg.
	//
	// An entry whose email is missing or blank is repaired to the same address,
	// because the reconciler authorizes whatever this returns in kipper-users
	// and the empty string is not a login. HasAdmin is what keeps that from
	// inventing an admin on a config that has no static passwords at all.
	if wantEmail := "admin@" + s.Domain; identityIsNew(s) {
		cur, ok, aerr := cfg.AdminEmail()
		if aerr != nil {
			return "", "", aerr
		}
		if (!ok || cur != wantEmail) && cfg.HasAdmin() {
			if err := cfg.SetAdminEmail(wantEmail); err != nil {
				return "", "", err
			}
		}
	}
	email, _, err := cfg.AdminEmail()
	if err != nil {
		return "", "", err
	}
	out, err := cfg.Marshal()
	if err != nil {
		return "", "", err
	}
	return out, email, nil
}

// identityIsNew reports whether the active identity is the transition target
// (issuer already flipped) or steady state.
func identityIsNew(s Spec) bool {
	if s.Transition == nil {
		return true
	}
	switch s.Transition.Phase {
	case PhaseCuttingOver, PhaseVerifying, PhaseContracting:
		return true
	default:
		return false
	}
}

// --- ingress builders (match the installer's specs exactly) ---

// securityHeadersMiddleware is the Traefik Middleware every platform Ingress
// references, one per platform namespace. Traefik drops a router whose
// middleware is missing, so whoever applies one of these Ingresses must also
// apply SecurityHeadersMiddleware for its namespace — an Ingress carrying the
// reference alone takes the host to a 404.
const securityHeadersMiddleware = "security-headers"

// SecurityHeadersMiddleware returns the Traefik Middleware that sets HSTS and
// the clickjacking / MIME-sniffing / referrer response headers on the platform
// hosts served from namespace. It omits a Content-Security-Policy: the console
// and Dex each serve their own, and overriding it here would break them.
func SecurityHeadersMiddleware(namespace string) *unstructured.Unstructured {
	mw := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":      securityHeadersMiddleware,
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"headers": map[string]interface{}{
				"stsSeconds":              int64(31536000),
				"stsIncludeSubdomains":    true,
				"stsPreload":              true,
				"contentTypeNosniff":      true,
				"browserXssFilter":        true,
				"referrerPolicy":          "strict-origin-when-cross-origin",
				"customFrameOptionsValue": "SAMEORIGIN",
				"customResponseHeaders": map[string]interface{}{
					"Server":       "",
					"X-Powered-By": "",
				},
			},
		},
	}}
	mw.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "traefik.io",
		Version: "v1alpha1",
		Kind:    "Middleware",
	})
	return mw
}

func ingressMeta(name, namespace string, issue bool) metav1.ObjectMeta {
	annotations := map[string]string{
		"traefik.ingress.kubernetes.io/router.middlewares": fmt.Sprintf("%s-%s@kubernetescrd", namespace, securityHeadersMiddleware),
	}
	if issue {
		annotations[clusterIssuerAnnotation] = clusterIssuer
	}
	return metav1.ObjectMeta{Name: name, Namespace: namespace, Annotations: annotations}
}

func pathTypePrefix() *networkingv1.PathType {
	pt := networkingv1.PathTypePrefix
	return &pt
}

func backend(service string, port int32) networkingv1.IngressBackend {
	return networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
		Name: service,
		Port: networkingv1.ServiceBackendPort{Number: port},
	}}
}

func rulesFor(hosts []string, paths []networkingv1.HTTPIngressPath) []networkingv1.IngressRule {
	rules := make([]networkingv1.IngressRule, 0, len(hosts))
	for _, h := range hosts {
		rules = append(rules, networkingv1.IngressRule{
			Host:             h,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: paths}},
		})
	}
	return rules
}

// ingressTLS splits hosts into per-issuance TLS entries. Custom-domain hosts
// share one entry carrying the cert-manager secretName. Gateway-fronted
// *.kipper.run hosts get an entry with no secretName: Traefik still needs the
// TLS router for the SNI, but the certificate served must be the default-store
// hop certificate the gateway pins (see console-api/internal/hopcert) — a
// cert-manager cert is unissuable there anyway, since the HTTP-01 challenge
// 404s at the gateway. A transition Ingress can carry both kinds at once.
func ingressTLS(hosts []string, tlsSecret string) []networkingv1.IngressTLS {
	var custom, gateway []string
	for _, h := range hosts {
		if hostnames.IsKipperRun(h) {
			gateway = append(gateway, h)
		} else {
			custom = append(custom, h)
		}
	}
	var entries []networkingv1.IngressTLS
	if len(custom) > 0 {
		entries = append(entries, networkingv1.IngressTLS{Hosts: custom, SecretName: tlsSecret})
	}
	if len(gateway) > 0 {
		entries = append(entries, networkingv1.IngressTLS{Hosts: gateway})
	}
	return entries
}

// needsIssuer reports whether any host requires a cert-manager certificate;
// the cluster-issuer annotation is only rendered then.
func needsIssuer(hosts []string) bool {
	for _, h := range hosts {
		if !hostnames.IsKipperRun(h) {
			return true
		}
	}
	return false
}

func ingress(name, namespace, tlsSecret string, hosts []string, paths []networkingv1.HTTPIngressPath) networkingv1.Ingress {
	class := ingressClass
	return networkingv1.Ingress{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: ingressMeta(name, namespace, needsIssuer(hosts)),
		Spec: networkingv1.IngressSpec{
			IngressClassName: &class,
			TLS:              ingressTLS(hosts, tlsSecret),
			Rules:            rulesFor(hosts, paths),
		},
	}
}

func consoleIngress(hosts []string) networkingv1.Ingress {
	return ingress("console", kipperSystemNS, "console-tls", hosts, []networkingv1.HTTPIngressPath{
		{Path: "/", PathType: pathTypePrefix(), Backend: backend("console", 80)},
		{Path: "/api", PathType: pathTypePrefix(), Backend: backend("console-api", 8080)},
		{Path: "/auth", PathType: pathTypePrefix(), Backend: backend("console-api", 8080)},
	})
}

func consoleAPIIngress(hosts []string) networkingv1.Ingress {
	return ingress("console-api", kipperSystemNS, "console-api-tls", hosts, []networkingv1.HTTPIngressPath{
		{Path: "/", PathType: pathTypePrefix(), Backend: backend("console-api", 8080)},
	})
}

func dexIngress(hosts []string) networkingv1.Ingress {
	return ingress("dex", dexNS, "dex-tls", hosts, []networkingv1.HTTPIngressPath{
		{Path: "/", PathType: pathTypePrefix(), Backend: backend("dex", 5556)},
	})
}

func union(a, b string) []string {
	if a == b {
		return []string{a}
	}
	return []string{a, b}
}
