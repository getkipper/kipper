package gitreach

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// advertising stands in for a git host's smart-HTTP endpoint, answering by
// whatever rule the test gives it and recording what it was sent.
func advertising(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	// A test server listens on loopback, which production refuses to dial. The
	// guard has its own test below; every other test here is about what the
	// probe concludes, so it reaches the server directly.
	trustInTest(t, server)
	return server
}

// advertisingOverTLS is the same over https, which is the only scheme a token
// may travel on. The transport is swapped for the test's lifetime so the
// self-signed certificate is trusted.
func advertisingOverTLS(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	trustInTest(t, server)
	return server
}

func trustInTest(t *testing.T, server *httptest.Server) {
	t.Helper()
	original := transport
	transport = server.Client().Transport
	t.Cleanup(func() { transport = original })
}

func TestAPublicRepositoryNeedsNoToken(t *testing.T) {
	server := advertising(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/shop/checkout.git/info/refs" || r.URL.Query().Get("service") != "git-upload-pack" {
			t.Errorf("asked for %s?%s, want the reference advertisement", r.URL.Path, r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
	})

	got, detail := Check(context.Background(), server.URL+"/shop/checkout.git", "", "", "")

	if got != Reachable {
		t.Errorf("result = %v (%s), want Reachable: requiring a token here would break every public repo", got, detail)
	}
}

// The failure an operator actually hit: a private repository configured with
// no token, whose every build died at clone with nothing on the app to say so.
func TestAPrivateRepositoryWithoutATokenIsRefusedUpFront(t *testing.T) {
	server := advertising(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	got, detail := Check(context.Background(), server.URL+"/shop/checkout.git", "", "", "")

	if got != NeedsCredential {
		t.Fatalf("result = %v, want NeedsCredential", got)
	}
	if detail == "" {
		t.Error("the refusal has to say what is wrong, since it is shown instead of a build log")
	}
}

func TestAValidTokenReachesAPrivateRepository(t *testing.T) {
	server := advertisingOverTLS(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || pass != "s3cr3t" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if user == "" {
			t.Error("basic auth sent no username, which some hosts reject outright")
		}
		w.WriteHeader(http.StatusOK)
	})

	if got, detail := Check(context.Background(), server.URL+"/shop/checkout.git", "", "kipper", "s3cr3t"); got != Reachable {
		t.Errorf("result = %v (%s), want Reachable", got, detail)
	}
}

func TestARejectedTokenSaysSoRatherThanBlamingTheRepository(t *testing.T) {
	server := advertisingOverTLS(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	got, detail := Check(context.Background(), server.URL+"/shop/checkout.git", "", "kipper", "stale")

	if got != NeedsCredential {
		t.Fatalf("result = %v, want NeedsCredential", got)
	}
	if detail == "this repository is private, so it needs an access token" {
		t.Error("a token was given, so the message must not read as though none was")
	}
}

// The token travels as a basic-auth header, and Go replays those across a
// redirect. An open redirect on a git host would otherwise hand the token to
// whatever it pointed at.
func TestATokenIsNotFollowedOffItsOwnHost(t *testing.T) {
	var elsewhereSawAuth bool
	elsewhere := advertising(t, func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); ok {
			elsewhereSawAuth = true
		}
		w.WriteHeader(http.StatusOK)
	})
	origin := advertisingOverTLS(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusFound)
	})

	got, _ := Check(context.Background(), origin.URL+"/shop/checkout.git", "", "kipper", "s3cr3t")

	if elsewhereSawAuth {
		t.Fatal("the access token was carried to another host by a redirect")
	}
	// Unsafe rather than Unknown, and the difference decides whether the caller
	// blocks: a host that tries to move the credential has told us something,
	// and the build's own git would follow where this refused to.
	if got != Unsafe {
		t.Errorf("result = %v, want Unsafe: an Unknown here is waved through as a network blip", got)
	}
}

// A host this cluster cannot reach says nothing about the repository. Blocking
// a create on it would make the console refuse work whenever its own egress is
// unhappy, which is a worse failure than letting a build report the problem.
func TestAnUnreachableHostDoesNotBlockTheWrite(t *testing.T) {
	got, detail := Check(context.Background(), "https://git.invalid.example/shop/checkout.git", "", "", "")

	if got != Unknown {
		t.Errorf("result = %v (%s), want Unknown", got, detail)
	}
}

// SSH remotes are taken on trust rather than guessed at: this speaks HTTP, and
// refusing every SSH source would block a legitimate configuration.
func TestAnSSHRemoteIsUnsupportedRatherThanRefused(t *testing.T) {
	for _, remote := range []string{
		"git@git.example.com:shop/checkout.git",
		"ssh://git@git.example.com/shop/checkout.git",
	} {
		if got, _ := Check(context.Background(), remote, "", "", ""); got != Unsupported {
			t.Errorf("Check(%q) = %v, want Unsupported", remote, got)
		}
	}
}

// A server that answers something unexpected has told us nothing, and guessing
// either way would be worse than saying so.
func TestAnOddStatusIsUnknown(t *testing.T) {
	server := advertising(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	if got, _ := Check(context.Background(), server.URL+"/shop/checkout.git", "", "", ""); got != Unknown {
		t.Errorf("result = %v, want Unknown", got)
	}
}

// The host compares equal on an https-to-http redirect, so a policy that only
// pins the host lets the token onto the wire in clear. Two servers cannot share
// a port to demonstrate that end to end, so the policy itself is the thing
// under test: a same-host, scheme-downgraded hop is exactly the case a host
// comparison cannot see.
func TestTheRedirectPolicyRefusesASchemeDowngradeOnTheSameHost(t *testing.T) {
	policy := clientFor("https", "git.example.com").CheckRedirect

	err := policy(requestTo(t, "http://git.example.com/shop/checkout.git/info/refs"), nil)

	if err == nil {
		t.Fatal("a same-host https-to-http redirect was allowed, which puts the access token on the wire in clear")
	}
}

func TestTheRedirectPolicyRefusesAnotherHost(t *testing.T) {
	policy := clientFor("https", "git.example.com").CheckRedirect

	err := policy(requestTo(t, "https://elsewhere.example.com/steal"), nil)

	if err == nil {
		t.Fatal("a redirect to another host was allowed, which carries the access token off its origin")
	}
}

func TestTheRedirectPolicyAllowsTheSameOrigin(t *testing.T) {
	policy := clientFor("https", "git.example.com").CheckRedirect

	if err := policy(requestTo(t, "https://git.example.com/shop/checkout.git/info/refs/"), nil); err != nil {
		t.Errorf("a same-origin redirect was refused: %v", err)
	}
}

func TestTheRedirectPolicyStopsLooping(t *testing.T) {
	policy := clientFor("https", "git.example.com").CheckRedirect
	via := make([]*http.Request, maxRedirects)

	if err := policy(requestTo(t, "https://git.example.com/again"), via); err == nil {
		t.Error("redirects are unbounded, so a redirector or a loop never terminates")
	}
}

func requestTo(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	return req
}

// A plaintext URL with a token attached is refused outright: there is no way
// to ask the question without putting the token on the wire.
func TestAPlaintextURLWithATokenIsRefused(t *testing.T) {
	got, detail := Check(context.Background(), "http://git.example.com/shop/checkout.git", "", "kipper", "s3cr3t")

	if got != Unsupported {
		t.Errorf("result = %v (%s), want Unsupported", got, detail)
	}
}

// advertisement renders a reference advertisement the way a git host does, so
// a test asks the real question rather than a paraphrase of it.
func advertisement(refs ...string) string {
	body := "001e# service=git-upload-pack\n0000"
	for i, ref := range refs {
		line := "0000000000000000000000000000000000000000 " + ref
		if i == 0 {
			line += "\x00multi_ack thin-pack side-band"
		}
		body += fmt.Sprintf("%04x%s\n", len(line)+5, line)
	}
	return body
}

// A repository that answers is not the same as a branch that exists. A build
// configured against a deleted or mistyped branch fails every time, with the
// reason in a job log nobody is looking at.
func TestAMissingBranchIsRefused(t *testing.T) {
	server := advertising(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(advertisement("refs/heads/main", "refs/heads/develop")))
	})

	got, detail := Check(context.Background(), server.URL+"/shop/checkout.git", "release", "", "")

	if got != NeedsCredential {
		t.Fatalf("result = %v, want the branch refused", got)
	}
	if !strings.Contains(detail, "release") {
		t.Errorf("detail = %q, want it to name the branch that is missing", detail)
	}
}

func TestABranchThatExistsIsReachable(t *testing.T) {
	server := advertising(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(advertisement("refs/heads/main", "refs/heads/develop")))
	})

	if got, detail := Check(context.Background(), server.URL+"/shop/checkout.git", "develop", "", ""); got != Reachable {
		t.Errorf("result = %v (%s), want Reachable", got, detail)
	}
}

// A substring match would accept "main" for a repository that only has
// "maintenance", which is the kind of near-miss that makes a check worse than
// none: it passes exactly when the operator has made a typo.
func TestABranchIsNotMatchedByPrefix(t *testing.T) {
	server := advertising(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(advertisement("refs/heads/maintenance")))
	})

	if got, _ := Check(context.Background(), server.URL+"/shop/checkout.git", "main", "", ""); got == Reachable {
		t.Error(`"main" was accepted for a repository whose only branch is "maintenance"`)
	}
}

// Asking about the repository alone stays possible, because a create without a
// branch means the repository's own default.
func TestNoBranchAsksAboutTheRepositoryOnly(t *testing.T) {
	server := advertising(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(advertisement("refs/heads/main")))
	})

	if got, _ := Check(context.Background(), server.URL+"/shop/checkout.git", "", "", ""); got != Reachable {
		t.Errorf("result = %v, want Reachable", got)
	}
}

// A host that redirects to itself with an explicit default port is the same
// destination, and refusing it would block a perfectly ordinary git server.
func TestTheRedirectPolicyAcceptsAnExplicitDefaultPort(t *testing.T) {
	policy := clientFor("https", "git.example.com").CheckRedirect

	if err := policy(requestTo(t, "https://git.example.com:443/shop/checkout.git/info/refs"), nil); err != nil {
		t.Errorf("a same-host redirect with an explicit :443 was refused: %v", err)
	}
}

// A different port is a different destination, and the token must not follow.
func TestTheRedirectPolicyStillRefusesAnotherPort(t *testing.T) {
	policy := clientFor("https", "git.example.com").CheckRedirect

	if err := policy(requestTo(t, "https://git.example.com:8443/steal"), nil); err == nil {
		t.Error("a redirect to another port was allowed, which is another listener")
	}
}

// Callers reject a URL carrying its own credential before they get here, but
// this is a shared helper and must not depend on that: Go's transport replays
// userinfo as basic auth, so the caller that forgets is the one that leaks.
func TestAURLCarryingItsOwnCredentialIsRefused(t *testing.T) {
	for _, embedded := range []string{
		"http://deployer:supersecret@git.example.com/shop/checkout.git",
		"https://deployer:supersecret@git.example.com/shop/checkout.git",
	} {
		got, _ := Check(context.Background(), embedded, "", "", "")
		if got != Unsupported {
			t.Errorf("Check(%q) = %v, want Unsupported", embedded, got)
		}
	}
}

// A redirect that only changes capitalisation is the same destination. Git
// servers add a .git suffix by redirect and normalise case on the way, so
// refusing it blocks a repository that clones perfectly well, and does so with
// a message accusing the host.
func TestTheRedirectPolicyAcceptsADifferentlySpeltSameHost(t *testing.T) {
	policy := clientFor("https", "github.com").CheckRedirect

	for _, spelling := range []string{
		"https://GitHub.com/acme/shop.git/info/refs",
		"https://github.com./acme/shop.git/info/refs",
		"https://github.com:443/acme/shop.git/info/refs",
	} {
		if err := policy(requestTo(t, spelling), nil); err != nil {
			t.Errorf("a same-host redirect spelt %q was refused: %v", spelling, err)
		}
	}
}

// The probe runs inside console-api, which reaches the whole cluster, against a
// URL supplied by whoever configures an app. Without a dial guard a deployer
// could name a cluster service or a metadata endpoint and have the control
// plane fetch it for them, reading the outcome from whether the write was
// accepted.
//
// This test deliberately does not swap the transport: it exercises the one
// production uses.
func TestTheProbeWillNotReachAPrivateAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	got, detail := Check(context.Background(), server.URL+"/internal/service.git", "", "", "")

	if got == Reachable {
		t.Fatal("the probe reached a loopback address, so a deployer can aim console-api at cluster-internal services")
	}
	// Unknown rather than a refusal: a genuinely private git host is a real
	// configuration, and the build is not subject to this guard, so it still
	// deploys. It simply does not get the benefit of the check.
	if got != Unknown {
		t.Errorf("result = %v (%s), want Unknown", got, detail)
	}
}
