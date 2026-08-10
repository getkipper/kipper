// Package spki computes the SubjectPublicKeyInfo fingerprint that pins the
// kipper.run gateway→cluster hop: the cluster asserts it, the gateway
// enforces it on every proxied handshake. One shared implementation, because
// any drift between the two sides would break every pinned handshake.
package spki

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
)

// Fingerprint returns the lowercase-hex SHA-256 of a certificate's
// SubjectPublicKeyInfo. Pinning the public key rather than the whole leaf
// means a certificate reissue with the same key never changes the pin.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}
