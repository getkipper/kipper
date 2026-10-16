package v1alpha1

import (
	"github.com/getkipper/kipper/controller/pkg/identity"
)

// CutoverApprovalHash returns the SHA-256 approval hash shared by the CLI and
// reconciler for a host cutover. It binds the observed generation, source and
// target hosts (including issuers), and transition nonce, and returns lowercase hex.
func CutoverApprovalHash(observedGeneration int64, from, to ResolvedHosts, nonce string) string {
	return identity.ApprovalHash(observedGeneration, from.key(), to.key(), nonce)
}

// key is the canonical serialization of a host set for hashing.
func (h ResolvedHosts) key() string {
	return identity.HostKey(h.Console, h.ConsoleAPI, h.Dex, h.Issuer)
}
