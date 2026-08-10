// Package identity holds the serving-identity contract shared between the
// console-api reconciler (which drives a host change) and the kip CLI (which
// approves the one session-invalidating step). Keeping the approval-hash
// encoding here means the two sides call one function and can never disagree on
// it, without the CLI depending on the whole console-api module.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ApprovalHash is the value the CLI writes to spec.cutoverApproval to approve the
// one session-invalidating step of a host change, and the value the reconciler
// recomputes to check it. Binding the hash to the whole pending transition — the
// spec generation the reconciler observed, the from/to host keys, and the
// per-transition nonce — means a stale or replayed approval never matches:
// editing the host target bumps the generation and rewrites the nonce, so any
// earlier approval is void.
func ApprovalHash(observedGeneration int64, fromKey, toKey, nonce string) string {
	data := fmt.Sprintf("v1\n%d\n%s\n%s\n%s", observedGeneration, fromKey, toKey, nonce)
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// HostKey is the canonical serialisation of a resolved host set for hashing. The
// issuer is included explicitly so a base-domain move that changes only the
// issuer still changes the hash.
func HostKey(console, consoleAPI, dex, issuer string) string {
	return console + "|" + consoleAPI + "|" + dex + "|" + issuer
}
