package uisession

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	signingMethod = "HS256"

	// issuerCode marks the single-use SSO handoff code; issuerSession marks
	// the browser session cookie. Pinning the issuer per artefact kind means
	// a code can never be replayed as a session or vice versa.
	issuerCode    = "kipper-ui-code"
	issuerSession = "kipper-ui-session"

	// CodeTTL bounds the console→UI handoff code: single use, one host.
	CodeTTL = 60 * time.Second
	// SessionIdleTTL is the sliding session lifetime; a session is re-minted
	// at the gate when less than SessionRenewBefore remains.
	SessionIdleTTL     = 30 * time.Minute
	SessionRenewBefore = 15 * time.Minute
	// SessionAbsoluteTTL caps a session's total life from its auth_time,
	// forcing a full re-login however active the operator is.
	SessionAbsoluteTTL = 12 * time.Hour

	leeway = 30 * time.Second
)

// CookieName is the per-host session cookie name. The __Host- prefix makes
// browsers refuse it if it ever carries a Domain attribute or arrives over
// plain HTTP, which defeats cookie-tossing from a sibling subdomain. The host
// label keeps one UI's cookie from shadowing another's.
func CookieName(host string) string {
	label := host
	if i := indexByte(host, '.'); i > 0 {
		label = host[:i]
	}
	return "__Host-kipper-ui-" + label
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// claims are the artefact claims. Sub is the Dex subject; Email drives the
// X-Auth-User header and the role check. SID is the server-side record id
// (stable across renewals). AuthTime anchors the absolute lifetime.
type claims struct {
	Email    string `json:"email"`
	SID      string `json:"sid"`
	AuthTime int64  `json:"auth_time"`
	jwt.RegisteredClaims
}

func newSID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("uisession: generating sid: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func signWith(kr *Keyring, c claims) (string, error) {
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	tok.Header["kid"] = kr.CurrentKID
	return tok.SignedString(kr.CurrentKey)
}

// MintCode issues a single-use SSO code for one host. sid is the future
// session record id, so redeeming the code creates a record under that id and
// the resulting session carries the same sid.
func MintCode(kr *Keyring, sub, email, host string, now time.Time) (code, sid string, err error) {
	sid, err = newSID()
	if err != nil {
		return "", "", err
	}
	c := claims{
		Email: email,
		SID:   sid,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuerCode,
			Subject:   sub,
			Audience:  jwt.ClaimStrings{host},
			ID:        sid,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(CodeTTL)),
		},
	}
	code, err = signWith(kr, c)
	return code, sid, err
}

// MintSession issues a browser session cookie for one host. authTime anchors
// the absolute lifetime; a re-mint keeps the same sid and authTime.
func MintSession(kr *Keyring, sub, email, host, sid string, authTime, now time.Time) (string, error) {
	c := claims{
		Email:    email,
		SID:      sid,
		AuthTime: authTime.Unix(),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuerSession,
			Subject:   sub,
			Audience:  jwt.ClaimStrings{host},
			ID:        sid,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(SessionIdleTTL)),
		},
	}
	return signWith(kr, c)
}

// Validated carries what a valid artefact yields.
type Validated struct {
	Sub      string
	Email    string
	SID      string
	Host     string
	AuthTime time.Time
	// Expiry is the artefact's own exp, used by the gate to decide when a
	// session is close enough to expiry to re-mint.
	Expiry time.Time
}

func validate(kr *Keyring, tokenStr, issuer, host string, maxSpan time.Duration, now time.Time) (*Validated, error) {
	c := &claims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{signingMethod}),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(host),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(leeway),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	_, err := parser.ParseWithClaims(tokenStr, c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		key := kr.KeyFor(kid)
		if key == nil {
			return nil, fmt.Errorf("uisession: unknown key id")
		}
		return key, nil
	})
	if err != nil {
		// One opaque error for every failure mode, so a caller cannot learn
		// which check failed.
		return nil, fmt.Errorf("uisession: invalid token")
	}
	// Reject a token whose lifetime span exceeds its class maximum, so a
	// forged-but-somehow-signed token cannot claim an oversized window.
	if c.IssuedAt == nil || c.ExpiresAt == nil || c.ExpiresAt.Sub(c.IssuedAt.Time) > maxSpan+leeway {
		return nil, fmt.Errorf("uisession: invalid token")
	}
	if len(c.Audience) != 1 || c.Audience[0] != host {
		return nil, fmt.Errorf("uisession: invalid token")
	}
	v := &Validated{Sub: c.Subject, Email: c.Email, SID: c.SID, Host: host, Expiry: c.ExpiresAt.Time}
	if c.AuthTime > 0 {
		v.AuthTime = time.Unix(c.AuthTime, 0)
	}
	return v, nil
}

// ValidateCode checks an SSO code for the given host.
func ValidateCode(kr *Keyring, tokenStr, host string, now time.Time) (*Validated, error) {
	return validate(kr, tokenStr, issuerCode, host, CodeTTL, now)
}

// ValidateSession checks a session cookie for the given host, enforcing the
// idle span here; the absolute cap is enforced by the caller against AuthTime
// (which the record also holds).
func ValidateSession(kr *Keyring, tokenStr, host string, now time.Time) (*Validated, error) {
	v, err := validate(kr, tokenStr, issuerSession, host, SessionIdleTTL, now)
	if err != nil {
		return nil, err
	}
	if v.AuthTime.IsZero() || now.After(v.AuthTime.Add(SessionAbsoluteTTL)) {
		return nil, fmt.Errorf("uisession: session past absolute lifetime")
	}
	return v, nil
}
