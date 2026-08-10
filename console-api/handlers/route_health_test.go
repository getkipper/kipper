package handlers

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// testIngressNamespace is the namespace every test in this package uses
// for Ingress fixtures. Kept as a constant so the routeHealth tests don't
// drift apart over time.
const testIngressNamespace = "team-test"

// selfSignedCertFor returns a PEM-encoded self-signed certificate whose
// SANs list the given hostnames. Tests use it to populate fake TLS
// secrets that routeHealth's hostname check can verify.
func selfSignedCertFor(t *testing.T, hosts ...string) []byte {
	t.Helper()
	return selfSignedCertForValidity(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), hosts...)
}

func selfSignedCertForValidity(t *testing.T, notBefore, notAfter time.Time, hosts ...string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: hosts[0]},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		DNSNames:     hosts,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func newTestIngress(name, host, tlsSecret string) *networkingv1.Ingress {
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testIngressNamespace,
			Labels:    map[string]string{kipperLabel: kipperValue},
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: host}},
		},
	}
	if tlsSecret != "" {
		ing.Spec.TLS = []networkingv1.IngressTLS{{
			Hosts:      []string{host},
			SecretName: tlsSecret,
		}}
	}
	return ing
}

func TestRouteHealth_IngressMissing(t *testing.T) {
	client := fake.NewClientset()

	got := routeHealth(context.Background(), client, testIngressNamespace, "web", "web.example.com")

	assert.False(t, got.IngressReady)
	assert.False(t, got.TLSReady)
	assert.Contains(t, got.Message, "web.example.com")
}

func TestRouteHealth_NoTLS(t *testing.T) {
	ing := newTestIngress("web", "web.example.com", "")
	client := fake.NewClientset(ing)

	got := routeHealth(context.Background(), client, testIngressNamespace, "web", "web.example.com")

	assert.True(t, got.IngressReady)
	assert.False(t, got.TLSReady)
	assert.Contains(t, got.Message, "HTTP only")
}

func TestRouteHealth_TLSSecretMissing(t *testing.T) {
	ing := newTestIngress("web", "web.example.com", "web-tls")
	client := fake.NewClientset(ing)

	got := routeHealth(context.Background(), client, testIngressNamespace, "web", "web.example.com")

	assert.True(t, got.IngressReady)
	assert.False(t, got.TLSReady)
	assert.Contains(t, got.Message, "Waiting")
}

func TestRouteHealth_TLSSecretEmpty(t *testing.T) {
	ing := newTestIngress("web", "web.example.com", "web-tls")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "web-tls", Namespace: testIngressNamespace},
		Data:       map[string][]byte{},
	}
	client := fake.NewClientset(ing, secret)

	got := routeHealth(context.Background(), client, testIngressNamespace, "web", "web.example.com")

	assert.True(t, got.IngressReady)
	assert.False(t, got.TLSReady)
	assert.Contains(t, got.Message, "provisioning")
}

func TestRouteHealth_TLSReady(t *testing.T) {
	ing := newTestIngress("web", "web.example.com", "web-tls")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "web-tls", Namespace: testIngressNamespace},
		Data:       map[string][]byte{"tls.crt": selfSignedCertFor(t, "web.example.com")},
	}
	client := fake.NewClientset(ing, secret)

	got := routeHealth(context.Background(), client, testIngressNamespace, "web", "web.example.com")

	assert.True(t, got.IngressReady)
	assert.True(t, got.TLSReady)
	assert.Contains(t, got.Message, "active")
}

func TestRouteHealth_NilClient(t *testing.T) {
	got := routeHealth(context.Background(), nil, testIngressNamespace, "web", "web.example.com")
	assert.False(t, got.IngressReady)
	assert.False(t, got.TLSReady)
	assert.Empty(t, got.Message)
}

// After a host change, the existing Ingress (named after the app) still
// has the OLD host until the reconciler catches up. Health for the NEW
// host must report not-yet-ready instead of pretending the Ingress is
// already serving it.
func TestRouteHealth_StaleIngressHostIsNotReady(t *testing.T) {
	ing := newTestIngress("web", "old.example.com", "web-tls")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "web-tls", Namespace: testIngressNamespace},
		Data:       map[string][]byte{"tls.crt": selfSignedCertFor(t, "old.example.com")},
	}
	client := fake.NewClientset(ing, secret)

	got := routeHealth(context.Background(), client, testIngressNamespace, "web", "new.example.com")

	assert.False(t, got.IngressReady)
	assert.False(t, got.TLSReady)
	assert.Contains(t, got.Message, "new.example.com")
}

// An expired certificate in the TLS secret must NOT be reported as ready
// even though it covers the host — clients will fail validation.
func TestRouteHealth_ExpiredCertIsNotReady(t *testing.T) {
	ing := newTestIngress("web", "web.example.com", "web-tls")
	expired := selfSignedCertForValidity(t,
		time.Now().Add(-2*time.Hour),
		time.Now().Add(-time.Hour),
		"web.example.com",
	)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "web-tls", Namespace: testIngressNamespace},
		Data:       map[string][]byte{"tls.crt": expired},
	}
	client := fake.NewClientset(ing, secret)

	got := routeHealth(context.Background(), client, testIngressNamespace, "web", "web.example.com")

	assert.True(t, got.IngressReady)
	assert.False(t, got.TLSReady)
}

// Even when the Ingress has been updated to the new host, cert-manager
// may not yet have re-issued the certificate. The TLS secret still holds
// the OLD cert. Health must report TLS not-yet-ready for the new host
// rather than treating any non-empty cert as active.
func TestRouteHealth_StaleCertForNewHost(t *testing.T) {
	ing := newTestIngress("web", "new.example.com", "web-tls")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "web-tls", Namespace: testIngressNamespace},
		Data:       map[string][]byte{"tls.crt": selfSignedCertFor(t, "old.example.com")},
	}
	client := fake.NewClientset(ing, secret)

	got := routeHealth(context.Background(), client, testIngressNamespace, "web", "new.example.com")

	assert.True(t, got.IngressReady)
	assert.False(t, got.TLSReady)
	assert.Contains(t, got.Message, "new.example.com")
}

// Shared-host route group: frontend has its Ingress (and cert), api does
// not yet. Looking up api's health must report "missing" even though the
// frontend Ingress on the same host is fully ready.
func TestRouteHealth_SharedHostReportsPerApp(t *testing.T) {
	host := "team.kipper.run"
	frontendIng := newTestIngress("frontend", host, "frontend-tls")
	frontendSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend-tls", Namespace: testIngressNamespace},
		Data:       map[string][]byte{"tls.crt": selfSignedCertFor(t, host)},
	}
	client := fake.NewClientset(frontendIng, frontendSecret)

	frontendHealth := routeHealth(context.Background(), client, testIngressNamespace, "frontend", host)
	assert.True(t, frontendHealth.IngressReady)
	assert.True(t, frontendHealth.TLSReady)

	apiHealth := routeHealth(context.Background(), client, testIngressNamespace, "api", host)
	assert.False(t, apiHealth.IngressReady)
	assert.False(t, apiHealth.TLSReady)
}
