// Package applink holds the parts of a cross-project app link that more than
// one module has to agree about.
//
// A link is declared on the calling app and rendered by the App reconciler,
// which injects the target's address as an environment variable. The CLI and
// the console API both name that variable — to refuse a link whose name is
// already taken, to show a link, to withdraw one — and they are separate
// modules from the reconciler that writes it. One of them spelling it
// differently means writing or removing a variable the app never receives, so
// the rule lives here rather than in each of them.
package applink

import "strings"

// EnvKey is the environment variable a link to the named app injects the
// target's address as: "domain-service" → "DOMAIN_SERVICE_URL".
func EnvKey(app string) string {
	return strings.ToUpper(strings.ReplaceAll(app, "-", "_")) + "_URL"
}
