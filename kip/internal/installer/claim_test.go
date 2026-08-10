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
