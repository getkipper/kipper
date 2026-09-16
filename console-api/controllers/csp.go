package controllers

import (
	"fmt"
	"strings"
)

// buildWorkloadCSP renders the shared App/Function policy, adding operator
// domains to script, style, and font sources. Inline scripts and eval remain
// allowed for compatibility with hosted apps. connect-src already permits
// all HTTPS and WSS origins.
func buildWorkloadCSP(allowlist []string) string {
	extra := ""
	if len(allowlist) > 0 {
		extra = " " + strings.Join(allowlist, " ")
	}
	return fmt.Sprintf(
		"default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'%s; "+
			"style-src 'self' 'unsafe-inline'%s; img-src 'self' data: https:; "+
			"font-src 'self' data: https:%s; connect-src 'self' wss: https:;",
		extra, extra, extra,
	)
}
