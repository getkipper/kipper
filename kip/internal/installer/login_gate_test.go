package installer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
)

func httptestNewTLSDiscovery(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"issuer":"x"}`))
	}))
}

func fakeDeps(user string, groups []string, authErr error, allowed bool, accessErr error) proveDeps {
	return proveDeps{
		selfReview: func(context.Context) (*authnv1.SelfSubjectReview, error) {
			if authErr != nil {
				return nil, authErr
			}
			return &authnv1.SelfSubjectReview{Status: authnv1.SelfSubjectReviewStatus{
				UserInfo: authnv1.UserInfo{Username: user, Groups: groups},
			}}, nil
		},
		accessReview: func(context.Context) (*authzv1.SelfSubjectAccessReview, error) {
			if accessErr != nil {
				return nil, accessErr
			}
			return &authzv1.SelfSubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: allowed}}, nil
		},
	}
}

func TestProveOperatorIdentity(t *testing.T) {
	const domain = "cluster.example.com"
	tests := []struct {
		name  string
		email string
		deps  proveDeps
		want  ProofResult
	}{
		{
			name:  "admin authenticates and is authorized",
			email: "admin@cluster.example.com",
			deps:  fakeDeps("oidc:admin@cluster.example.com", []string{"system:authenticated", "oidc:kipper-admins"}, nil, true, nil),
			want:  ProofPass,
		},
		{
			name:  "non-admin operator passes with a note",
			email: "dev@cluster.example.com",
			deps:  fakeDeps("oidc:dev@cluster.example.com", []string{"system:authenticated"}, nil, false, nil),
			want:  ProofPassNonAdmin,
		},
		{
			name:  "admin denied cluster-admin is a broken binding",
			email: "admin@cluster.example.com",
			deps:  fakeDeps("oidc:admin@cluster.example.com", []string{"system:authenticated"}, nil, false, nil),
			want:  ProofAuthzDeniedAsAdmin,
		},
		{
			name:  "username not matching the logged-in identity is rejected",
			email: "admin@cluster.example.com",
			deps:  fakeDeps("oidc:someone-else@cluster.example.com", []string{"system:authenticated"}, nil, true, nil),
			want:  ProofAuthnRejected,
		},
		{
			name:  "unprefixed username is rejected",
			email: "admin@cluster.example.com",
			deps:  fakeDeps("admin@cluster.example.com", []string{"system:authenticated"}, nil, true, nil),
			want:  ProofAuthnRejected,
		},
		{
			name:  "unprefixed group is rejected",
			email: "admin@cluster.example.com",
			deps:  fakeDeps("oidc:admin@cluster.example.com", []string{"system:authenticated", "system:masters"}, nil, true, nil),
			want:  ProofAuthnRejected,
		},
		{
			name:  "transport error on the access review",
			email: "admin@cluster.example.com",
			deps:  fakeDeps("oidc:admin@cluster.example.com", []string{"system:authenticated"}, nil, false, errors.New("connection refused")),
			want:  ProofTransportError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := proveOperatorIdentity(context.Background(), tt.deps, tt.email, domain, 60*time.Second)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestProveOperatorIdentityRetriesThroughLazyJWKS(t *testing.T) {
	// The first authenticated call after activation can 401 while the
	// apiserver lazily fetches JWKS; a brief retry must not read that as a
	// rejection.
	calls := 0
	deps := proveDeps{
		selfReview: func(context.Context) (*authnv1.SelfSubjectReview, error) {
			calls++
			if calls < 2 {
				return nil, errors.New("Unauthorized")
			}
			return &authnv1.SelfSubjectReview{Status: authnv1.SelfSubjectReviewStatus{
				UserInfo: authnv1.UserInfo{Username: "oidc:admin@cluster.example.com", Groups: []string{"system:authenticated"}},
			}}, nil
		},
		accessReview: func(context.Context) (*authzv1.SelfSubjectAccessReview, error) {
			return &authzv1.SelfSubjectAccessReview{Status: authzv1.SubjectAccessReviewStatus{Allowed: true}}, nil
		},
	}
	got, _ := proveOperatorIdentity(context.Background(), deps, "admin@cluster.example.com", "cluster.example.com", 60*time.Second)
	assert.Equal(t, ProofPass, got)
	assert.GreaterOrEqual(t, calls, 2)
}

func TestProvePersistentUnauthorizedIsRejected(t *testing.T) {
	// A persistent 401 (never a transport error) means the token is genuinely
	// not accepted — reject, do not treat as transport.
	deps := fakeDeps("", nil, errors.New("Unauthorized"), false, nil)
	// retryWindow 0: the first 401 is already past the deadline, so it is
	// classified as a rejection with no wait.
	got, _ := proveOperatorIdentity(context.Background(), deps, "admin@cluster.example.com", "cluster.example.com", 0)
	assert.Equal(t, ProofAuthnRejected, got)
}

func TestClassifyProbeError(t *testing.T) {
	assert.Contains(t, classifyProbeError(errors.New("dial tcp: lookup dex.x: no such host")), "DNS")
	assert.Contains(t, classifyProbeError(errors.New("x509: certificate signed by unknown authority")), "TLS certificate")
	assert.Contains(t, classifyProbeError(errors.New("connection refused")), "accept connections")
}

func TestProbeIssuerReturnsWhenReady(t *testing.T) {
	srv := httptestNewTLSDiscovery(t)
	defer srv.Close()
	// srv.URL is https://127.0.0.1:port; ProbeIssuer builds
	// https://<host>/dex/.well-known/... so point it at the test server's
	// host:port and accept its cert via the default client override.
	err := probeIssuerAt(context.Background(), srv.URL+"/dex/.well-known/openid-configuration", srv.Client(), time.Second)
	assert.NoError(t, err)
}

func TestProbeIssuerGivesUpAfterBudget(t *testing.T) {
	// An unreachable endpoint must return an error within the budget, not hang.
	err := probeIssuerAt(context.Background(), "https://127.0.0.1:1/dex/.well-known/openid-configuration", nil, 200*time.Millisecond)
	assert.Error(t, err)
}

type blockingRT struct{ released chan struct{} }

func (b blockingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case <-req.Context().Done():
		return nil, req.Context().Err()
	case <-b.released:
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	}
}

func TestProbeIssuerHonoursBudgetUnderBlockingTransport(t *testing.T) {
	// A transport that never answers must not make the probe overrun its
	// budget: each request is bounded by the remaining deadline.
	rt := blockingRT{released: make(chan struct{})}
	client := &http.Client{Transport: rt}
	start := time.Now()
	err := probeIssuerAt(context.Background(), "https://dex.example.com/x", client, 300*time.Millisecond)
	elapsed := time.Since(start)
	assert.Error(t, err)
	assert.Less(t, elapsed, 2*time.Second, "the probe must not overrun its budget on a stalled request")
}

func TestIssuerProbeBudget(t *testing.T) {
	assert.Equal(t, 180*time.Second, issuerProbeBudget("dex--203-0-113-10.kipper.run"))
	assert.Equal(t, 300*time.Second, issuerProbeBudget("dex.custom.example.com"))
}
