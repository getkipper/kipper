package installer

import (
	"context"
	"fmt"
	"time"

	"github.com/getkipper/kipper/kip/internal/auth"
)

// DefaultLoginGate returns the production LoginGate closure for install and
// `kip auth verify`: it performs the browser OIDC login against dexHost,
// persists the session, builds a bearer client from the server+CA, and proves
// the operator's own token authenticates and authorizes. clusterID keys the
// auth store (the cluster domain). A reusable existing session skips the
// browser.
func DefaultLoginGate(clusterID, dexHost string) func(ctx context.Context, domain, server string, caData []byte) GateResult {
	return func(ctx context.Context, domain, server string, caData []byte) GateResult {
		return RunLoginGate(ctx, LoginGateDeps{
			Login: func(context.Context) (string, string, bool, error) {
				store, err := auth.Load()
				if err != nil {
					return "", "", false, err
				}
				// Reuse a valid session (a re-install by an already-logged-in
				// operator) instead of forcing another browser round trip.
				if tok, terr := store.Token(clusterID, dexHost); terr == nil {
					if c := store.Credential(clusterID); c != nil {
						return c.Email, tok, false, nil
					}
				}
				// Warm the issuer before opening a browser: a fresh install
				// reaches here seconds after Dex starts, and DNS or certificate
				// timing would otherwise throw a raw error at the operator.
				// A probe failure is a client-side condition, so it defers
				// (empty token, deferred=true) and never downgrades.
				budget := issuerProbeBudget(dexHost)
				if perr := ProbeIssuer(ctx, dexHost, budget, func(msg string) {
					fmt.Printf("  %s\n", msg)
				}); perr != nil {
					fmt.Printf("  Sign-in endpoint not ready (%v). Finish later: kip auth login\n", perr)
					return "", "", true, nil
				}
				fmt.Printf("  Sign in to finish setup (a browser will open; Ctrl+C to skip and finish later with: kip auth login)\n")
				creds, lerr := auth.Login(dexHost)
				if lerr != nil {
					return "", "", false, lerr
				}
				if merr := auth.Mutate(func(s *auth.Store) { s.Clusters[clusterID] = creds }); merr != nil {
					return "", "", false, merr
				}
				return creds.Email, creds.IDToken, false, nil
			},
			Prove: func(ctx context.Context, email, idToken string) (ProofResult, string) {
				cs, err := NewBearerClient(server, caData, idToken)
				if err != nil {
					return ProofTransportError, err.Error()
				}
				return proveOperatorIdentity(ctx, clientProveDeps(cs), email, domain, 60*time.Second)
			},
			RetryWindow: 60 * time.Second,
		}, domain)
	}
}

// VerifyOperatorIdentity is the body of `kip auth verify`: with a live session
// it builds a bearer client from server+CA and proves the token authenticates
// and authorizes. Returns the proof result and a human detail. Never writes
// any kubeconfig — it only reports.
func VerifyOperatorIdentity(ctx context.Context, clusterID, dexHost, domain, server string, caData []byte) (ProofResult, string, error) {
	store, err := auth.Load()
	if err != nil {
		return ProofTransportError, "", err
	}
	tok, err := store.Token(clusterID, dexHost)
	if err != nil {
		return ProofAuthnRejected, "", fmt.Errorf("not authenticated. Run: kip auth login")
	}
	creds := store.Credential(clusterID)
	if creds == nil {
		return ProofAuthnRejected, "", fmt.Errorf("not authenticated. Run: kip auth login")
	}
	cs, err := NewBearerClient(server, caData, tok)
	if err != nil {
		return ProofTransportError, "", err
	}
	result, detail := proveOperatorIdentity(ctx, clientProveDeps(cs), creds.Email, domain, 60*time.Second)
	return result, detail, nil
}
