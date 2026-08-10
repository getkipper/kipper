package v1alpha1

import (
	"github.com/getkipper/kipper/controller/pkg/identity"
)

// CutoverApprovalHash is the value the CLI writes to spec.cutoverApproval to
// approve the one session-invalidating step of a host change, and the value the
// reconciler recomputes to check it. Both the CLI (which computes it from CR
// status) and the reconciler call the shared identity.ApprovalHash, so they can
// never disagree on the encoding.
func CutoverApprovalHash(observedGeneration int64, from, to ResolvedHosts, nonce string) string {
	return identity.ApprovalHash(observedGeneration, from.key(), to.key(), nonce)
}

// key is the canonical serialisation of a host set for hashing.
func (h ResolvedHosts) key() string {
	return identity.HostKey(h.Console, h.ConsoleAPI, h.Dex, h.Issuer)
}
