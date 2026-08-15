package installer

import (
	"errors"
	"strings"
	"testing"

	"github.com/getkipper/kipper/kip/internal/domain"
)

type fakeRegistrar struct {
	reg *domain.Registration
	err error
	// accepts is the token this gateway recognises; anything else draws the
	// tokenless, challengeless answer it gives a stranger.
	accepts    string
	sawToken   string
	released   string
	releaseErr error
}

func (f *fakeRegistrar) Deregister(token string) error {
	f.released = token
	return f.releaseErr
}

func (f *fakeRegistrar) Register(_, _, token string) (*domain.Registration, error) {
	f.sawToken = token
	if f.reg != nil && f.reg.Token == "" && token != "" && token == f.accepts {
		// A renewal the gateway authorised: tokenless, but challenged.
		withChallenge := *f.reg
		withChallenge.Challenge = "nonce"
		return &withChallenge, f.err
	}
	return f.reg, f.err
}

type fakeStore struct {
	known     string
	saved     string
	saveErr   error
	savedFor  string
	savedName string
}

func (s *fakeStore) tokenFor(string) string { return s.known }
func (s *fakeStore) save(host, clusterName, token string) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	s.savedFor, s.savedName, s.saved = host, clusterName, token
	return nil
}

// The ordinary first install: the gateway creates the name and hands over its
// one-time token, which must be recorded before anything else can fail.
func TestClaimRecordsTheTokenTheGatewayDisclosesOnce(t *testing.T) {
	gw := &fakeRegistrar{reg: &domain.Registration{Domain: "203-0-113-10.kipper.run", Token: "tok-new"}}
	store := &fakeStore{}

	claim, err := claimGatewayName(gw, store, "203-0-113-10", "203.0.113.10")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claim.Token != "tok-new" || claim.Domain != "203-0-113-10.kipper.run" {
		t.Errorf("claim = %+v, want the created registration", claim)
	}
	if store.saved != "tok-new" {
		t.Error("the token must be recorded before the install can fail and lose it")
	}
	// The name the store is handed becomes the cluster's permanent name: the
	// checkpoint reloads this entry by host and inherits it. Passing the host
	// here left every fresh install called after its own IP, and no test saw it
	// because the adapter's own test supplies the right value directly.
	if store.savedName != "203-0-113-10.kipper.run" {
		t.Errorf("saved under name %q, want the claimed domain", store.savedName)
	}
	if store.savedFor != "203.0.113.10" {
		t.Errorf("saved against host %q, want the host being installed", store.savedFor)
	}
}

// A name already claimed, with nothing to prove ownership: the gateway answers
// without a token and such a cluster could never be routed to.
func TestClaimRefusesANameItCannotProveItOwns(t *testing.T) {
	gw := &fakeRegistrar{reg: &domain.Registration{Domain: "203-0-113-10.kipper.run"}}

	_, err := claimGatewayName(gw, &fakeStore{}, "203-0-113-10", "203.0.113.10")
	if err == nil {
		t.Fatal("a registration without a token must fail the install, not build an unreachable cluster")
	}
	if !strings.Contains(err.Error(), "--domain") {
		t.Errorf("the error must offer a way forward, got %v", err)
	}
}

// A retry after a failed attempt holds the token already. Presenting it renews
// the registration rather than arriving anonymously against its own name — which
// is what turned one failed install into a 30-day lockout.
func TestClaimPresentsATokenItAlreadyHolds(t *testing.T) {
	gw := &fakeRegistrar{reg: &domain.Registration{Domain: "203-0-113-10.kipper.run"}, accepts: "tok-held"}
	store := &fakeStore{known: "tok-held"}

	claim, err := claimGatewayName(gw, store, "203-0-113-10", "203.0.113.10")
	if err != nil {
		t.Fatalf("a retry holding the token must succeed: %v", err)
	}
	if gw.sawToken != "tok-held" {
		t.Errorf("the known token must be presented, got %q", gw.sawToken)
	}
	if claim.Token != "tok-held" {
		t.Errorf("a renewal discloses no new token, so the held one stands; got %q", claim.Token)
	}
}

func TestClaimSurfacesAGatewayFailure(t *testing.T) {
	gw := &fakeRegistrar{err: errors.New("gateway: 503")}

	if _, err := claimGatewayName(gw, &fakeStore{}, "203-0-113-10", "203.0.113.10"); err == nil {
		t.Fatal("a gateway that cannot be reached must fail the install")
	}
}

// Recording the token is the thing that makes a failed install recoverable, so a
// store that cannot hold it must not pass silently.
func TestClaimReportsWhenTheTokenCannotBeRecorded(t *testing.T) {
	gw := &fakeRegistrar{reg: &domain.Registration{Domain: "203-0-113-10.kipper.run", Token: "tok-new"}}
	store := &fakeStore{saveErr: errors.New("read-only file system")}

	if _, err := claimGatewayName(gw, store, "203-0-113-10", "203.0.113.10"); err == nil {
		t.Fatal("a token that could not be recorded must be reported, not assumed durable")
	}
}

// The gateway discloses a fresh token once. If nothing durable can hold it,
// returning would leave the name claimed by a registration nobody can prove
// ownership of — a 30-day lockout produced by a local disk condition. It is
// handed back while still in hand.
func TestClaimReleasesAFreshNameItCannotRecord(t *testing.T) {
	gw := &fakeRegistrar{reg: &domain.Registration{Domain: "203-0-113-10.kipper.run", Token: "tok-new"}}
	store := &fakeStore{saveErr: errors.New("read-only file system")}

	_, err := claimGatewayName(gw, store, "203-0-113-10", "203.0.113.10")
	if err == nil {
		t.Fatal("an unrecordable claim must fail")
	}
	if gw.released != "tok-new" {
		t.Error("a name that cannot be recorded must be handed back, not stranded")
	}
	if !strings.Contains(err.Error(), "re-running is safe") {
		t.Errorf("the operator must be told the retry is clean, got %v", err)
	}
}

// Renewing someone's existing name confers no right to end it: the registration
// predates this attempt and a half-built cluster may still be serving on it.
func TestClaimNeverReleasesANameItOnlyRenewed(t *testing.T) {
	gw := &fakeRegistrar{reg: &domain.Registration{Domain: "203-0-113-10.kipper.run"}, accepts: "tok-held"}
	store := &fakeStore{known: "tok-held", saveErr: errors.New("read-only file system")}

	claimed, err := claimGatewayName(gw, store, "203-0-113-10", "203.0.113.10")
	if err != nil {
		t.Fatalf("a renewal holds a token that is already durable: %v", err)
	}
	if claimed.Created {
		t.Error("a renewal did not create the registration")
	}
	if gw.released != "" {
		t.Error("a renewed name must never be released")
	}
}

// Only a name this run created may be rolled back, so the caller must be able to
// tell the difference.
func TestClaimReportsWhetherItCreatedTheRegistration(t *testing.T) {
	fresh := &fakeRegistrar{reg: &domain.Registration{Domain: "d", Token: "tok-new"}}
	claimed, err := claimGatewayName(fresh, &fakeStore{}, "s", "203.0.113.10")
	if err != nil || !claimed.Created {
		t.Errorf("a disclosed token means the gateway created it: %+v %v", claimed, err)
	}

	renewed := &fakeRegistrar{reg: &domain.Registration{Domain: "d"}, accepts: "tok-held"}
	claimed2, err2 := claimGatewayName(renewed, &fakeStore{known: "tok-held"}, "s", "203.0.113.10")
	if err2 != nil || claimed2.Created {
		t.Errorf("no new token means a renewal: %+v %v", claimed2, err2)
	}
}

// Holding a token is not the gateway accepting it. A credential the gateway no
// longer honours for this name — swept and re-registered, or belonging to
// another cluster behind the same address — draws the identical tokenless 201 a
// renewal does. Only the challenge separates them, and without it the install
// would record a dead credential and build a cluster that can never renew,
// prove or release its own name.
func TestClaimRefusesAStaleTokenTheGatewayDidNotAccept(t *testing.T) {
	// The fake accepts nothing, so it answers tokenless and challengeless.
	gw := &fakeRegistrar{reg: &domain.Registration{Domain: "203-0-113-10.kipper.run"}}
	store := &fakeStore{known: "tok-no-longer-valid"}

	_, err := claimGatewayName(gw, store, "203-0-113-10", "203.0.113.10")
	if err == nil {
		t.Fatal("a token the gateway did not accept must not pass as ownership")
	}
	if gw.sawToken != "tok-no-longer-valid" {
		t.Errorf("the held token must still be presented, sent %q", gw.sawToken)
	}
}

// --- choosing the *.kipper.run name ---

func TestKipperRunLabelForDefaultsToTheServerAddress(t *testing.T) {
	label, wanted, err := KipperRunLabelFor("", "203.0.113.10")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !wanted {
		t.Error("an install with no --domain must claim a gateway name")
	}
	if label != "203-0-113-10" {
		t.Errorf("label = %q, want the address-derived name", label)
	}
}

func TestKipperRunLabelForTakesTheChosenLabel(t *testing.T) {
	label, wanted, err := KipperRunLabelFor("lab.kipper.run", "203.0.113.10")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !wanted {
		t.Error("a *.kipper.run --domain must claim a gateway name")
	}
	if label != "lab" {
		t.Errorf("label = %q, want lab", label)
	}
}

// A custom domain's DNS belongs to the operator, so nothing is claimed from the
// gateway and no label rule applies to it.
func TestKipperRunLabelForIgnoresACustomDomain(t *testing.T) {
	label, wanted, err := KipperRunLabelFor("kipper.example.com", "203.0.113.10")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wanted {
		t.Error("a custom domain must claim no gateway name")
	}
	if label != "" {
		t.Errorf("label = %q, want empty", label)
	}
}

// Every one of these is refused by the gateway. Catching them here is what keeps
// the failure at flag-parse time rather than after an SSH connection.
func TestKipperRunLabelForRefusesUnregistrableNames(t *testing.T) {
	cases := map[string]string{
		"console.kipper.run":      "reserved",
		"login.kipper.run":        "reserved after the sweep",
		"under_score.kipper.run":  "underscore is not a DNS label character",
		"-lead.kipper.run":        "leading hyphen",
		"a--b.kipper.run":         "carries the derived-route separator",
		"deep.nested.kipper.run":  "more than one label",
		"kipper.run":              "the apex itself",
		"198-51-100-7.kipper.run": "another server's derived name",
	}
	for domain, why := range cases {
		if _, _, err := KipperRunLabelFor(domain, "203.0.113.10"); err == nil {
			t.Errorf("KipperRunLabelFor(%q) = nil error, want it refused (%s)", domain, why)
		}
	}
}

// A server asking for its own derived name is the default path spelled out, and
// must not be caught by the guard that protects it.
func TestKipperRunLabelForAllowsAServerItsOwnDerivedName(t *testing.T) {
	label, _, err := KipperRunLabelFor("203-0-113-10.kipper.run", "203.0.113.10")
	if err != nil {
		t.Fatalf("a server naming its own derived label must be accepted: %v", err)
	}
	if label != "203-0-113-10" {
		t.Errorf("label = %q, want 203-0-113-10", label)
	}
}

// A chosen label has to reach the ClusterIdentity's gateway block, not just the
// serving domain. Without the block console-api gets no KIPPER_RUN_DOMAIN, so it
// never heartbeats: the registration ages out, the hop pin is never asserted,
// and the cluster serves a name the gateway will not route to.
func TestChosenLabelReachesTheGatewayBlock(t *testing.T) {
	got := gatewayRegistrationFor(nil, "lab.kipper.run")
	if got != "lab.kipper.run" {
		t.Errorf("gatewayRegistrationFor = %q, want lab.kipper.run", got)
	}

	manifest := ClusterIdentityManifest("lab.kipper.run", "", "", "", got, "203.0.113.10")
	for _, want := range []string{
		"kipperRunDomain: lab.kipper.run",
		"clusterHost: 203.0.113.10",
		"register: true",
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest is missing %q:\n%s", want, manifest)
		}
	}
}

func TestDerivedLabelReachesTheGatewayBlock(t *testing.T) {
	if got := gatewayRegistrationFor(nil, "203-0-113-10.kipper.run"); got != "203-0-113-10.kipper.run" {
		t.Errorf("gatewayRegistrationFor = %q, want the derived name", got)
	}
}

// A custom domain's DNS is the operator's, so the cluster registers nothing and
// the manifest carries no gateway block at all.
func TestCustomDomainRecordsNoGatewayRegistration(t *testing.T) {
	got := gatewayRegistrationFor(nil, "kipper.example.com")
	if got != "" {
		t.Errorf("gatewayRegistrationFor = %q, want empty", got)
	}
	if manifest := ClusterIdentityManifest("kipper.example.com", "", "", "", got, "203.0.113.10"); strings.Contains(manifest, "gateway:") {
		t.Errorf("a custom-domain install must write no gateway block:\n%s", manifest)
	}
}

// After a custom-domain move the cluster serves example.com while its
// registration is still the kipper.run name the heartbeat renews. A reinstall
// must keep that, rather than deriving one from the domain it now serves.
func TestAdoptedIdentityKeepsItsRecordedRegistration(t *testing.T) {
	existing := &ExistingIdentity{Domain: "kipper.example.com", KipperRunDomain: "lab.kipper.run"}
	if got := gatewayRegistrationFor(existing, "kipper.example.com"); got != "lab.kipper.run" {
		t.Errorf("gatewayRegistrationFor = %q, want the recorded lab.kipper.run", got)
	}
}

// DNS names are case-insensitive and may carry a trailing dot, so both spellings
// name the same host. Classifying on the raw string sent LAB.KIPPER.RUN down the
// custom-domain path: nothing was registered with the gateway, and cert-manager
// then tried to issue for a host the gateway would never route an ACME challenge
// to.
func TestKipperRunLabelForNormalisesTheDomain(t *testing.T) {
	for _, domain := range []string{
		"LAB.KIPPER.RUN",
		"Lab.Kipper.Run",
		"lab.kipper.run.",
		"  lab.kipper.run  ",
	} {
		label, wanted, err := KipperRunLabelFor(domain, "203.0.113.10")
		if err != nil {
			t.Errorf("KipperRunLabelFor(%q): unexpected error %v", domain, err)
			continue
		}
		if !wanted {
			t.Errorf("KipperRunLabelFor(%q) was read as a custom domain", domain)
		}
		if label != "lab" {
			t.Errorf("KipperRunLabelFor(%q) = %q, want lab", domain, label)
		}
	}

	// The apex is the apex however it is spelled.
	if _, _, err := KipperRunLabelFor("KIPPER.RUN.", "203.0.113.10"); err == nil {
		t.Error("the apex must be refused whatever its case")
	}
}

func TestNormaliseDomain(t *testing.T) {
	cases := map[string]string{
		"LAB.KIPPER.RUN":  "lab.kipper.run",
		"Example.COM.":    "example.com",
		"  example.com  ": "example.com",
		"example.com":     "example.com",
		"":                "",
	}
	for in, want := range cases {
		if got := NormaliseDomain(in); got != want {
			t.Errorf("NormaliseDomain(%q) = %q, want %q", in, got, want)
		}
	}
}
