package hopcert

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/getkipper/kipper/controller/pkg/hopca"
	"github.com/getkipper/kipper/controller/pkg/hostnames"
	"github.com/getkipper/kipper/controller/pkg/spki"
)

var fingerprintShape = regexp.MustCompile(`^[0-9a-f]{64}$`)

func newCRClient(t *testing.T, objs ...crclient.Object) crclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return crfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func getStore(t *testing.T, cr crclient.Client, namespace string) *unstructured.Unstructured {
	t.Helper()
	store := &unstructured.Unstructured{}
	store.SetGroupVersionKind(tlsStoreGVK)
	if err := cr.Get(context.Background(), crclient.ObjectKey{Namespace: namespace, Name: tlsStoreName}, store); err != nil {
		t.Fatalf("get TLSStore: %v", err)
	}
	return store
}

func TestEnsureProvisionsSecretAndStore(t *testing.T) {
	client := fake.NewSimpleClientset()
	cr := newCRClient(t)

	state, err := Ensure(context.Background(), client, cr)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !fingerprintShape.MatchString(state.Fingerprint) {
		t.Errorf("expected an SPKI fingerprint, got %q", state.Fingerprint)
	}
	if state.CandidateFingerprint != "" || len(state.ForeignStores) != 0 {
		t.Errorf("fresh cluster: expected no candidate and no foreign stores, got %+v", state)
	}

	secret, err := client.CoreV1().Secrets(Namespace).Get(context.Background(), SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if secret.Type != corev1.SecretTypeTLS {
		t.Errorf("expected a kubernetes.io/tls secret, got %s", secret.Type)
	}

	store := getStore(t, cr, Namespace)
	secretName, _, _ := unstructured.NestedString(store.Object, "spec", "defaultCertificate", "secretName")
	if secretName != SecretName {
		t.Errorf("TLSStore must serve the hop secret, got %q", secretName)
	}

	// A second run changes nothing: same key, same fingerprint.
	again, err := Ensure(context.Background(), client, cr)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if again.Fingerprint != state.Fingerprint {
		t.Errorf("ensure must be idempotent: fingerprint changed %s → %s", state.Fingerprint, again.Fingerprint)
	}
}

// shortLivedSecret builds a hop secret whose certificate expires within the
// reissue window.
func shortLivedSecret(t *testing.T) *corev1.Secret {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "kipper-hop"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: Namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
			corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		},
	}
}

func TestEnsureReissuesNearExpiryKeepingKey(t *testing.T) {
	old := shortLivedSecret(t)
	client := fake.NewSimpleClientset(old)
	cr := newCRClient(t)

	before, err := hopca.ParseCert(old.Data[corev1.TLSCertKey])
	if err != nil {
		t.Fatal(err)
	}
	state, err := Ensure(context.Background(), client, cr)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}

	secret, _ := client.CoreV1().Secrets(Namespace).Get(context.Background(), SecretName, metav1.GetOptions{})
	after, err := hopca.ParseCert(secret.Data[corev1.TLSCertKey])
	if err != nil {
		t.Fatal(err)
	}
	if !after.NotAfter.After(before.NotAfter.Add(time.Hour)) {
		t.Errorf("expected the certificate reissued with fresh validity, got %v", after.NotAfter)
	}
	// Same key, so the pin the gateway enforces is untouched.
	if state.Fingerprint != spki.Fingerprint(before) {
		t.Errorf("a reissue must not change the SPKI fingerprint: %s → %s", spki.Fingerprint(before), state.Fingerprint)
	}
}

func TestEnsureStagesRotationOnAnnotation(t *testing.T) {
	client := fake.NewSimpleClientset()
	cr := newCRClient(t)
	first, err := Ensure(context.Background(), client, cr)
	if err != nil {
		t.Fatal(err)
	}

	secret, _ := client.CoreV1().Secrets(Namespace).Get(context.Background(), SecretName, metav1.GetOptions{})
	secret.Annotations = map[string]string{RotateAnnotation: "requested"}
	if _, err := client.CoreV1().Secrets(Namespace).Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	staged, err := Ensure(context.Background(), client, cr)
	if err != nil {
		t.Fatalf("ensure with rotation requested: %v", err)
	}
	if staged.CandidateFingerprint == "" || staged.CandidateFingerprint == first.Fingerprint {
		t.Errorf("expected a fresh candidate fingerprint, got %q", staged.CandidateFingerprint)
	}
	if staged.Fingerprint != first.Fingerprint {
		t.Errorf("staging must not touch the live keypair: %s → %s", first.Fingerprint, staged.Fingerprint)
	}

	secret, _ = client.CoreV1().Secrets(Namespace).Get(context.Background(), SecretName, metav1.GetOptions{})
	if _, still := secret.Annotations[RotateAnnotation]; still {
		t.Error("the rotation annotation must be consumed when the candidate is staged")
	}

	// Re-running does not restage a different candidate.
	again, err := Ensure(context.Background(), client, cr)
	if err != nil {
		t.Fatal(err)
	}
	if again.CandidateFingerprint != staged.CandidateFingerprint {
		t.Errorf("a staged candidate must be stable across runs: %s → %s", staged.CandidateFingerprint, again.CandidateFingerprint)
	}
}

func TestPromoteCandidate(t *testing.T) {
	client := fake.NewSimpleClientset()
	cr := newCRClient(t)
	if _, err := Ensure(context.Background(), client, cr); err != nil {
		t.Fatal(err)
	}

	// Idempotent with nothing staged.
	if err := PromoteCandidate(context.Background(), client); err != nil {
		t.Fatalf("promote without candidate: %v", err)
	}

	secret, _ := client.CoreV1().Secrets(Namespace).Get(context.Background(), SecretName, metav1.GetOptions{})
	secret.Annotations = map[string]string{RotateAnnotation: "requested"}
	_, _ = client.CoreV1().Secrets(Namespace).Update(context.Background(), secret, metav1.UpdateOptions{})
	staged, err := Ensure(context.Background(), client, cr)
	if err != nil {
		t.Fatal(err)
	}

	if err := PromoteCandidate(context.Background(), client); err != nil {
		t.Fatalf("promote: %v", err)
	}
	promoted, err := Ensure(context.Background(), client, cr)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Fingerprint != staged.CandidateFingerprint {
		t.Errorf("expected the candidate to be live after promotion, got %s want %s", promoted.Fingerprint, staged.CandidateFingerprint)
	}
	if promoted.CandidateFingerprint != "" {
		t.Error("promotion must clear the staged candidate")
	}
}

func TestEnsureReportsForeignDefaultStores(t *testing.T) {
	foreign := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1",
		"kind":       "TLSStore",
		"metadata":   map[string]any{"name": "default", "namespace": "tenant-a"},
		"spec":       map[string]any{"defaultCertificate": map[string]any{"secretName": "their-cert"}},
	}}
	client := fake.NewSimpleClientset()
	cr := newCRClient(t, foreign)

	state, err := Ensure(context.Background(), client, cr)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(state.ForeignStores) != 1 || state.ForeignStores[0] != "tenant-a/default" {
		t.Errorf("expected the foreign default store reported, got %v", state.ForeignStores)
	}
}

func TestEnsureRepairsHijackedStore(t *testing.T) {
	hijacked := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1",
		"kind":       "TLSStore",
		"metadata":   map[string]any{"name": "default", "namespace": Namespace},
		"spec":       map[string]any{"defaultCertificate": map[string]any{"secretName": "something-else"}},
	}}
	client := fake.NewSimpleClientset()
	cr := newCRClient(t, hijacked)

	if _, err := Ensure(context.Background(), client, cr); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	store := getStore(t, cr, Namespace)
	secretName, _, _ := unstructured.NestedString(store.Object, "spec", "defaultCertificate", "secretName")
	if secretName != SecretName {
		t.Errorf("expected the managed store repaired to serve %s, got %q", SecretName, secretName)
	}
}

func TestSigningKeyMatchesServedCert(t *testing.T) {
	client := fake.NewSimpleClientset()
	cr := newCRClient(t)
	state, err := Ensure(context.Background(), client, cr)
	if err != nil {
		t.Fatal(err)
	}

	key, err := SigningKey(context.Background(), client)
	if err != nil {
		t.Fatalf("signing key: %v", err)
	}

	// The signing key's public half must be the one served in the cert whose
	// SPKI Ensure returned — otherwise a proof signed with it would not verify
	// against what the gateway dials.
	secret, _ := client.CoreV1().Secrets(Namespace).Get(context.Background(), SecretName, metav1.GetOptions{})
	cert, err := hopca.ParseCert(secret.Data[corev1.TLSCertKey])
	if err != nil {
		t.Fatal(err)
	}
	if spki.Fingerprint(cert) != state.Fingerprint {
		t.Fatal("test setup: cert fingerprint mismatch")
	}
	certPub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatal("served cert is not ECDSA")
	}
	if !key.PublicKey.Equal(certPub) {
		t.Error("the signing key must correspond to the served certificate's public key")
	}
}

func TestSigningKeyErrorsWhenAbsent(t *testing.T) {
	client := fake.NewSimpleClientset()
	if _, err := SigningKey(context.Background(), client); err == nil {
		t.Error("expected an error when the hop secret does not exist")
	}
}

// Every certificate this package writes must be signed under the cluster CA,
// because that CA is what the API server was handed as its trust anchor. The
// reissue path is the dangerous one: it fires on its own when a certificate
// nears expiry, so a self-signed reissue would silently strip the anchor and
// leave operator OIDC broken with nothing in the cluster able to repair it.
func TestReissueStaysSignedByTheClusterCA(t *testing.T) {
	// A leaf already adopted under the CA, but near enough to expiry to reissue.
	client := fake.NewSimpleClientset()
	cr := newCRClient(t)
	if _, err := Ensure(context.Background(), client, cr); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	caSecret, err := client.CoreV1().Secrets(Namespace).Get(context.Background(), CASecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading CA: %v", err)
	}
	caPEM := caSecret.Data[corev1.TLSCertKey]

	live, err := client.CoreV1().Secrets(Namespace).Get(context.Background(), SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading leaf: %v", err)
	}
	if !hopca.SignedBy(live.Data[corev1.TLSCertKey], caPEM) {
		t.Fatal("the first leaf is not signed by the cluster CA")
	}
	// Age it into the reissue window.
	short := shortLivedSecret(t)
	live.Data[corev1.TLSCertKey] = short.Data[corev1.TLSCertKey]
	live.Data[corev1.TLSPrivateKeyKey] = short.Data[corev1.TLSPrivateKeyKey]
	if _, err := client.CoreV1().Secrets(Namespace).Update(context.Background(), live, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	state, err := Ensure(context.Background(), client, cr)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}

	after, err := client.CoreV1().Secrets(Namespace).Get(context.Background(), SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !hopca.SignedBy(after.Data[corev1.TLSCertKey], caPEM) {
		t.Error("the renewed certificate is not signed by the cluster CA, so the API server's anchor no longer verifies it")
	}
	renewed, err := hopca.ParseCert(after.Data[corev1.TLSCertKey])
	if err != nil {
		t.Fatal(err)
	}
	if state.Fingerprint != spki.Fingerprint(renewed) {
		t.Error("the asserted fingerprint does not describe the certificate now served")
	}
	if len(renewed.DNSNames) == 0 {
		t.Error("the renewed certificate carries no name, so the API server cannot verify the host")
	}
}

// A cluster whose leaf predates the CA holds a self-signed certificate with no
// names, which the API server can never verify. Adoption re-signs that same key
// under the CA, so the gateway's pin is untouched and no registration has to be
// re-observed.
func TestEnsureAdoptsALegacySelfSignedLeaf(t *testing.T) {
	legacy := shortLivedSecret(t)
	legacyKey := legacy.Data[corev1.TLSPrivateKeyKey]
	legacyCert, err := hopca.ParseCert(legacy.Data[corev1.TLSCertKey])
	if err != nil {
		t.Fatal(err)
	}
	pinBefore := spki.Fingerprint(legacyCert)

	client := fake.NewSimpleClientset(legacy)
	state, err := Ensure(context.Background(), client, newCRClient(t))
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}

	adopted, err := client.CoreV1().Secrets(Namespace).Get(context.Background(), SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(adopted.Data[corev1.TLSPrivateKeyKey], legacyKey) {
		t.Error("adoption replaced the key, which moves the pin and 502s the cluster until the gateway re-observes it")
	}
	caSecret, err := client.CoreV1().Secrets(Namespace).Get(context.Background(), CASecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !hopca.SignedBy(adopted.Data[corev1.TLSCertKey], caSecret.Data[corev1.TLSCertKey]) {
		t.Error("the legacy leaf was not adopted under the cluster CA")
	}
	if state.Fingerprint != pinBefore {
		t.Errorf("adoption moved the pin: %s -> %s", pinBefore, state.Fingerprint)
	}
}

// The CA is the API server's anchor and only the installer can replace it, so a
// second reconcile — or a second replica — must never mint another one.
func TestEnsureNeverRegeneratesTheCA(t *testing.T) {
	client := fake.NewSimpleClientset()
	cr := newCRClient(t)
	if _, err := Ensure(context.Background(), client, cr); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	first, err := client.CoreV1().Secrets(Namespace).Get(context.Background(), CASecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if _, err := Ensure(context.Background(), client, cr); err != nil {
			t.Fatalf("ensure %d: %v", i+2, err)
		}
	}
	again, err := client.CoreV1().Secrets(Namespace).Get(context.Background(), CASecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.Data[corev1.TLSCertKey], first.Data[corev1.TLSCertKey]) {
		t.Error("the CA was regenerated, so the anchor the API server holds no longer verifies anything")
	}
}

// tls.crt feeds the fingerprint asserted to the gateway. If anything ever
// appended the CA to it, ParseCert would refuse rather than describe whichever
// certificate came first — pinning the CA would 502 every handshake against the
// leaf actually served.
func TestServedCertificateIsNeverAChain(t *testing.T) {
	client := fake.NewSimpleClientset()
	if _, err := Ensure(context.Background(), client, newCRClient(t)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	secret, err := client.CoreV1().Secrets(Namespace).Get(context.Background(), SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hopca.ParseCert(secret.Data[corev1.TLSCertKey]); err != nil {
		t.Errorf("what Traefik serves must be exactly one certificate: %v", err)
	}
}

// A missing authority is only "this cluster never had one" when nothing
// contradicts it. Reading it that way on a cluster that is already serving a
// CA-signed certificate mints a replacement and adopts the leaf under it, which
// puts the cluster on an authority the API server's anchor does not name — every
// operator locked out of the login path, with nothing inside the cluster able to
// hand the API server a new anchor.
func TestADestroyedAuthorityIsNotMistakenForAFreshCluster(t *testing.T) {
	material, err := hopca.New(hostnames.GatewayDomain)
	if err != nil {
		t.Fatalf("minting fixture authority: %v", err)
	}

	// The cluster serves a leaf this authority signed, and the authority's
	// Secret has been destroyed.
	served := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: Namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       material.LeafCertPEM,
			corev1.TLSPrivateKeyKey: material.LeafKeyPEM,
		},
	}
	client := fake.NewSimpleClientset(served)

	if _, err := Ensure(context.Background(), client, newCRClient(t)); err == nil {
		t.Fatal("minting a replacement authority under a served CA-signed leaf must be refused")
	} else if !strings.Contains(err.Error(), "destroyed") {
		t.Errorf("the refusal must say what happened, got: %v", err)
	}

	// Nothing was minted, so the cluster goes on serving what it already serves
	// and an operator with SSH can restore the authority.
	if _, err := client.CoreV1().Secrets(Namespace).Get(context.Background(), CASecretName, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("a replacement authority was created anyway: %v", err)
	}
	after, err := client.CoreV1().Secrets(Namespace).Get(context.Background(), SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the served certificate back: %v", err)
	}
	if !bytes.Equal(after.Data[corev1.TLSCertKey], material.LeafCertPEM) {
		t.Error("the served certificate was re-signed under an authority the API server does not trust")
	}
}

// The migration this path exists for still works: a leaf that predates the
// authority is self-signed, so there is no destroyed authority to protect, and
// it is adopted under a freshly minted one.
func TestALeafPredatingTheAuthorityIsStillAdopted(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("encoding key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "kipper-hop"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("self-signing legacy leaf: %v", err)
	}
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: Namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		},
	}
	client := fake.NewSimpleClientset(legacy)

	if _, err := Ensure(context.Background(), client, newCRClient(t)); err != nil {
		t.Fatalf("a self-signed legacy leaf must still be adopted: %v", err)
	}
	ca, err := client.CoreV1().Secrets(Namespace).Get(context.Background(), CASecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("an authority should have been minted: %v", err)
	}
	after, err := client.CoreV1().Secrets(Namespace).Get(context.Background(), SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the adopted certificate: %v", err)
	}
	if !hopca.SignedBy(after.Data[corev1.TLSCertKey], ca.Data[corev1.TLSCertKey]) {
		t.Error("the legacy leaf was not adopted under the new authority")
	}
}

// Damaged material is not permission to mint. A hop Secret that exists but is
// empty or unreadable is still evidence that this cluster has been serving
// something, and minting a new authority over it leaves the API server trusting
// one the cluster no longer serves. Refusing wrongly stops a reconcile an
// operator can unblock; minting wrongly locks everyone out.
func TestDamagedServedMaterialIsNotPermissionToMint(t *testing.T) {
	tests := []struct {
		name string
		data map[string][]byte
	}{
		{name: "the served certificate is missing", data: map[string][]byte{}},
		{name: "the served certificate is empty", data: map[string][]byte{corev1.TLSCertKey: {}}},
		{name: "the served certificate is not a certificate", data: map[string][]byte{corev1.TLSCertKey: []byte("not a certificate")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: Namespace},
				Type:       corev1.SecretTypeTLS,
				Data:       tt.data,
			})
			if _, err := Ensure(context.Background(), client, newCRClient(t)); err == nil {
				t.Fatal("minting an authority over damaged served material must be refused")
			}
			if _, err := client.CoreV1().Secrets(Namespace).Get(context.Background(), CASecretName, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
				t.Errorf("an authority was minted anyway: %v", err)
			}
		})
	}
}

// A certificate can claim any issuer name it likes, including its own subject.
// The test that decides whether a leaf predates the authority has to be the
// signature, not the names, or a leaf signed by someone else under a matching
// name is read as legacy and a new authority is minted over a live one.
func TestASelfIssuedButNotSelfSignedLeafIsNotTreatedAsLegacy(t *testing.T) {
	impostorKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	// Issuer and subject match, so a name comparison calls this self-signed.
	// It is signed by a different key entirely.
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "kipper-hop"},
		Issuer:       pkix.Name{CommonName: "kipper-hop"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	parent := &x509.Certificate{
		SerialNumber:          big.NewInt(3),
		Subject:               pkix.Name{CommonName: "kipper-hop"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &leafKey.PublicKey, impostorKey)
	if err != nil {
		t.Fatalf("signing the impostor leaf: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatalf("encoding key: %v", err)
	}

	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: Namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		},
	})

	if _, err := Ensure(context.Background(), client, newCRClient(t)); err == nil {
		t.Error("a leaf that only claims to be self-issued must not be read as predating the authority")
	}
}
