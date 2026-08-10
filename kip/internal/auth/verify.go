package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// verifiedToken carries what kip consumes from a cryptographically verified
// ID token. ExpiresAt comes from the token's own `exp` claim: the OAuth
// response's expires_in describes the access token and can diverge from the
// ID token's lifetime, and the API server will judge the token by its `exp`.
type verifiedToken struct {
	Email     string
	ExpiresAt time.Time
}

// verifyIDToken performs conforming OIDC verification of a raw ID token:
// discovery from the issuer, JWKS fetch, signature verification against the
// issuer's keys with the accepted algorithms pinned, and issuer, audience,
// and expiry validation. TLS on the token endpoint authenticates the
// connection, not the token — only the signature makes the token itself
// trustworthy as an artefact. expectedNonce binds a code-exchange token to
// its login attempt; refresh responses carry no fresh nonce and pass "".
func verifyIDToken(ctx context.Context, issuer, rawToken, expectedNonce string) (*verifiedToken, error) {
	// The discovery and JWKS fetches use the bounded client: the refresh
	// path runs under the exclusive store lock, and an unbounded fetch
	// there would stall every kip invocation on the machine.
	ctx = oidc.ClientContext(ctx, authHTTPClient)
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("fetching issuer metadata: %w", err)
	}
	verifier := provider.Verifier(&oidc.Config{
		ClientID:             clientID,
		SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256},
	})
	idToken, err := verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("verifying id token: %w", err)
	}
	if expectedNonce != "" && idToken.Nonce != expectedNonce {
		return nil, fmt.Errorf("nonce mismatch: the token does not belong to this login attempt")
	}
	var claims struct {
		Email string `json:"email"`
		Azp   string `json:"azp"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("reading id token claims: %w", err)
	}
	// OIDC Core §3.1.3.7 authorized-party checks, which go-oidc leaves to
	// the caller: audience membership alone would accept a token minted for
	// a different client that merely lists this one in its audience set.
	if len(idToken.Audience) > 1 && claims.Azp == "" {
		return nil, fmt.Errorf("id token has multiple audiences and no azp claim")
	}
	if claims.Azp != "" && claims.Azp != clientID {
		return nil, fmt.Errorf("id token azp %q is a different client than %s", claims.Azp, clientID)
	}
	return &verifiedToken{Email: claims.Email, ExpiresAt: idToken.Expiry}, nil
}
