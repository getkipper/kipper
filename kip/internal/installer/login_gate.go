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

// execCommandForHost returns the command kubectl uses to invoke kip.
func execCommandForHost() string {
	cmd, _ := execCommandForHostPinned()
	return cmd
}

// execCommandForHostPinned prefers kip on PATH, then this executable's path.
// The pinned result lets callers warn that the kubeconfig depends on that location.
func execCommandForHostPinned() (command string, pinned bool) {
	if _, err := exec.LookPath("kip"); err == nil {
		return "kip", false
	}
	if self, err := osExecutable(); err == nil {
		return self, true
	}
	return "kip", false
}

// PinnedKipWarning tells the operator their kubeconfig pins this binary's path.
const PinnedKipWarning = "⚠  kip is not on PATH; the kubeconfig pins this binary's location. Add kip to PATH and run: kip auth kubeconfig" //nolint:gosec // a notice, not a credential

// KipPinned reports whether a kubeconfig written now pins this binary's
// absolute path, because kip is not on PATH.
func KipPinned() bool {
	_, pinned := execCommandForHostPinned()
	return pinned
}

// osExecutable lets tests override the executable path.
var osExecutable = os.Executable

// ProofResult classifies the outcome of proving the operator's own token
// works against the API server.
type ProofResult int

const (
	ProofPass ProofResult = iota
	ProofPassNonAdmin
	ProofTransportError
	ProofAuthnRejected
	ProofAuthzDeniedAsAdmin
)

// NewBearerClient authenticates with token and verifies the server using caData.
// Explicit credentials ensure the identity check uses this token independently of kubeconfig.
func NewBearerClient(server string, caData []byte, token string) (kubernetes.Interface, error) {
	cfg := &rest.Config{
		Host:            server,
		BearerToken:     token,
		TLSClientConfig: rest.TLSClientConfig{CAData: caData},
		Timeout:         15 * time.Second,
	}
	return kubernetes.NewForConfig(cfg)
}

// proveDeps allows tests to supply identity and access review results.
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

// proveOperatorIdentity checks the authenticated identity against the logged-in
// email and checks wildcard resource access. Denied access is a failure for
// admin@<domain>; other users may authenticate with limited access.
func proveOperatorIdentity(ctx context.Context, deps proveDeps, email, domain string, retryWindow time.Duration) (ProofResult, string) {
	// Allow time for authentication to become ready after activation.
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
	// Require OIDC group namespacing, except for system:authenticated.
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

// ProbeIssuer waits for HTTP 200 responses from Dex discovery and login
// stylesheets over verified TLS before browser login. The probes share a time
// budget; notify, if provided, receives messages describing likely failure causes.
func ProbeIssuer(ctx context.Context, dexHost string, budget time.Duration, notify func(string)) error {
	return probeLoginPageAt(ctx, "https://"+dexHost, &http.Client{Timeout: 8 * time.Second}, budget, notify)
}

// loginPagePaths includes stylesheets so readiness covers login styling as well as discovery.
var loginPagePaths = []string{
	"/dex/.well-known/openid-configuration",
	"/dex/static/main.css",
	"/dex/theme/styles.css",
}

func probeLoginPageAt(ctx context.Context, base string, client *http.Client, budget time.Duration, notify func(string)) error {
	deadline := time.Now().Add(budget)
	for _, path := range loginPagePaths {
		if err := probeIssuerNotify(ctx, base+path, client, time.Until(deadline), notify); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

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
		// Cap requests at the remaining budget, even if the client timeout is longer.
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

// IssuerProbeBudget returns the Dex readiness timeout, allowing extra time
// for DNS and certificate setup on custom domains.
func IssuerProbeBudget(dexHost string) time.Duration {
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
