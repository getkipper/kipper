package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

type contextKey string

// UserContextKey is the context key for authenticated user claims.
const UserContextKey contextKey = "user"

// DefaultAudience is the OAuth client ID the console registers with Dex.
// Dex sets it as the `aud` claim on every ID token, so it is also the
// audience the API validates. Override with DEX_CLIENT_ID.
const DefaultAudience = "kipper-console"

// CLIAudience is the public OAuth client the kip CLI authenticates as
// (loopback redirect, no secret). Dex issues CLI tokens with this
// audience. Only the REST bearer path accepts it: the CLI talks to
// the Kubernetes API directly for logs and exec, so the forward-auth
// SSO gate and the raw WebSockets stay console-only — a stolen CLI
// token must not open browser SSO, log streams, or terminals.
const CLIAudience = "kipper-cli"

// Claims represents the JWT claims from a Dex-issued token. EmailVerified is a
// pointer so an absent claim (nil) is distinguishable from an explicit false —
// authorization keys off Email, so a token that carries an email but marks it
// unverified must not be trusted for a role.
type Claims struct {
	jwt.RegisteredClaims
	Email         string   `json:"email"`
	EmailVerified *bool    `json:"email_verified,omitempty"`
	Name          string   `json:"name"`
	Groups        []string `json:"groups"`
}

// UserFromContext extracts the authenticated user claims from the request context.
func UserFromContext(ctx context.Context) *Claims {
	claims, _ := ctx.Value(UserContextKey).(*Claims)
	return claims
}

// EmailUsableForAuth reports whether the token's email may drive authorization.
// Roles are keyed by email, so a token that carries an email its issuer
// explicitly marked unverified must not be trusted. An absent email_verified
// claim (nil) is accepted so the default Dex local-password connector, whose
// operator-provisioned identities have no mailbox-verification event, keeps
// working. Every Dex-token entry point must gate on this before using the email.
func (c *Claims) EmailUsableForAuth() bool {
	return c.Email == "" || c.EmailVerified == nil || *c.EmailVerified
}

// Auth returns middleware that validates JWT tokens issued by Dex.
// The issuerURL is used to validate the token's issuer claim.
// keyFunc is used to look up the signing key for token verification.
type Auth struct {
	Issuer   string
	Audience string
	KeyFunc  jwt.Keyfunc
}

// Handler returns the HTTP middleware that enforces authentication.
func (a *Auth) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := a.extractAndValidate(r)
		if err != nil {
			http.Error(w, fmt.Sprintf("unauthorized: %s", err), http.StatusUnauthorized)
			return
		}

		claims, ok := token.Claims.(*Claims)
		if !ok {
			http.Error(w, "unauthorized: invalid claims", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), UserContextKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *Auth) extractAndValidate(r *http.Request) (*jwt.Token, error) {
	// Middleware-chain validation: header only. Accepting the
	// kipper_auth cookie here would let any same-site request from
	// a service UI subdomain ride the user's session to call
	// console-api endpoints — a CSRF vector. The cookie path is
	// reserved for /auth/check (a read-only auth probe).
	raw, err := extractBearerToken(r)
	if err != nil {
		return nil, err
	}

	token, err := jwt.ParseWithClaims(raw, &Claims{}, a.KeyFunc,
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(a.Issuer),
		jwt.WithAudience(a.Audience, CLIAudience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	// Central email-trust gate: every caller of extractAndValidate (the REST
	// middleware and ValidateRequest) refuses an explicitly-unverified email
	// here, so the policy can't drift between entry points.
	if claims, ok := token.Claims.(*Claims); ok && !claims.EmailUsableForAuth() {
		return nil, fmt.Errorf("email not verified")
	}

	return token, nil
}

// ValidateRequest validates an Authorization Bearer header. Used by
// API endpoints that aren't behind the middleware chain (e.g. a
// custom handler that wants to know who's calling).
func (a *Auth) ValidateRequest(r *http.Request) (*Claims, error) {
	token, err := a.extractAndValidate(r)
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*Claims)
	if !ok {
		return nil, fmt.Errorf("invalid claims")
	}
	return claims, nil
}

// ValidateIDToken verifies a raw Dex ID token independent of any HTTP
// header or cookie: RS256, Dex issuer, console audience, expiry required.
// Used by Callback to trust the email in the ID token it just received
// from Dex, and never to authenticate an inbound request (that stays with
// extractAndValidate, which reads the header only).
func (a *Auth) ValidateIDToken(raw string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(raw, &Claims{}, a.KeyFunc,
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(a.Issuer),
		jwt.WithAudience(a.Audience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok {
		return nil, fmt.Errorf("invalid claims")
	}
	if !claims.EmailUsableForAuth() {
		return nil, fmt.Errorf("email not verified")
	}
	return claims, nil
}

// extractBearerToken pulls the raw JWT from the Authorization header.
// Header-only — the gate authenticates service-UI browsers through
// uisession artefacts, never a Dex token from a cookie.
func extractBearerToken(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", fmt.Errorf("missing authorization header")
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return "", fmt.Errorf("invalid authorization header format")
	}
	return parts[1], nil
}
