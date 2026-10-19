package doctor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/controller/pkg/spki"
)

// tlsServer serves a certificate with the given common name and returns its
// address and SPKI fingerprint.
func tlsServer(t *testing.T, commonName string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: commonName},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), spki.Fingerprint(leaf)
}

// dialVia points the probe's dialer at addr while keeping its SNI.
func dialVia(t *testing.T, addr string) {
	t.Helper()
	prev := dialHop
	dialHop = func(ctx context.Context, _ string, serverName string) (*x509.Certificate, error) {
		return prev(ctx, addr, serverName)
	}
	t.Cleanup(func() { dialHop = prev })
}

func TestCheckServedCertificate_PassesForTheHopKey(t *testing.T) {
	addr, fp := tlsServer(t, "kipper hop")
	dialVia(t, addr)

	r := CheckServedCertificate(context.Background(), "203.0.113.10", "lab.kipper.run", []string{fp})
	assert.Equal(t, Pass, r.Status)
}

func TestCheckServedCertificate_NamesTraefiksFallbackCertificate(t *testing.T) {
	addr, _ := tlsServer(t, "TRAEFIK DEFAULT CERT")
	dialVia(t, addr)

	r := CheckServedCertificate(context.Background(), "203.0.113.10", "lab.kipper.run", []string{"aaaa"})
	assert.Equal(t, Fail, r.Status)
	assert.Contains(t, r.Message, "Traefik's built-in fallback certificate")
	assert.Contains(t, r.Message, "TLSStore")
}

func TestCheckServedCertificate_FailsForAnotherKey(t *testing.T) {
	addr, _ := tlsServer(t, "something else")
	dialVia(t, addr)

	r := CheckServedCertificate(context.Background(), "203.0.113.10", "lab.kipper.run", []string{"aaaa"})
	assert.Equal(t, Fail, r.Status)
	assert.Contains(t, r.Message, "different key")
	assert.NotContains(t, r.Message, "aaaa", "fingerprints stay out of the message")
}

func TestCheckServedCertificate_WarnsWhenThereIsNothingToCompareWith(t *testing.T) {
	addr, _ := tlsServer(t, "kipper hop")
	dialVia(t, addr)

	r := CheckServedCertificate(context.Background(), "203.0.113.10", "lab.kipper.run", nil)
	assert.Equal(t, Warn, r.Status)
}

func TestCheckServedCertificate_FailsWhenNothingAnswers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	dialVia(t, addr)

	r := CheckServedCertificate(context.Background(), "203.0.113.10", "lab.kipper.run", []string{"aaaa"})
	assert.Equal(t, Fail, r.Status)
	assert.Contains(t, r.Message, "port 443")
	assert.NotContains(t, r.Message, "127.0.0.1")
}

func statusServer(t *testing.T, code int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestCheckGateway(t *testing.T) {
	assert.Equal(t, Pass, CheckGateway(context.Background(), statusServer(t, http.StatusOK)).Status)
	assert.Equal(t, Fail, CheckGateway(context.Background(), statusServer(t, http.StatusServiceUnavailable)).Status)
}

func TestCheckRoute(t *testing.T) {
	assert.Equal(t, Pass, CheckRoute(context.Background(), statusServer(t, http.StatusOK)).Status)
	for _, code := range []int{http.StatusUnauthorized, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		r := CheckRoute(context.Background(), statusServer(t, code))
		assert.Equal(t, Fail, r.Status, "status %d does not prove the route", code)
	}

	notRouted := CheckRoute(context.Background(), statusServer(t, http.StatusNotFound))
	assert.Equal(t, Fail, notRouted.Status)
	assert.Contains(t, notRouted.Message, "not routing")

	refused := CheckRoute(context.Background(), statusServer(t, http.StatusBadGateway))
	assert.Equal(t, Fail, refused.Status)
	assert.Contains(t, refused.Message, "cannot reach the cluster")
}

func TestCheckRoute_ReportsAnUnreachableGateway(t *testing.T) {
	r := CheckRoute(context.Background(), "https://127.0.0.1:1")
	assert.Equal(t, Fail, r.Status)
	assert.Contains(t, r.Message, "could not reach")
}
