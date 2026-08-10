package hopca

import (
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/getkipper/kipper/controller/pkg/spki"
)

const gatewayDomain = "kipper.run"

func mint(t *testing.T) Material {
	t.Helper()
	m, err := New(gatewayDomain)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// The whole design rests on this: signing a key under a CA does not change its
// SPKI, so the gateway's pin survives and no registration has to be
// re-observed. If this were false the fix would 502 every cluster it touched.
func TestSigningUnderTheCADoesNotMoveThePin(t *testing.T) {
	m := mint(t)
	leaf, err := ParseCert(m.LeafCertPEM)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	before := spki.Fingerprint(leaf)

	// Re-sign the same key: this is the reissue path.
	reissuedPEM, err := SignLeaf(m.CACertPEM, m.CAKeyPEM, m.LeafKeyPEM, gatewayDomain)
	if err != nil {
		t.Fatalf("SignLeaf: %v", err)
	}
	reissued, err := ParseCert(reissuedPEM)
	if err != nil {
		t.Fatalf("parse reissued: %v", err)
	}
	if after := spki.Fingerprint(reissued); after != before {
		t.Errorf("reissue moved the pin: %s -> %s", before, after)
	}
	if reissued.SerialNumber.Cmp(leaf.SerialNumber) == 0 {
		t.Error("a reissue must be a distinct certificate")
	}
}

// One wildcard covers every host a gateway-fronted cluster is reached by,
// because each is a single label under the shared apex. It must not stretch
// further, and above all it must not match a custom domain, or a leaf could
// satisfy the certificate gate custom domains depend on.
func TestLeafNameCoversEveryGatewayHostAndNothingElse(t *testing.T) {
	m := mint(t)
	leaf, err := ParseCert(m.LeafCertPEM)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	for _, host := range []string{
		"acme.kipper.run",
		"dex--acme.kipper.run",
		"console--acme.kipper.run",
		"myapp--159-195-217-19.kipper.run",
		"159-195-217-19.kipper.run",
	} {
		if err := leaf.VerifyHostname(host); err != nil {
			t.Errorf("%s must be covered: %v", host, err)
		}
	}
	for _, host := range []string{
		"a.b.kipper.run",
		"dex.example.com",
		"kipper.run",
		"kipper.run.evil.com",
	} {
		if err := leaf.VerifyHostname(host); err == nil {
			t.Errorf("%s must not be covered", host)
		}
	}
}

// The API server does ordinary verification. Anchored on the CA the chain must
// verify; anchored on the leaf itself — which is what a self-signed hop
// certificate forces — it must not, because that is the trap the old design
// walked into.
func TestLeafVerifiesAgainstTheCAAnchor(t *testing.T) {
	m := mint(t)
	leaf, err := ParseCert(m.LeafCertPEM)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	ca, err := ParseCert(m.CACertPEM)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName: "dex--159-195-217-19.kipper.run",
		Roots:   pool,
	}); err != nil {
		t.Errorf("the API server could not verify the leaf against the CA: %v", err)
	}

	if !SignedBy(m.LeafCertPEM, m.CACertPEM) {
		t.Error("SignedBy must recognise a leaf it signed")
	}
	other := mint(t)
	if SignedBy(m.LeafCertPEM, other.CACertPEM) {
		t.Error("SignedBy must reject a leaf from another cluster's CA")
	}
}

// A rotation mints a new key, so the pin moves by design — but the anchor the
// API server holds must keep working, which is the property a leaf anchor lacks.
func TestRotatedLeafStillVerifiesAgainstTheSameCA(t *testing.T) {
	m := mint(t)
	rotatedPEM, rotatedKeyPEM, err := NewLeaf(m.CACertPEM, m.CAKeyPEM, gatewayDomain)
	if err != nil {
		t.Fatalf("NewLeaf: %v", err)
	}
	if string(rotatedKeyPEM) == string(m.LeafKeyPEM) {
		t.Fatal("a rotation must mint a new key")
	}
	if !SignedBy(rotatedPEM, m.CACertPEM) {
		t.Error("the rotated leaf must verify against the unchanged CA anchor")
	}

	old, _ := ParseCert(m.LeafCertPEM)
	rotated, _ := ParseCert(rotatedPEM)
	if spki.Fingerprint(old) == spki.Fingerprint(rotated) {
		t.Error("a rotation must move the pin, or the gateway has nothing to re-observe")
	}
}

// A leaf may never outlive the anchor the API server was given: past the CA's
// expiry the chain stops verifying and nothing inside the cluster can hand the
// API server a new anchor. Repeated reissue must not walk past it.
func TestLeafNeverOutlivesItsCA(t *testing.T) {
	m := mint(t)
	ca, err := ParseCert(m.CACertPEM)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	leaf, err := ParseCert(m.LeafCertPEM)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if leaf.NotAfter.After(ca.NotAfter) {
		t.Errorf("leaf expires %s after the CA", leaf.NotAfter.Sub(ca.NotAfter))
	}

	// A CA close to its own end must cap the leaves it signs rather than issue
	// one that dies unanchored.
	shortCA, err := shortLivedCA(t, 24*time.Hour)
	if err != nil {
		t.Fatalf("short CA: %v", err)
	}
	leafPEM, _, err := NewLeaf(shortCA.CACertPEM, shortCA.CAKeyPEM, gatewayDomain)
	if err != nil {
		t.Fatalf("NewLeaf under a short CA: %v", err)
	}
	capped, err := ParseCert(leafPEM)
	if err != nil {
		t.Fatalf("parse capped: %v", err)
	}
	shortCACert, _ := ParseCert(shortCA.CACertPEM)
	if capped.NotAfter.After(shortCACert.NotAfter) {
		t.Error("a leaf signed by an expiring CA must be capped at the CA's expiry")
	}
}

// ParseCert feeds the fingerprint a cluster asserts to the gateway. Handed a
// chain it must refuse rather than silently describe whichever certificate came
// first: leading with the CA would assert the CA's key and 502 every handshake
// against the leaf actually served.
func TestParseCertRefusesAChain(t *testing.T) {
	m := mint(t)
	chain := append(append([]byte{}, m.CACertPEM...), m.LeafCertPEM...)
	if _, err := ParseCert(chain); err == nil {
		t.Error("a chain must be refused, not silently reduced to its first certificate")
	} else if !strings.Contains(err.Error(), "single certificate") {
		t.Errorf("the error must name the cause, got %v", err)
	}

	// Trailing whitespace is not a chain.
	padded := append(append([]byte{}, m.LeafCertPEM...), '\n', '\n')
	if _, err := ParseCert(padded); err != nil {
		t.Errorf("a single certificate with trailing whitespace must parse: %v", err)
	}
}

func TestCAIsUsableAsAnAuthority(t *testing.T) {
	m := mint(t)
	ca, err := ParseCert(m.CACertPEM)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	if !ca.IsCA || !ca.BasicConstraintsValid {
		t.Error("the CA must be marked as one")
	}
	if ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("the CA must be allowed to sign certificates")
	}
	if len(ca.DNSNames) != 0 {
		t.Error("the CA is an anchor, not a server certificate; it needs no names")
	}
}

// shortLivedCA re-signs a minted CA's own key with a near-term expiry, so a
// test can watch the leaf cap take effect without waiting years.
func shortLivedCA(t *testing.T, life time.Duration) (Material, error) {
	t.Helper()
	m, err := New(gatewayDomain)
	if err != nil {
		return Material{}, err
	}
	caKey, err := parseKey(m.CAKeyPEM)
	if err != nil {
		return Material{}, err
	}
	serial, err := serialNumber()
	if err != nil {
		return Material{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: caCommonName},
		NotBefore:             time.Now().Add(-backdate),
		NotAfter:              time.Now().Add(life),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return Material{}, err
	}
	m.CACertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return m, nil
}

// An authority cannot be replaced in one step: the API server holds one anchor,
// and whichever moves first — the anchor or the certificate it anchors —
// verification fails until the other catches up. Trusting both across the
// transition is what removes the window.
func TestBundleTrustsBothAuthoritiesAcrossARollover(t *testing.T) {
	outgoing := mint(t)
	incoming := mint(t)

	bundle := Bundle(outgoing.CACertPEM, incoming.CACertPEM)
	if !SignedByAny(outgoing.LeafCertPEM, bundle) {
		t.Error("a leaf signed by the outgoing authority must still verify during the rollover")
	}
	if !SignedByAny(incoming.LeafCertPEM, bundle) {
		t.Error("a leaf signed by the incoming authority must verify during the rollover")
	}

	// And once contracted, only the incoming one is trusted.
	contracted := Bundle(incoming.CACertPEM, nil)
	if SignedByAny(outgoing.LeafCertPEM, contracted) {
		t.Error("after contracting, a leaf from the retired authority must no longer verify")
	}
	if !SignedByAny(incoming.LeafCertPEM, contracted) {
		t.Error("the surviving authority must still verify its own leaf")
	}

	// A third party's leaf is never trusted at any point.
	stranger := mint(t)
	if SignedByAny(stranger.LeafCertPEM, bundle) {
		t.Error("a bundle must not trust an authority it does not contain")
	}
}

// The installer writes this file and the reconciler hashes it. Identical inputs
// must produce identical bytes or the API server's active-config check can never
// match.
func TestBundleIsByteCanonical(t *testing.T) {
	a := mint(t)
	b := mint(t)

	first := Bundle(a.CACertPEM, b.CACertPEM)
	second := Bundle(a.CACertPEM, b.CACertPEM)
	if string(first) != string(second) {
		t.Error("the same inputs must render the same bytes")
	}

	// Order is part of the canonical form, so the two directions differ.
	if string(Bundle(a.CACertPEM, b.CACertPEM)) == string(Bundle(b.CACertPEM, a.CACertPEM)) {
		t.Error("active and retained are distinct positions")
	}

	// Padding on the way in must not change the result.
	padded := append(append([]byte("\n"), a.CACertPEM...), '\n', '\n')
	if string(Bundle(padded, nil)) != string(Bundle(a.CACertPEM, nil)) {
		t.Error("surrounding whitespace must be normalised away, or the hash moves for no reason")
	}

	// An absent second authority is the steady state, not an empty entry.
	steady := Bundle(a.CACertPEM, nil)
	if _, err := ParseCert(steady); err != nil {
		t.Errorf("a single-authority bundle must still be one parseable certificate: %v", err)
	}
	if string(Bundle(nil, nil)) != "" {
		t.Error("no authorities is an empty bundle, which renders as no anchor at all")
	}
}
