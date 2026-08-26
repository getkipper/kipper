package ws

import (
	"context"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/controller/pkg/capability"
)

// authSubprotocol is the sentinel the client sends as the first
// Sec-WebSocket-Protocol value; the Dex JWT follows as the second. The
// browser WebSocket API can set subprotocols but not headers, and a JWT
// is a valid RFC 6455 subprotocol token, so this keeps the token out of
// the URL where it would land in proxy logs and browser history.
const authSubprotocol = "kipper.auth"

// tokenFromRequest pulls the JWT from the Sec-WebSocket-Protocol header.
// The header is "kipper.auth, <jwt>"; the token is the value that is not
// the sentinel.
func tokenFromRequest(r *http.Request) string {
	for _, proto := range r.Header["Sec-Websocket-Protocol"] {
		for _, v := range strings.Split(proto, ",") {
			if v = strings.TrimSpace(v); v != "" && v != authSubprotocol {
				return v
			}
		}
	}
	return ""
}

// AuthenticatedEmail validates the Dex JWT the WebSocket client carries in
// the Sec-WebSocket-Protocol header and returns the caller's email. These
// handlers run on a raw mux that bypasses the Chi auth chain, so they
// authenticate here. Exported so the migration progress WebSocket, which lives
// in another package, authenticates the same way.
func AuthenticatedEmail(r *http.Request, issuer, audience string, keyFunc jwt.Keyfunc) (string, bool) {
	tokenStr := tokenFromRequest(r)
	if tokenStr == "" {
		return "", false
	}
	// Console audience only: the CLI streams logs and opens shells
	// through the Kubernetes API, never these WebSockets, so CLI
	// tokens are rejected here (see middleware.CLIAudience).
	claims := &middleware.Claims{}
	if _, err := jwt.ParseWithClaims(tokenStr, claims, keyFunc,
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
	); err != nil {
		return "", false
	}
	// Same email-trust gate as the REST middleware: these WebSockets authorize
	// by email, so an explicitly-unverified email must not open a log stream,
	// terminal, or the migration-admin gate.
	if !claims.EmailUsableForAuth() {
		return "", false
	}
	return claims.Email, true
}

// authorizeProject reports whether the caller holds the capability on the
// project that owns the namespace.
func authorizeProject(ctx context.Context, resolver *middleware.ProjectAccessResolver, email, namespace string, required capability.Name) bool {
	if resolver == nil {
		return false
	}
	access, ok := resolver.Resolve(ctx, email, namespace)
	return ok && access.Allows(required)
}
