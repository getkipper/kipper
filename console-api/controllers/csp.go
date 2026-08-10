package controllers

import (
	"fmt"
	"strings"
)

// buildWorkloadCSP renders the Content-Security-Policy Kipper attaches to a
// tenant workload's routes, with any operator-supplied domains added.
//
// The policy is permissive by design. It is applied to applications Kipper knows
// nothing about, so dropping 'unsafe-inline' or 'unsafe-eval' would break any
// app with an inline script, an inline handler, or a framework that evals — on
// upgrade, with a console error as the only clue. Operators who want more
// control already have two levers on the CR: NoSecurityHeaders removes the
// header entirely, and CSPAllowlist widens it. A strict opt-in mode is a
// feature, not a change of this default.
//
// connect-src deliberately omits the allowlist: it already carries the https:
// scheme-source, which permits any HTTPS origin, so listing individual hosts
// there would add nothing.
//
// Apps and functions share this because they serve the same kind of traffic.
// They previously had a copy each, and the copies had drifted.
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
