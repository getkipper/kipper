// Package velerolabel computes the label values Velero writes for object names.
package velerolabel

import (
	"crypto/sha256"
	"encoding/hex"
)

// ValidName formats an object name to match Velero's backup-name label selectors.
// Names longer than 63 bytes use a 57-byte prefix and six hex digits of the SHA-256 hash.
// The input must already use valid label characters.
//
// See https://github.com/vmware-tanzu/velero/blob/main/pkg/label/label.go.
func ValidName(name string) string {
	const dns1035Max = 63
	if len(name) <= dns1035Max {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	const charsFromLabel = dns1035Max - 6
	return name[:charsFromLabel] + hex.EncodeToString(sum[:])[:6]
}
