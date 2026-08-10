package middleware

import (
	"net/http"
	"os"
)

// CheckWebSocketOrigin guards WebSocket upgrades against cross-site
// hijacking. Browsers always send an Origin header on a WebSocket
// handshake, so a request whose Origin is not the console is rejected.
// Requests with no Origin header come from non-browser clients (the CLI,
// a websocket library) and are not a CSRF vector, so they are allowed.
// With no CONSOLE_DOMAIN configured the check fails closed.
func CheckWebSocketOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if consoleDomain := os.Getenv("CONSOLE_DOMAIN"); consoleDomain != "" {
		return origin == "https://"+consoleDomain
	}
	return false
}
