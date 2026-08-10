package installer

import (
	"context"
	"fmt"
	"time"
)

// GateResult carries what install should print and record after the inline
// login and proof. The gate NEVER writes the shared admin certificate — the
// certificate reaches the operator machine only through the explicit
// --admin-kubeconfig opt-out. A proof failure keeps the credential-free
// kubeconfig and tells the operator how to diagnose or opt in.
type GateResult struct {
	VerifiedEmail string
	Message       string
	// AuthMode labels the result for Result.AuthMode: "oidc" (verified) or
	// "deferred" (login skipped, unreachable, or proof failed — the operator
	// finishes with kip auth verify or re-installs with --admin-kubeconfig).
	AuthMode string
}

// LoginGateDeps seams the gate for tests: the login itself, the proof, and
// the issuer probe. Real install fills these with the auth package and a
// bearer client; tests substitute deterministic functions.
type LoginGateDeps struct {
	// Login performs the browser OIDC login and returns the operator's email
	// and ID token, or an error. A nil error with an empty token means the
	// operator deferred (Ctrl+C).
	Login func(ctx context.Context) (email, idToken string, deferred bool, err error)
	// Prove runs the identity proof against the API server with the token.
	Prove func(ctx context.Context, email, idToken string) (ProofResult, string)
	// RetryWindow bounds the SelfSubjectReview 401 retry; tests set it small.
	RetryWindow time.Duration
}

// RunLoginGate performs the inline login and proof for an interactive
// install. It never writes the admin certificate: a passing proof confirms
// the credential-free kubeconfig, and anything else — a declined login, an
// unreachable server, or even a genuine proof failure — keeps that
// credential-free kubeconfig and defers, telling the operator how to finish,
// diagnose, or opt into the admin certificate explicitly. The shared
// certificate reaches disk only through --admin-kubeconfig.
func RunLoginGate(ctx context.Context, deps LoginGateDeps, domain string) GateResult {
	email, idToken, deferred, err := deps.Login(ctx)
	if err != nil || deferred || idToken == "" {
		reason := "sign-in was not completed"
		if err != nil {
			reason = err.Error()
		}
		return GateResult{
			AuthMode: "deferred",
			Message:  fmt.Sprintf("This machine holds no cluster credential (%s). Finish later: kip auth login && kip auth verify", reason),
		}
	}

	result, detail := deps.Prove(ctx, email, idToken)
	switch result {
	case ProofPass:
		return GateResult{
			AuthMode:      "oidc",
			VerifiedEmail: email,
			Message:       fmt.Sprintf("kubectl authenticates as %s — the admin certificate never left the server (break-glass: ssh, then sudo k3s kubectl)", email),
		}
	case ProofPassNonAdmin:
		return GateResult{
			AuthMode:      "oidc",
			VerifiedEmail: email,
			Message:       fmt.Sprintf("kubectl authenticates as %s (%s)", email, detail),
		}
	case ProofTransportError:
		return GateResult{
			AuthMode: "deferred",
			Message:  fmt.Sprintf("Could not reach the API server to confirm sign-in (%s). Verify later: kip auth verify", detail),
		}
	default:
		// ProofAuthnRejected / ProofAuthzDeniedAsAdmin. This may be a genuine
		// authenticator problem, or simply a token that expired during the
		// proof (15-minute tokens make that edge routine). Either way, keep
		// the credential-free kubeconfig — never silently downgrade to a
		// shared certificate.
		return GateResult{
			AuthMode: "deferred",
			Message:  fmt.Sprintf("Sign-in did not verify against this cluster (%s). Your kubeconfig stays credential-free. Diagnose with: kip auth verify — or ssh to the server and run 'sudo k3s kubectl', or re-install with --admin-kubeconfig.", detail),
		}
	}
}
