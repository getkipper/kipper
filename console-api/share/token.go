// Package share issues and validates capability tokens that let a
// non-Dex user open exactly one browseable service UI for a bounded
// time. A share token is deliberately unlike a Dex token: HS256 signed
// with a dedicated key, its own issuer and subject, and an audience
// pinned to a single UI host. Those structural differences are what
// keep it from ever satisfying the RS256/Dex validators that guard the
// REST API and the WebSocket endpoints.
//
// Every token is backed by a server-side grant Secret (see grants.go):
// the token names its signing key by kid and its grant by jti, and it
// carries the Service object's immutable UID. Validation checks the
// signature, the claims, the grant's existence (revocation), and the
// live Service UID, so revoking a grant or rotating the key kills a link
// without touching the others.
package share

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// Issuer and Subject are pinned into every token and required at
	// validation. They give a share token an identity no Dex token
	// carries, so a Dex token can never be mistaken for a share token
	// and vice versa.
	Issuer  = "kipper-share"
	Subject = "service-share"

	// DefaultLifetime is what a link gets when the minter does not ask
	// for anything; MaxLifetime is the hard cap. Long enough for a
	// stakeholder review cycle, short enough that a leaked link ages out.
	DefaultLifetime = 7 * 24 * time.Hour
	MaxLifetime     = 30 * 24 * time.Hour

	// signingMethod is pinned so validation never selects an algorithm
	// from the token header (the algorithm-confusion class of bug).
	signingMethod = "HS256"

	// leeway absorbs small clock differences between the minting CLI
	// and the validating console-api.
	leeway = 30 * time.Second
)

// ErrInvalid is returned for every validation failure. It carries no
// detail about which check failed, so the gate can't become an oracle
// for forging tokens.
var ErrInvalid = errors.New("invalid share token")

// Claims are the registered claims a share token carries. Service is
// the "<namespace>/<name>" the token was minted for, recorded for
// audit/log context. The security-relevant bindings are Audience (the
// exact UI host) and ServiceUID (the Service CR's immutable UID, so a
// deleted-and-recreated service under the same name invalidates every
// old link).
type Claims struct {
	jwt.RegisteredClaims
	Service    string `json:"svc"`
	ServiceUID string `json:"uid,omitempty"`
}

// MintGrant signs a share token bound to a grant: the kid header names
// the signing key, jti ties the token to its grant Secret, and the uid
// claim pins the Service object's identity. now is passed in so callers
// and tests control the clock.
func MintGrant(kr *Keyring, g Grant, now time.Time) (string, error) {
	if kr == nil || len(kr.CurrentKey) == 0 || kr.CurrentKID == "" {
		return "", fmt.Errorf("share: keyring has no current key")
	}
	if g.Host == "" || g.JTI == "" || g.ServiceUID == "" {
		return "", fmt.Errorf("share: grant is incomplete")
	}
	lifetime := g.ExpiresAt.Sub(now)
	if lifetime <= 0 || lifetime > MaxLifetime {
		return "", fmt.Errorf("share: lifetime must be between 0 and %s", MaxLifetime)
	}

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			Subject:   Subject,
			Audience:  jwt.ClaimStrings{g.Host},
			ID:        g.JTI,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(g.ExpiresAt),
		},
		Service:    g.Namespace + "/" + g.Service,
		ServiceUID: g.ServiceUID,
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = kr.CurrentKID
	signed, err := tok.SignedString(kr.CurrentKey)
	if err != nil {
		return "", fmt.Errorf("share: signing token: %w", err)
	}
	return signed, nil
}

// ValidateGrantToken checks a share token against the keyring and the
// exact host it must be bound to. The signing key is selected strictly by
// the token's kid header — a missing or unknown kid fails, no key is ever
// tried blindly. It pins HS256, the share issuer and subject, requires a
// single audience equal to host, requires an expiry and a bounded
// lifetime, and requires both the jti and uid claims. On any failure it
// returns ErrInvalid with no detail. The caller must still resolve the jti
// against the grant store and compare the grant's fields.
func ValidateGrantToken(kr *Keyring, tokenStr, host string, now time.Time) (*Claims, error) {
	if kr == nil || tokenStr == "" || host == "" {
		return nil, ErrInvalid
	}

	claims := &Claims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{signingMethod}),
		jwt.WithIssuer(Issuer),
		jwt.WithSubject(Subject),
		jwt.WithAudience(host),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(leeway),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	if _, err := parser.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		key := kr.KeyFor(kid)
		if key == nil {
			return nil, ErrInvalid
		}
		return key, nil
	}); err != nil {
		return nil, ErrInvalid
	}

	if claims.IssuedAt == nil || claims.ExpiresAt == nil {
		return nil, ErrInvalid
	}
	life := claims.ExpiresAt.Sub(claims.IssuedAt.Time)
	if life <= 0 || life > MaxLifetime {
		return nil, ErrInvalid
	}
	if claims.ID == "" || claims.ServiceUID == "" {
		return nil, ErrInvalid
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != host {
		return nil, ErrInvalid
	}

	return claims, nil
}

// JTIPrefix returns a short, non-secret handle for a token's id, used
// as the X-Auth-User a backend sees (share:<prefix>). It identifies the
// link, not a person.
func JTIPrefix(jti string) string {
	const n = 8
	if len(jti) <= n {
		return jti
	}
	return jti[:n]
}

// CanonicalHost lowercases and strips a forwarded authority to a bare
// hostname, rejecting anything with userinfo, a path, or an unexpected
// shape. The gate compares the result against the token audience, so a
// spoofed or malformed X-Forwarded-Host can't produce a mismatched
// redirect or a wrong-host cookie.
func CanonicalHost(forwarded string) (string, error) {
	h := strings.TrimSpace(forwarded)
	if h == "" {
		return "", fmt.Errorf("share: empty host")
	}
	if strings.ContainsAny(h, "/@?#\\") {
		return "", fmt.Errorf("share: malformed host %q", forwarded)
	}
	// Strip a port if present; the UI host is a bare name under the
	// wildcard cert. A bracketed IPv6 literal is not a valid service
	// UI host, so reject it rather than mis-split.
	if strings.Contains(h, "[") {
		return "", fmt.Errorf("share: unexpected host %q", forwarded)
	}
	if i := strings.LastIndex(h, ":"); i >= 0 {
		h = h[:i]
	}
	h = strings.ToLower(h)
	if h == "" || strings.Contains(h, ":") {
		return "", fmt.Errorf("share: malformed host %q", forwarded)
	}
	return h, nil
}
