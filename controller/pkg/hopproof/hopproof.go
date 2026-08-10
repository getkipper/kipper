// Package hopproof is the shared proof-of-possession primitive for the
// kipper.run gateway registration challenge (B16). The cluster proves it
// controls the registered IP by signing a gateway-issued nonce with the
// hop-certificate private key; the gateway verifies the signature against the
// public key it independently observes at the registered IP:443. Echoing the
// destination's public certificate is not enough — only the private-key holder
// can produce the signature.
//
// Both the signer (console-api) and the verifier (gateway) build the signed
// message from this one package, so the canonical encoding can never drift
// between them, which would silently fail every proof.
package hopproof

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// Protocol is the message-format version. It domain-separates this signature
// from any other use of the hop key and is recorded on the durable proof so a
// future verifier can tell which protocol a lease was obtained under.
const Protocol = "kipper-hop-proof-v1"

// domainPrefix is the message's leading domain-separation tag.
const domainPrefix = Protocol

// Digest returns the SHA-256 the two sides sign and verify. The fields are
// newline-joined behind a version/domain prefix: nonce and token are hex,
// label is a DNS label, ip is a validated address, and origin is the gateway
// base domain, so none can contain a newline and the encoding is unambiguous.
// Binding label, ip, origin, and token makes a signature usable only for the
// exact registration it was issued for.
func Digest(nonce, label, ip, origin, token string) []byte {
	msg := strings.Join([]string{domainPrefix, nonce, label, ip, origin, token}, "\n")
	sum := sha256.Sum256([]byte(msg))
	return sum[:]
}

// Sign produces the base64 ASN.1 ECDSA signature of the challenge digest with
// the hop-certificate private key.
func Sign(key *ecdsa.PrivateKey, nonce, label, ip, origin, token string) (string, error) {
	sig, err := ecdsa.SignASN1(rand.Reader, key, Digest(nonce, label, ip, origin, token))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// Verify reports whether sigB64 is a valid signature of the challenge digest
// under pub — the public key the gateway observed at the registered IP. A
// malformed signature or a mismatch returns false, never an error, so the
// verifier treats "cannot prove" uniformly.
func Verify(pub *ecdsa.PublicKey, nonce, label, ip, origin, token, sigB64 string) bool {
	if pub == nil {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	return ecdsa.VerifyASN1(pub, Digest(nonce, label, ip, origin, token), sig)
}
