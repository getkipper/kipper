package installer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRunLoginGateNeverDowngradesOnClientFailure(t *testing.T) {
	// The security invariant: no client-side failure writes the admin cert.
	cases := []struct {
		name  string
		login func(context.Context) (string, string, bool, error)
	}{
		{"login errors (network/browser)", func(context.Context) (string, string, bool, error) {
			return "", "", false, errors.New("browser did not open")
		}},
		{"operator deferred with Ctrl+C", func(context.Context) (string, string, bool, error) {
			return "", "", true, nil
		}},
		{"empty token", func(context.Context) (string, string, bool, error) {
			return "admin@c.example.com", "", false, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := RunLoginGate(context.Background(), LoginGateDeps{
				Login: tc.login,
				Prove: func(context.Context, string, string) (ProofResult, string) {
					t.Fatal("prove must not run when login did not complete")
					return ProofPass, ""
				},
				RetryWindow: time.Second,
			}, "c.example.com")
			assert.Equal(t, "deferred", res.AuthMode, "client-side failure must keep the credential-free kubeconfig")
			assert.Equal(t, "deferred", res.AuthMode)
		})
	}
}

func TestRunLoginGateTransportErrorNeverDowngrades(t *testing.T) {
	// An unreachable API server is not proof of failure; keep the exec file.
	res := RunLoginGate(context.Background(), LoginGateDeps{
		Login: func(context.Context) (string, string, bool, error) { return "admin@c.example.com", "tok", false, nil },
		Prove: func(context.Context, string, string) (ProofResult, string) {
			return ProofTransportError, "connection refused"
		},
	}, "c.example.com")
	assert.Equal(t, "deferred", res.AuthMode)
}

func TestRunLoginGatePassKeepsExec(t *testing.T) {
	res := RunLoginGate(context.Background(), LoginGateDeps{
		Login: func(context.Context) (string, string, bool, error) { return "admin@c.example.com", "tok", false, nil },
		Prove: func(context.Context, string, string) (ProofResult, string) {
			return ProofPass, "oidc:admin@c.example.com"
		},
	}, "c.example.com")
	assert.Equal(t, "oidc", res.AuthMode)
	assert.Equal(t, "admin@c.example.com", res.VerifiedEmail)
}

func TestRunLoginGateProofFailureNeverDowngrades(t *testing.T) {
	// A genuine proof failure (or a token that expired mid-proof) keeps the
	// credential-free kubeconfig and defers — the admin certificate is never
	// written by the gate.
	for _, pr := range []ProofResult{ProofAuthnRejected, ProofAuthzDeniedAsAdmin} {
		res := RunLoginGate(context.Background(), LoginGateDeps{
			Login: func(context.Context) (string, string, bool, error) { return "admin@c.example.com", "tok", false, nil },
			Prove: func(context.Context, string, string) (ProofResult, string) { return pr, "detail" },
		}, "c.example.com")
		assert.Equal(t, "deferred", res.AuthMode)
		assert.NotContains(t, res.Message, "SHARED admin certificate", "the gate must not announce a downgrade it does not perform")
	}
}

func TestResolveKubeconfigMode(t *testing.T) {
	tests := []struct {
		admin, noLogin, tty bool
		want                KubeconfigMode
	}{
		{false, false, true, KubeconfigExecInteractive},
		{false, false, false, KubeconfigExecDeferred},
		{false, true, true, KubeconfigExecDeferred},
		{false, true, false, KubeconfigExecDeferred},
		{true, false, true, KubeconfigAdminCert},
		{true, true, true, KubeconfigAdminCert},
		{true, false, false, KubeconfigAdminCert},
		{true, true, false, KubeconfigAdminCert},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, ResolveKubeconfigMode(tt.admin, tt.noLogin, tt.tty),
			"admin=%v noLogin=%v tty=%v", tt.admin, tt.noLogin, tt.tty)
	}
}
