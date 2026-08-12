package serving

import (
	"sort"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
)

func ingressByName(objs Objects, name string) *networkingv1.Ingress {
	for i := range objs.Ingresses {
		if objs.Ingresses[i].Name == name {
			return &objs.Ingresses[i]
		}
	}
	return nil
}

func ruleHosts(ing *networkingv1.Ingress) []string {
	var hs []string
	for _, r := range ing.Spec.Rules {
		hs = append(hs, r.Host)
	}
	sort.Strings(hs)
	return hs
}

func tlsHosts(ing *networkingv1.Ingress) []string {
	var hs []string
	for _, t := range ing.Spec.TLS {
		hs = append(hs, t.Hosts...)
	}
	sort.Strings(hs)
	return hs
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func envValue(objs Objects, name string) string {
	for _, e := range objs.ConsoleAPIEnv {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// old = single-dash (pre-migration), new = double-dash (convention) for the
// same kipper.run cluster label.
func oldNew() (HostSet, HostSet) {
	from := HostSet{Console: "console-acme.kipper.run", ConsoleAPI: "console-api-acme.kipper.run", Dex: "dex-acme.kipper.run"}
	to := HostSet{Console: "console--acme.kipper.run", ConsoleAPI: "console-api--acme.kipper.run", Dex: "dex--acme.kipper.run"}
	return from, to
}

func specFor(phase Phase) Spec {
	from, to := oldNew()
	s := Spec{Domain: "acme.kipper.run", KipperRunDomain: "acme.kipper.run", ClusterHost: "203.0.113.7"}
	if phase != PhaseSteady {
		s.Transition = &Transition{Phase: phase, From: from, To: to}
	}
	return s
}

func TestSteadyServesNewHostsAndIdentity(t *testing.T) {
	objs, err := Render(specFor(PhaseSteady), Carry{})
	if err != nil {
		t.Fatal(err)
	}
	// Steady = the spec's conventional (double-dash) hosts only.
	if got := ruleHosts(ingressByName(objs, "console")); len(got) != 1 || got[0] != "console--acme.kipper.run" {
		t.Fatalf("console hosts = %v", got)
	}
	if got := envValue(objs, "DEX_ISSUER"); got != "https://dex--acme.kipper.run/dex" {
		t.Fatalf("DEX_ISSUER = %q", got)
	}
	if got := envValue(objs, "DEX_REDIRECT_URI"); got != "https://console--acme.kipper.run/callback" {
		t.Fatalf("DEX_REDIRECT_URI = %q", got)
	}
}

func TestDualServeIsAdditiveAndKeepsOldIdentity(t *testing.T) {
	for _, phase := range []Phase{PhaseDualServe, PhaseAwaitingApproval} {
		objs, err := Render(specFor(phase), Carry{})
		if err != nil {
			t.Fatal(err)
		}
		// All three ingresses serve BOTH hosts.
		if got := ruleHosts(ingressByName(objs, "console")); len(got) != 2 {
			t.Fatalf("%s: console must serve both hosts, got %v", phase, got)
		}
		if got := ruleHosts(ingressByName(objs, "dex")); len(got) != 2 {
			t.Fatalf("%s: dex must serve both hosts, got %v", phase, got)
		}
		// Identity is still the OLD one — nothing that invalidates sessions yet.
		if got := envValue(objs, "DEX_ISSUER"); got != "https://dex-acme.kipper.run/dex" {
			t.Fatalf("%s: DEX_ISSUER must still be old, got %q", phase, got)
		}
	}
}

func TestCuttingOverKeepsBothHostsButFlipsIdentity(t *testing.T) {
	for _, phase := range []Phase{PhaseCuttingOver, PhaseVerifying} {
		objs, err := Render(specFor(phase), Carry{})
		if err != nil {
			t.Fatal(err)
		}
		if got := ruleHosts(ingressByName(objs, "console")); len(got) != 2 {
			t.Fatalf("%s: old hosts must keep serving, got %v", phase, got)
		}
		// Identity is now the NEW one.
		if got := envValue(objs, "DEX_ISSUER"); got != "https://dex--acme.kipper.run/dex" {
			t.Fatalf("%s: DEX_ISSUER must be new, got %q", phase, got)
		}
		if got := envValue(objs, "CONSOLE_DOMAIN"); got != "console--acme.kipper.run" {
			t.Fatalf("%s: CONSOLE_DOMAIN must be new, got %q", phase, got)
		}
	}
}

func TestContractingPrunesToNewOnly(t *testing.T) {
	objs, err := Render(specFor(PhaseContracting), Carry{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ruleHosts(ingressByName(objs, "console")); len(got) != 1 || got[0] != "console--acme.kipper.run" {
		t.Fatalf("contracting must serve only new host, got %v", got)
	}
	if got := envValue(objs, "DEX_ISSUER"); got != "https://dex--acme.kipper.run/dex" {
		t.Fatalf("DEX_ISSUER = %q", got)
	}
}

// For every phase and every one of the three ingresses, the rule hosts and the
// TLS hosts must be identical and must match the phase table — cert-manager
// provisions certs from tls.hosts while Traefik routes from rules, so any
// mismatch silently drops TLS or routing for a live host.
func TestTLSHostsEqualRuleHostsAndMatchPhaseTable(t *testing.T) {
	type pair struct{ old, new string }
	ings := map[string]pair{
		"console":     {"console-acme.kipper.run", "console--acme.kipper.run"},
		"console-api": {"console-api-acme.kipper.run", "console-api--acme.kipper.run"},
		"dex":         {"dex-acme.kipper.run", "dex--acme.kipper.run"},
	}
	// The four transition phases keep both hosts; steady and contracting are new-only.
	wantBoth := map[Phase]bool{
		PhaseDualServe: true, PhaseAwaitingApproval: true,
		PhaseCuttingOver: true, PhaseVerifying: true,
		PhaseSteady: false, PhaseContracting: false,
	}
	for phase, both := range wantBoth {
		objs, err := Render(specFor(phase), Carry{})
		if err != nil {
			t.Fatal(err)
		}
		for name, p := range ings {
			ing := ingressByName(objs, name)
			if ing == nil {
				t.Fatalf("%s: ingress %q missing", phase, name)
			}
			want := []string{p.new}
			if both {
				want = []string{p.old, p.new}
			}
			sort.Strings(want)
			rh, th := ruleHosts(ing), tlsHosts(ing)
			if !equalStrings(rh, want) {
				t.Errorf("%s %s: rule hosts = %v, want %v", phase, name, rh, want)
			}
			if !equalStrings(th, want) {
				t.Errorf("%s %s: tls hosts = %v, want %v", phase, name, th, want)
			}
			if !equalStrings(rh, th) {
				t.Errorf("%s %s: tls hosts %v != rule hosts %v", phase, name, th, rh)
			}
		}
	}
}

// The console-api client secret is a Secret reference, never a baked value.
func TestClientSecretIsAReference(t *testing.T) {
	objs, _ := Render(specFor(PhaseSteady), Carry{})
	for _, e := range objs.ConsoleAPIEnv {
		if e.Name == "DEX_CLIENT_SECRET" {
			if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
				t.Fatalf("DEX_CLIENT_SECRET must be a secretKeyRef, got %+v", e)
			}
			if e.ValueFrom.SecretKeyRef.Name != "dex-oidc-client" {
				t.Fatalf("secret name = %q", e.ValueFrom.SecretKeyRef.Name)
			}
			return
		}
	}
	t.Fatal("DEX_CLIENT_SECRET env not found")
}

// Parity: the steady render matches what the installer produces today for the
// host-derived env family and the ingress specs (the client secret is the one
// intended change, so it is excluded here).
func TestSteadyMatchesInstallerSemantics(t *testing.T) {
	// Installer inputs (a bare kipper.run cluster; convention hosts).
	dexHost := "dex--acme.kipper.run"
	consoleHost := "console--acme.kipper.run"
	consoleAPIHost := "console-api--acme.kipper.run"
	bareDomain := "acme.kipper.run"
	clusterHost := "203.0.113.7"

	objs, err := Render(Spec{Domain: bareDomain, KipperRunDomain: bareDomain, ClusterHost: clusterHost}, Carry{})
	if err != nil {
		t.Fatal(err)
	}

	wantEnv := map[string]string{
		"DEX_ISSUER":         "https://" + dexHost + "/dex",
		"DEX_REDIRECT_URI":   "https://" + consoleHost + "/callback",
		"DEX_DOMAIN":         dexHost,
		"CONSOLE_DOMAIN":     consoleHost,
		"CONSOLE_API_DOMAIN": consoleAPIHost,
		"CLUSTER_DOMAIN":     bareDomain,
		"UI_DOMAIN":          "", // kipper.run -> empty
		"KIPPER_RUN_DOMAIN":  bareDomain,
		"CLUSTER_HOST":       clusterHost,
	}
	for k, want := range wantEnv {
		if got := envValue(objs, k); got != want {
			t.Errorf("env %s = %q, want %q", k, got, want)
		}
	}

	// Ingress parity: names, namespaces, TLS entries, hosts, backends, paths.
	// Gateway-fronted *.kipper.run hosts carry no secretName (the host falls
	// through to the default-store hop certificate the gateway pins) and no
	// cert-manager annotation (HTTP-01 404s at the gateway).
	console := ingressByName(objs, "console")
	if console.Namespace != "kipper-system" || console.Spec.TLS[0].SecretName != "" {
		t.Errorf("console ingress meta wrong: ns=%s tls=%s", console.Namespace, console.Spec.TLS[0].SecretName)
	}
	if console.Spec.TLS[0].Hosts[0] != consoleHost || console.Spec.Rules[0].Host != consoleHost {
		t.Errorf("console ingress host wrong")
	}
	paths := console.Spec.Rules[0].HTTP.Paths
	if len(paths) != 3 || paths[0].Path != "/" || paths[1].Path != "/api" || paths[2].Path != "/auth" {
		t.Errorf("console ingress paths wrong: %+v", paths)
	}
	if paths[0].Backend.Service.Name != "console" || paths[1].Backend.Service.Name != "console-api" {
		t.Errorf("console ingress backends wrong")
	}

	capi := ingressByName(objs, "console-api")
	if capi.Namespace != "kipper-system" || capi.Spec.TLS[0].SecretName != "" {
		t.Errorf("console-api ingress meta wrong: ns=%s tls=%s", capi.Namespace, capi.Spec.TLS[0].SecretName)
	}
	if capi.Spec.TLS[0].Hosts[0] != consoleAPIHost || capi.Spec.Rules[0].Host != consoleAPIHost {
		t.Errorf("console-api ingress host wrong")
	}
	capiPaths := capi.Spec.Rules[0].HTTP.Paths
	if len(capiPaths) != 1 || capiPaths[0].Path != "/" ||
		capiPaths[0].Backend.Service.Name != "console-api" || capiPaths[0].Backend.Service.Port.Number != 8080 {
		t.Errorf("console-api ingress path/backend wrong: %+v", capiPaths)
	}
	if *capi.Spec.IngressClassName != "traefik" || capi.Annotations["cert-manager.io/cluster-issuer"] != "" {
		t.Errorf("console-api ingress class/annotation wrong")
	}

	dex := ingressByName(objs, "dex")
	if dex.Namespace != "dex" || dex.Spec.TLS[0].SecretName != "" || dex.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Number != 5556 {
		t.Errorf("dex ingress wrong: %+v", dex.Spec)
	}
	if dex.Spec.TLS[0].Hosts[0] != dexHost || dex.Spec.Rules[0].Host != dexHost {
		t.Errorf("dex ingress host wrong")
	}
	if *dex.Spec.IngressClassName != "traefik" || dex.Annotations["cert-manager.io/cluster-issuer"] != "" {
		t.Errorf("dex ingress class/annotation wrong")
	}
	if *console.Spec.IngressClassName != "traefik" || console.Annotations["cert-manager.io/cluster-issuer"] != "" {
		t.Errorf("console ingress class/issuer annotation wrong")
	}
}

// Custom-domain hosts keep cert-manager issuance: annotation present and the
// TLS entry carries the secretName.
func TestCustomDomainIngressKeepsCertManager(t *testing.T) {
	objs, err := Render(Spec{Domain: "example.com", KipperRunDomain: "acme.kipper.run", ClusterHost: "203.0.113.7"}, Carry{})
	if err != nil {
		t.Fatal(err)
	}
	for name, secret := range map[string]string{"console": "console-tls", "console-api": "console-api-tls", "dex": "dex-tls"} {
		ing := ingressByName(objs, name)
		if ing.Annotations["cert-manager.io/cluster-issuer"] != "letsencrypt-prod" {
			t.Errorf("%s: custom domain must keep the cluster-issuer annotation", name)
		}
		if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].SecretName != secret {
			t.Errorf("%s: expected one TLS entry with secretName %s, got %+v", name, secret, ing.Spec.TLS)
		}
	}
}

// A base-domain move serves a kipper.run host and a custom host from one
// Ingress: the TLS entries split so cert-manager issues only for the custom
// host while the kipper.run host falls through to the hop certificate.
func TestTransitionIngressSplitsTLSByHostKind(t *testing.T) {
	from := HostSet{Console: "console--acme.kipper.run", ConsoleAPI: "console-api--acme.kipper.run", Dex: "dex--acme.kipper.run"}
	to := HostSet{Console: "console.example.com", ConsoleAPI: "console-api.example.com", Dex: "dex.example.com"}
	objs, err := Render(Spec{Domain: "example.com", KipperRunDomain: "acme.kipper.run", ClusterHost: "203.0.113.7",
		Transition: &Transition{Phase: PhaseDualServe, From: from, To: to}}, Carry{ExistingDexConfig: dexFixture})
	if err != nil {
		t.Fatal(err)
	}

	console := ingressByName(objs, "console")
	if console.Annotations["cert-manager.io/cluster-issuer"] != "letsencrypt-prod" {
		t.Error("a transition with a custom host still needs cert-manager")
	}
	if len(console.Spec.TLS) != 2 {
		t.Fatalf("expected the TLS entries split by host kind, got %+v", console.Spec.TLS)
	}
	var custom, gateway *networkingv1.IngressTLS
	for i := range console.Spec.TLS {
		if console.Spec.TLS[i].SecretName != "" {
			custom = &console.Spec.TLS[i]
		} else {
			gateway = &console.Spec.TLS[i]
		}
	}
	if custom == nil || custom.SecretName != "console-tls" || len(custom.Hosts) != 1 || custom.Hosts[0] != "console.example.com" {
		t.Errorf("custom TLS entry wrong: %+v", custom)
	}
	if gateway == nil || len(gateway.Hosts) != 1 || gateway.Hosts[0] != "console--acme.kipper.run" {
		t.Errorf("gateway TLS entry wrong: %+v", gateway)
	}
	// The invariant the phase table protects still holds: the union of TLS
	// hosts equals the rule hosts.
	if !equalStrings(tlsHosts(console), ruleHosts(console)) {
		t.Errorf("tls hosts %v != rule hosts %v", tlsHosts(console), ruleHosts(console))
	}
}

const dexFixture = `issuer: https://dex-acme.kipper.run/dex
frontend:
  issuer: Kipper
  logoURL: https://console-acme.kipper.run/logo-stacked-light.svg
  theme: light
connectors: []
staticClients:
- id: kipper-console
  redirectURIs:
  - https://console-acme.kipper.run/callback
  secret: keep-me
- id: kipper-cli
  public: true
  redirectURIs:
  - http://localhost:18741/callback
staticPasswords:
- email: admin@acme.kipper.run
  hash: HASH
  username: admin
`

func TestDexConfigMergeAtCutoverUnionsRedirectsAndFlipsIssuer(t *testing.T) {
	objs, err := Render(specFor(PhaseCuttingOver), Carry{ExistingDexConfig: dexFixture})
	if err != nil {
		t.Fatal(err)
	}
	out := objs.DexConfigYAML
	if !strings.Contains(out, "issuer: https://dex--acme.kipper.run/dex") {
		t.Errorf("issuer not flipped:\n%s", out)
	}
	// Both console callbacks present (dual-serve) so either host completes login.
	if !strings.Contains(out, "https://console-acme.kipper.run/callback") || !strings.Contains(out, "https://console--acme.kipper.run/callback") {
		t.Errorf("redirect URIs not unioned:\n%s", out)
	}
	// The client secret and CLI localhost callback survive.
	if !strings.Contains(out, "secret: keep-me") || !strings.Contains(out, "http://localhost:18741/callback") {
		t.Errorf("preserved fields lost:\n%s", out)
	}
}

func TestDexConfigMergeAtContractDropsOldRedirect(t *testing.T) {
	objs, err := Render(specFor(PhaseContracting), Carry{ExistingDexConfig: dexFixture})
	if err != nil {
		t.Fatal(err)
	}
	out := objs.DexConfigYAML
	if strings.Contains(out, "https://console-acme.kipper.run/callback") {
		t.Errorf("old redirect must be pruned at contract:\n%s", out)
	}
	if !strings.Contains(out, "https://console--acme.kipper.run/callback") {
		t.Errorf("new redirect missing:\n%s", out)
	}
}

// A dash migration keeps the same base label, so the admin email is untouched.
func TestDashMigrationDoesNotChangeAdminEmail(t *testing.T) {
	objs, err := Render(specFor(PhaseCuttingOver), Carry{ExistingDexConfig: dexFixture})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(objs.DexConfigYAML, "email: admin@acme.kipper.run") {
		t.Errorf("admin email should be unchanged on a dash migration:\n%s", objs.DexConfigYAML)
	}
}

// The reconciler authorizes Objects.AdminEmail in kipper-users, so a live
// config whose admin entry lost its email must come back with one rather than
// with the empty string. The value is asserted rather than the rendered line,
// because yaml.v3 keeps the quoting style the blank node had.
func TestRenderRepairsABlankAdminEmail(t *testing.T) {
	blank := strings.Replace(dexFixture, "email: admin@acme.kipper.run", `email: ""`, 1)

	objs, err := Render(specFor(PhaseCuttingOver), Carry{ExistingDexConfig: blank})
	if err != nil {
		t.Fatal(err)
	}
	if objs.AdminEmail != "admin@acme.kipper.run" {
		t.Errorf("AdminEmail = %q, want admin@acme.kipper.run", objs.AdminEmail)
	}
	if strings.Contains(objs.DexConfigYAML, `email: ""`) {
		t.Errorf("the blank admin email was left in the config:\n%s", objs.DexConfigYAML)
	}
	if !strings.Contains(objs.DexConfigYAML, "hash: HASH") {
		t.Errorf("bcrypt hash must be preserved:\n%s", objs.DexConfigYAML)
	}
}

// A config with no static passwords at all is not a damaged admin entry, and
// rendering must not fail trying to repair one that was never there.
func TestRenderAcceptsAConfigWithNoStaticPasswords(t *testing.T) {
	none := "issuer: https://dex--acme.kipper.run/dex\n" +
		"staticClients:\n- id: kipper-console\n  redirectURIs: [https://console--acme.kipper.run/callback]\n  secret: keep\n"

	objs, err := Render(specFor(PhaseCuttingOver), Carry{ExistingDexConfig: none})
	if err != nil {
		t.Fatalf("render refused a connector-only config: %v", err)
	}
	if strings.Contains(objs.DexConfigYAML, "staticPasswords") {
		t.Errorf("an admin entry was invented:\n%s", objs.DexConfigYAML)
	}
}

// A base-domain move flips the admin email once the identity is new.
func TestBaseDomainMoveRewritesAdminEmailAtCutover(t *testing.T) {
	from := HostSet{Console: "console--acme.kipper.run", ConsoleAPI: "console-api--acme.kipper.run", Dex: "dex--acme.kipper.run"}
	to := HostSet{Console: "console.example.com", ConsoleAPI: "console-api.example.com", Dex: "dex.example.com"}
	s := Spec{Domain: "example.com", KipperRunDomain: "acme.kipper.run", ClusterHost: "203.0.113.7",
		Transition: &Transition{Phase: PhaseCuttingOver, From: from, To: to}}

	objs, err := Render(s, Carry{ExistingDexConfig: dexFixture})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(objs.DexConfigYAML, "email: admin@example.com") {
		t.Errorf("admin email should move to the new base domain:\n%s", objs.DexConfigYAML)
	}
	if !strings.Contains(objs.DexConfigYAML, "hash: HASH") {
		t.Errorf("bcrypt hash must be preserved:\n%s", objs.DexConfigYAML)
	}
}

// Objects.AdminEmail must always equal the email the merged Dex config presents,
// so the reconciler can keep kipper-users in lockstep and never lock the operator
// out. A dash migration keeps the same base label, so it is constant.
func TestAdminEmailMatchesDexAcrossDashMigration(t *testing.T) {
	for _, phase := range []Phase{PhaseDualServe, PhaseCuttingOver, PhaseVerifying, PhaseContracting} {
		objs, err := Render(specFor(phase), Carry{ExistingDexConfig: dexFixture})
		if err != nil {
			t.Fatal(err)
		}
		if objs.AdminEmail != "admin@acme.kipper.run" {
			t.Errorf("%s: AdminEmail = %q, want admin@acme.kipper.run", phase, objs.AdminEmail)
		}
		if !strings.Contains(objs.DexConfigYAML, "email: "+objs.AdminEmail) {
			t.Errorf("%s: AdminEmail %q is not the Dex static-password email:\n%s", phase, objs.AdminEmail, objs.DexConfigYAML)
		}
	}
}

// On a base-domain move, AdminEmail is the OLD email until the issuer flips, then
// the NEW one — always matching what Dex presents. This is the value the
// reconciler authorizes in kipper-users before Dex rolls, closing the lockout
// window a contraction-time re-seed would leave open.
func TestAdminEmailFollowsIssuerFlipOnBaseDomainMove(t *testing.T) {
	from := HostSet{Console: "console--acme.kipper.run", ConsoleAPI: "console-api--acme.kipper.run", Dex: "dex--acme.kipper.run"}
	to := HostSet{Console: "console.example.com", ConsoleAPI: "console-api.example.com", Dex: "dex.example.com"}
	render := func(phase Phase) Objects {
		s := Spec{Domain: "example.com", KipperRunDomain: "acme.kipper.run", ClusterHost: "203.0.113.7",
			Transition: &Transition{Phase: phase, From: from, To: to}}
		objs, err := Render(s, Carry{ExistingDexConfig: dexFixture})
		if err != nil {
			t.Fatal(err)
		}
		return objs
	}

	// Before the flip the old email is still what Dex presents and what
	// kipper-users already authorizes.
	pre := render(PhaseDualServe)
	if pre.AdminEmail != "admin@acme.kipper.run" {
		t.Errorf("pre-flip AdminEmail = %q, want old", pre.AdminEmail)
	}
	if !strings.Contains(pre.DexConfigYAML, "email: admin@acme.kipper.run") {
		t.Errorf("pre-flip Dex email should still be old:\n%s", pre.DexConfigYAML)
	}

	// After the flip the new email is authoritative.
	post := render(PhaseCuttingOver)
	if post.AdminEmail != "admin@example.com" {
		t.Errorf("post-flip AdminEmail = %q, want new", post.AdminEmail)
	}
	if !strings.Contains(post.DexConfigYAML, "email: admin@example.com") {
		t.Errorf("post-flip Dex email should be new:\n%s", post.DexConfigYAML)
	}
}

// With no carry (fresh install) AdminEmail is the seeded admin for the domain.
func TestAdminEmailForFreshInstall(t *testing.T) {
	objs, err := Render(Spec{Domain: "example.com", KipperRunDomain: "acme.kipper.run", ClusterHost: "203.0.113.7"}, Carry{})
	if err != nil {
		t.Fatal(err)
	}
	if objs.AdminEmail != "admin@example.com" {
		t.Fatalf("AdminEmail = %q, want admin@example.com", objs.AdminEmail)
	}
}

// Custom-domain env: UI_DOMAIN is the real domain, not empty.
func TestCustomDomainUIScope(t *testing.T) {
	objs, err := Render(Spec{Domain: "example.com", KipperRunDomain: "acme.kipper.run", ClusterHost: "203.0.113.7"}, Carry{})
	if err != nil {
		t.Fatal(err)
	}
	if got := envValue(objs, "UI_DOMAIN"); got != "example.com" {
		t.Fatalf("UI_DOMAIN = %q, want example.com", got)
	}
	if got := envValue(objs, "CONSOLE_DOMAIN"); got != "console.example.com" {
		t.Fatalf("CONSOLE_DOMAIN = %q", got)
	}
	// KIPPER_RUN_DOMAIN retains the original kipper.run label for the heartbeat.
	if got := envValue(objs, "KIPPER_RUN_DOMAIN"); got != "acme.kipper.run" {
		t.Fatalf("KIPPER_RUN_DOMAIN = %q", got)
	}
}

// The render patches these onto a live Deployment, so a value it does not have
// must be omitted rather than emitted empty: blanking KIPPER_RUN_DOMAIN or
// CLUSTER_HOST silently stops the heartbeat, the hop pin, and the proof of
// control on a cluster that was working.
func TestGatewayEnvOmittedWhenUnknown(t *testing.T) {
	objs, err := Render(Spec{Domain: "example.com"}, Carry{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"KIPPER_RUN_DOMAIN", "CLUSTER_HOST"} {
		if envPresent(objs, name) {
			t.Errorf("%s must be omitted when the spec does not carry it, so the live value survives", name)
		}
	}

	// One known and one unknown: emit only what is known.
	partial, err := Render(Spec{Domain: "example.com", KipperRunDomain: "acme.kipper.run"}, Carry{})
	if err != nil {
		t.Fatal(err)
	}
	if !envPresent(partial, "KIPPER_RUN_DOMAIN") {
		t.Error("a known KIPPER_RUN_DOMAIN must be rendered")
	}
	if envPresent(partial, "CLUSTER_HOST") {
		t.Error("an unknown CLUSTER_HOST must stay out of the patch")
	}
}

// Opting out is the one case where the render clears the pair: refusing to
// register is a decision, and it has to reach the running cluster rather than
// being preserved as "unknown".
func TestGatewayEnvClearedWhenRegistrationRefused(t *testing.T) {
	objs, err := Render(Spec{
		Domain:          "example.com",
		KipperRunDomain: "acme.kipper.run",
		ClusterHost:     "203.0.113.7",
		GatewayDisabled: true,
	}, Carry{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"KIPPER_RUN_DOMAIN", "CLUSTER_HOST"} {
		if !envPresent(objs, name) {
			t.Errorf("%s must be emitted so the opt-out reaches the Deployment", name)
		}
		if got := envValue(objs, name); got != "" {
			t.Errorf("%s = %q, want it cleared", name, got)
		}
	}
}

// envPresent reports whether the render emitted the var at all, which is what
// decides whether the reconciler overwrites the live value.
func envPresent(objs Objects, name string) bool {
	for _, e := range objs.ConsoleAPIEnv {
		if e.Name == name {
			return true
		}
	}
	return false
}

func TestResolveHosts(t *testing.T) {
	// kipper.run: double-dash derived hosts.
	k := ResolveHosts("acme.kipper.run", Overrides{})
	if k.Console != "console--acme.kipper.run" || k.ConsoleAPI != "console-api--acme.kipper.run" || k.Dex != "dex--acme.kipper.run" {
		t.Fatalf("kipper.run resolve wrong: %+v", k)
	}
	// custom domain: dot subdomains.
	c := ResolveHosts("example.com", Overrides{})
	if c.Console != "console.example.com" || c.Dex != "dex.example.com" {
		t.Fatalf("custom resolve wrong: %+v", c)
	}
	// override wins.
	o := ResolveHosts("acme.kipper.run", Overrides{Console: "console.example.com"})
	if o.Console != "console.example.com" || o.Dex != "dex--acme.kipper.run" {
		t.Fatalf("override resolve wrong: %+v", o)
	}
}

func TestPlatformIngressesCarrySecurityHeadersMiddleware(t *testing.T) {
	// A transition re-applies the platform ingresses; each must keep the
	// security-headers middleware reference (namespace-scoped), or HSTS and the
	// frame protection vanish mid-move.
	objs, err := Render(Spec{Domain: "acme.kipper.run", KipperRunDomain: "acme.kipper.run", ClusterHost: "203.0.113.7"}, Carry{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, namespace string }{
		{"console", "kipper-system"},
		{"console-api", "kipper-system"},
		{"dex", "dex"},
	} {
		ing := ingressByName(objs, tc.name)
		if ing == nil {
			t.Fatalf("ingress %s missing", tc.name)
		}
		want := tc.namespace + "-security-headers@kubernetescrd"
		if got := ing.Annotations["traefik.ingress.kubernetes.io/router.middlewares"]; got != want {
			t.Errorf("ingress %s: middleware annotation = %q, want %q", tc.name, got, want)
		}
	}
}
