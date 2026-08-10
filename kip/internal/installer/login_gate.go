package installer

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// execCommandForHost returns the exec kubeconfig's command: "kip" when it is
// on PATH, otherwise this binary's absolute path so kubectl still finds it.
func execCommandForHost() string {
	cmd, _ := execCommandForHostPinned()
	return cmd
}

// execCommandForHostPinned returns the exec command and whether it is an
// absolute path pinned because kip is not on PATH — the caller warns when
// pinned so the operator knows to normalise with kip auth kubeconfig.
func execCommandForHostPinned() (command string, pinned bool) {
	if _, err := exec.LookPath("kip"); err == nil {
		return "kip", false
	}
	if self, err := osExecutable(); err == nil {
		return self, true
	}
	return "kip", false
}

// osExecutable is a seam for tests; production is os.Executable so the
// kubeconfig can pin this binary's real path when kip is not on PATH.
var osExecutable = os.Executable

// ProofResult classifies the outcome of proving the operator's own token
// works against the API server.
type ProofResult int

const (
	// ProofPass: the token authenticates as the expected operator and is
	// authorized as cluster-admin.
	ProofPass ProofResult = iota
	// ProofPassNonAdmin: authenticates as the operator but is not
	// cluster-admin — legitimate for a non-admin re-install, not a failure.
	ProofPassNonAdmin
	// ProofTransportError: the API server could not be reached to decide.
	ProofTransportError
	// ProofAuthnRejected: the token did not authenticate as the expected
	// prefixed identity — the authenticator is not effective.
	ProofAuthnRejected
	// ProofAuthzDeniedAsAdmin: authenticates as admin@<domain> but is denied
	// cluster-admin — the kipper-initial-admin binding is not effective.
	ProofAuthzDeniedAsAdmin
)

// NewBearerClient builds a Kubernetes client that authenticates with a raw
// bearer token against a server it verifies with caData. It never reads the
// on-disk kubeconfig, so the proof is of the token itself, not of whatever
// credential the kubeconfig might still carry.
func NewBearerClient(server string, caData []byte, token string) (kubernetes.Interface, error) {
	cfg := &rest.Config{
		Host:            server,
		BearerToken:     token,
		TLSClientConfig: rest.TLSClientConfig{CAData: caData},
		Timeout:         15 * time.Second,
	}
	return kubernetes.NewForConfig(cfg)
}

// proveDeps are the two authorization checks, seamed for tests. Both are
// creatable by system:basic-user, so no RBAC prerequisite exists beyond
// being authenticated.
type proveDeps struct {
	selfReview   func(ctx context.Context) (*authnv1.SelfSubjectReview, error)
	accessReview func(ctx context.Context) (*authzv1.SelfSubjectAccessReview, error)
}

func clientProveDeps(cs kubernetes.Interface) proveDeps {
	return proveDeps{
		selfReview: func(ctx context.Context) (*authnv1.SelfSubjectReview, error) {
			return cs.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{})
		},
		accessReview: func(ctx context.Context) (*authzv1.SelfSubjectAccessReview, error) {
			return cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authzv1.SelfSubjectAccessReview{
				Spec: authzv1.SelfSubjectAccessReviewSpec{
					ResourceAttributes: &authzv1.ResourceAttributes{Verb: "*", Group: "*", Resource: "*"},
				},
			}, metav1.CreateOptions{})
		},
	}
}

// proveOperatorIdentity runs the two reviews and classifies the result
// against the expected identity. email is the operator's logged-in email and
// domain the cluster domain; the assertion is identity-relative — the token's
// username must be exactly the prefixed logged-in email, so a spoofed or
// stale token cannot pass.
func proveOperatorIdentity(ctx context.Context, deps proveDeps, email, domain string, retryWindow time.Duration) (ProofResult, string) {
	// SelfSubjectReview: retry briefly through the lazy JWKS fetch that
	// makes the first authenticated call after activation 401.
	var review *authnv1.SelfSubjectReview
	deadline := time.Now().Add(retryWindow)
	for {
		var err error
		review, err = deps.selfReview(ctx)
		if err == nil {
			break
		}
		if !isUnauthorized(err) || time.Now().After(deadline) {
			if isUnauthorized(err) {
				return ProofAuthnRejected, "the API server rejected the token"
			}
			return ProofTransportError, err.Error()
		}
		select {
		case <-ctx.Done():
			return ProofTransportError, ctx.Err().Error()
		case <-time.After(2 * time.Second):
		}
	}

	username := review.Status.UserInfo.Username
	wantUser := oidcUsernamePrefix + email
	if username != wantUser {
		return ProofAuthnRejected, fmt.Sprintf("authenticated as %q, expected %q", username, wantUser)
	}
	// Every group must be a built-in authenticated group or oidc:-prefixed —
	// a live assertion of the prefix invariant.
	for _, g := range review.Status.UserInfo.Groups {
		if g == "system:authenticated" || strings.HasPrefix(g, oidcGroupsPrefix) {
			continue
		}
		return ProofAuthnRejected, fmt.Sprintf("token carries unprefixed group %q", g)
	}

	access, err := deps.accessReview(ctx)
	if err != nil {
		return ProofTransportError, err.Error()
	}
	if access.Status.Allowed {
		return ProofPass, username
	}
	if email == "admin@"+domain {
		return ProofAuthzDeniedAsAdmin, "the kipper-initial-admin binding is not effective"
	}
	return ProofPassNonAdmin, fmt.Sprintf("authenticated as %s, which is not cluster-admin", username)
}

func isUnauthorized(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unauthorized")
}

// ProbeIssuer waits for the Dex issuer's discovery endpoint to be reachable
// over verified TLS before the browser login, so a fresh install does not
// throw a raw DNS or certificate error in the operator's face while the
// record propagates or Let's Encrypt issues. It classifies the failure so
// the wait message is actionable, and gives up after budget with a hint. A
// nil return means the issuer is ready.
func ProbeIssuer(ctx context.Context, dexHost string, budget time.Duration, notify func(string)) error {
	url := fmt.Sprintf("https://%s/dex/.well-known/openid-configuration", dexHost)
	return probeIssuerNotify(ctx, url, &http.Client{Timeout: 8 * time.Second}, budget, notify)
}

// probeIssuerAt is the testable core with an injectable client and no notify.
func probeIssuerAt(ctx context.Context, url string, client *http.Client, budget time.Duration) error {
	if client == nil {
		client = &http.Client{Timeout: time.Second}
	}
	return probeIssuerNotify(ctx, url, client, budget, nil)
}

func probeIssuerNotify(ctx context.Context, url string, client *http.Client, budget time.Duration, notify func(string)) error {
	deadline := time.Now().Add(budget)
	var lastClass string
	for {
		// Bound each request to the remaining budget so a stalled TLS
		// handshake cannot overrun the deadline by the client's own timeout.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("dex issuer not ready after %s", budget)
		}
		reqCtx, cancel := context.WithTimeout(ctx, remaining)
		req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("issuer returned %s", resp.Status)
		}
		class := classifyProbeError(err)
		if class != lastClass && notify != nil {
			notify(class)
			lastClass = class
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dex issuer not ready after %s: %w", budget, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// issuerProbeBudget gives a custom domain longer than a *.kipper.run host,
// since a custom domain waits on the operator's own DNS plus Let's Encrypt,
// while a kipper.run subdomain resolves through the gateway immediately.
func issuerProbeBudget(dexHost string) time.Duration {
	if strings.HasSuffix(dexHost, ".kipper.run") {
		return 180 * time.Second
	}
	return 300 * time.Second
}

// classifyProbeError turns a probe error into an operator-facing waiting
// message naming the likely cause.
func classifyProbeError(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no such host") || strings.Contains(msg, "server misbehaving"):
		return "waiting for DNS to point at the server (this can take a few minutes)"
	case strings.Contains(msg, "certificate") || strings.Contains(msg, "tls"):
		return "waiting for the TLS certificate (Let's Encrypt usually completes within a minute or two)"
	default:
		return "waiting for the identity provider to accept connections"
	}
}
