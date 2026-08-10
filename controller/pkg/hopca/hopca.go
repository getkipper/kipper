// Package hopca mints the certificate authority a cluster anchors its own hop
// certificate on, and the leaves signed under it.
//
// The hop certificate serves two consumers that verify it in incompatible ways.
// The kipper.run gateway pins its SPKI and ignores everything else, so for the
// gateway the certificate's names and issuer are irrelevant. The cluster's own
// API server, fetching OIDC discovery and JWKS from the Dex host through the
// loopback pin, does ordinary verification: it needs a name that matches and a
// chain it trusts. A self-signed leaf with no names satisfies the first and can
// never satisfy the second, which is what deadlocked fresh installs on a default
// *.kipper.run domain.
//
// A CA resolves it without changing what the gateway sees. Signing a key under a
// CA does not alter its SPKI, so the pin is untouched and no registration has to
// be re-observed; the leaf gains a wildcard name that matches every host a
// cluster is reached by; and the API server is handed the CA, which stays valid
// across leaf reissues and key rotations where a leaf anchor would not.
//
// This package is deliberately crypto only. It holds no Kubernetes types so the
// installer, which talks to a cluster over SSH, and the console-api reconciler,
// which talks to the API, can share one definition of what this material is
// instead of keeping a copy each.
package hopca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// RetainedCAKey is where an outgoing authority sits on the CA Secret while it
// is being replaced: still trusted, no longer signing.
//
// The name lives here because two modules that cannot import each other both
// depend on it agreeing. The installer builds the host trust anchor from it,
// console-api renders the same bundle from the Secret to gate a domain cutover,
// and if the two ever named it differently their hashes would stop matching and
// every cutover would park mid-transaction. Each side used to keep its own copy,
// guarded by a test that compared a constant against a hand-copied literal in
// the same file — an assertion that could not fail.
const RetainedCAKey = "previous.crt"

const (
	// CALifetime is long because rolling the CA is a two-anchor transaction
	// across the API server's authentication config, not something to schedule
	// casually. See LeafLifetime for why a long CA does not mean a long leaf.
	CALifetime = 30 * 365 * 24 * time.Hour

	// LeafLifetime is the validity a freshly signed leaf asks for. It is capped
	// at the CA's own expiry (see SignLeaf), so a leaf can never outlive the
	// anchor the API server was given — an unanchored leaf would fail every
	// verification with no in-cluster repair path.
	LeafLifetime = 10 * 365 * 24 * time.Hour

	// backdate covers modest clock skew, so a certificate is accepted as
	// current the moment it is written.
	backdate = time.Hour

	caCommonName   = "kipper-hop-ca"
	leafCommonName = "kipper-hop"
)

// Material is a complete set: a CA and the leaf signed under it, PEM encoded.
type Material struct {
	CACertPEM   []byte
	CAKeyPEM    []byte
	LeafCertPEM []byte
	LeafKeyPEM  []byte
}

// WildcardFor returns the SAN a hop leaf carries for a gateway domain. One
// wildcard covers every host a gateway-fronted cluster is reached by, because
// each is a single label under the shared apex — the cluster itself, and every
// derived route that joins a service prefix to the cluster label. It matches
// nothing deeper, and no custom domain, so a leaf can never satisfy the
// certificate gate that custom domains rely on.
func WildcardFor(gatewayDomain string) string {
	return "*." + gatewayDomain
}

// New mints a CA and a first leaf under it for the given gateway domain.
func New(gatewayDomain string) (Material, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Material{}, fmt.Errorf("generating CA key: %w", err)
	}
	caCert, err := selfSignedCA(caKey)
	if err != nil {
		return Material{}, err
	}
	caCertPEM, caKeyPEM, err := encode(caCert, caKey)
	if err != nil {
		return Material{}, err
	}

	leafCertPEM, leafKeyPEM, err := NewLeaf(caCertPEM, caKeyPEM, gatewayDomain)
	if err != nil {
		return Material{}, err
	}
	return Material{
		CACertPEM:   caCertPEM,
		CAKeyPEM:    caKeyPEM,
		LeafCertPEM: leafCertPEM,
		LeafKeyPEM:  leafKeyPEM,
	}, nil
}

// NewLeaf mints a fresh keypair and signs it under the CA. Used for a first
// leaf and for staging a rotation candidate; both mean a new SPKI, which the
// gateway must observe before it serves.
func NewLeaf(caCertPEM, caKeyPEM []byte, gatewayDomain string) (leafCertPEM, leafKeyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating hop key: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding hop key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	certPEM, err := SignLeaf(caCertPEM, caKeyPEM, keyPEM, gatewayDomain)
	if err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}

// SignLeaf issues a leaf certificate for an existing private key under the CA.
//
// This is the reissue path and the adoption path both: reissuing keeps the key,
// so the SPKI and therefore the gateway's pin are unchanged, and adopting a
// cluster's existing self-signed leaf means signing that same key under the new
// CA so its registration survives untouched.
func SignLeaf(caCertPEM, caKeyPEM, leafKeyPEM []byte, gatewayDomain string) ([]byte, error) {
	caCert, err := ParseCert(caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("parsing CA certificate: %w", err)
	}
	caKey, err := parseKey(caKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parsing CA key: %w", err)
	}
	leafKey, err := parseKey(leafKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parsing hop key: %w", err)
	}

	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	notAfter := time.Now().Add(LeafLifetime)
	// A leaf may not outlive the anchor the API server holds: past the CA's own
	// expiry the chain stops verifying, and nothing inside the cluster can hand
	// the API server a new anchor. Capping here makes a CA nearing its end
	// visible as shortening leaves rather than as a cliff.
	if notAfter.After(caCert.NotAfter) {
		notAfter = caCert.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: leafCommonName},
		DNSNames:              []string{WildcardFor(gatewayDomain)},
		NotBefore:             time.Now().Add(-backdate),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("signing hop certificate: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// Bundle joins the authorities a cluster's API server should trust, in a
// canonical order so the installer writing the file and the reconciler hashing
// it produce identical bytes. The active signer comes first and a retained one
// second; either may be empty.
//
// A bundle exists so an authority can be replaced without a window where
// nothing verifies. The API server holds one anchor value, and swapping it and
// the certificate it anchors cannot be simultaneous: whichever moves first,
// verification fails until the other catches up. Trusting both across the
// transition removes the window entirely, which is the same shape as the
// loopback pin's superset invariant in kip's operator auth.
func Bundle(active, retained []byte) []byte {
	var out []byte
	for _, pem := range [][]byte{active, retained} {
		trimmed := trimSpace(pem)
		if len(trimmed) == 0 {
			continue
		}
		out = append(out, trimmed...)
		out = append(out, '\n')
	}
	return out
}

// SignedByAny reports whether the leaf verifies against any authority in a
// bundle. During a rollover both the outgoing and incoming authority are
// trusted, so a leaf signed by either is legitimate.
func SignedByAny(leafCertPEM, bundlePEM []byte) bool {
	leaf, err := ParseCert(leafCertPEM)
	if err != nil {
		return false
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundlePEM) {
		return false
	}
	_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err == nil
}

// SignedBy reports whether the leaf verifies against the CA. The installer and
// the reconciler both use it to tell an adopted legacy leaf, which must be
// re-signed, from one already anchored.
func SignedBy(leafCertPEM, caCertPEM []byte) bool {
	leaf, err := ParseCert(leafCertPEM)
	if err != nil {
		return false
	}
	ca, err := ParseCert(caCertPEM)
	if err != nil {
		return false
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err == nil
}

// ParseCert decodes the first PEM block as a certificate.
//
// Callers must pass a single certificate, never a chain: the SPKI of whatever
// this returns is what a cluster asserts to the gateway as the key it serves,
// so decoding a chain that happened to lead with the CA would pin the CA and
// fail every handshake against the leaf actually served.
func ParseCert(certPEM []byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("not a PEM certificate")
	}
	if len(trimSpace(rest)) > 0 {
		return nil, fmt.Errorf("expected a single certificate, got a chain of at least 2")
	}
	return x509.ParseCertificate(block.Bytes)
}

// NeedsReissue reports whether a certificate's remaining validity has dropped
// below window.
func NeedsReissue(cert *x509.Certificate, window time.Duration) bool {
	return time.Until(cert.NotAfter) < window
}

func selfSignedCA(key *ecdsa.PrivateKey) (*x509.Certificate, error) {
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: caCommonName},
		NotBefore:             time.Now().Add(-backdate),
		NotAfter:              time.Now().Add(CALifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("creating CA certificate: %w", err)
	}
	return x509.ParseCertificate(der)
}

func encode(cert *x509.Certificate, key *ecdsa.PrivateKey) (certPEM, keyPEM []byte, err error) {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding CA key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

func parseKey(keyPEM []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an ECDSA key")
	}
	return key, nil
}

func serialNumber() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generating serial number: %w", err)
	}
	return serial, nil
}

// trimSpace avoids importing strings for one call on a byte slice.
func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
