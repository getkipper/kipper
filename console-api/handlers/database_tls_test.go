package handlers

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

const dbServerName = "db.shop-prod.svc.cluster.local"

// testCA is a certificate authority that issues server certificates for the
// fake Postgres server.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test service CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca *testCA) issue(t *testing.T, dnsName string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{dnsName},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// fakePostgres answers the Postgres SSLRequest. With a certificate it accepts
// TLS and counts completed handshakes. Without one it refuses TLS. Any startup
// message sent without TLS is counted as a plaintext connection.
type fakePostgres struct {
	addr       *net.TCPAddr
	handshakes atomic.Int32
	plaintext  atomic.Int32
	accepted   atomic.Int32
}

const sslRequestCode = 80877103

func startFakePostgres(t *testing.T, cert *tls.Certificate) *fakePostgres {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	f := &fakePostgres{addr: ln.Addr().(*net.TCPAddr)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.accepted.Add(1)
			go f.serve(conn, cert)
		}
	}()
	return f
}

func (f *fakePostgres) serve(conn net.Conn, cert *tls.Certificate) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	head := make([]byte, 8)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}
	if binary.BigEndian.Uint32(head[4:]) != sslRequestCode {
		f.plaintext.Add(1)
		return
	}
	if cert == nil {
		_, _ = conn.Write([]byte{'N'})
		if _, err := io.ReadFull(conn, head[:4]); err == nil {
			f.plaintext.Add(1)
		}
		return
	}
	_, _ = conn.Write([]byte{'S'})
	server := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12})
	if err := server.Handshake(); err == nil {
		f.handshakes.Add(1)
	}
}

// databaseFor builds a Database handler for Postgres service "db" in
// "shop-prod" that connects to server. A nil caPEM omits the TLS Secret.
func databaseFor(t *testing.T, mode string, server *fakePostgres, caPEM []byte, tlsFor string) *Database {
	t.Helper()
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop-prod"},
		Spec:       kipperv1.ServiceSpec{Type: "postgres", TLS: mode},
	}
	objects := []k8sruntime.Object{&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretname.ServiceCredentials("db"), Namespace: "shop-prod"},
		Data: map[string][]byte{
			"HOST": []byte("127.0.0.1"), "PORT": []byte(strconv.Itoa(server.addr.Port)),
			"USERNAME": []byte("app"), "PASSWORD": []byte("not-a-real-password"), "NAME": []byte("app"),
		},
	}}
	if caPEM != nil {
		objects = append(objects, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "db-tls", Namespace: "shop-prod",
				Labels:      map[string]string{labels.TLSFor: tlsFor},
				Annotations: map[string]string{"cert-manager.io/certificate-name": "db-tls"},
			},
			Data: map[string][]byte{"ca.crt": caPEM, "tls.crt": []byte("x"), "tls.key": []byte("x")},
		})
	}
	return &Database{Client: fake.NewClientset(objects...), CRClient: testCRClient(svc)}
}

func connectToDB(d *Database) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := d.connect(ctx, "db", "shop-prod", "")
	return err
}

func TestTheDatabaseViewVerifiesARequiredServiceWithItsCA(t *testing.T) {
	ca := newTestCA(t)
	cert := ca.issue(t, dbServerName)
	server := startFakePostgres(t, &cert)

	_ = connectToDB(databaseFor(t, "required", server, ca.pem, "db"))

	assert.Equal(t, int32(1), server.handshakes.Load(), "the handshake with a certificate from the service CA must succeed")
	assert.Zero(t, server.plaintext.Load())
}

func TestTheDatabaseViewRefusesACertificateFromAnotherCA(t *testing.T) {
	ca, other := newTestCA(t), newTestCA(t)
	cert := other.issue(t, dbServerName)
	server := startFakePostgres(t, &cert)

	err := connectToDB(databaseFor(t, "on", server, ca.pem, "db"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "certificate")
	assert.Zero(t, server.handshakes.Load())
	assert.Zero(t, server.plaintext.Load())
}

func TestTheDatabaseViewRefusesACertificateForAnotherName(t *testing.T) {
	ca := newTestCA(t)
	cert := ca.issue(t, "other.shop-prod.svc.cluster.local")
	server := startFakePostgres(t, &cert)

	err := connectToDB(databaseFor(t, "required", server, ca.pem, "db"))

	require.Error(t, err)
	assert.Zero(t, server.handshakes.Load())
}

func TestTheDatabaseViewNeverFallsBackToPlaintextForATLSService(t *testing.T) {
	ca := newTestCA(t)
	for _, mode := range []string{"on", "required"} {
		server := startFakePostgres(t, nil)

		err := connectToDB(databaseFor(t, mode, server, ca.pem, "db"))

		require.Error(t, err, mode)
		assert.Zero(t, server.plaintext.Load(), mode)
	}
}

func TestTheDatabaseViewRefusesATLSServiceWithoutAnIssuedCA(t *testing.T) {
	for name, tc := range map[string]struct {
		caPEM  []byte
		tlsFor string
	}{
		"no TLS Secret":           {nil, ""},
		"Secret of other service": {newTestCA(t).pem, "other"},
		"no usable CA":            {[]byte("not a certificate"), "db"},
	} {
		server := startFakePostgres(t, nil)

		err := connectToDB(databaseFor(t, "required", server, tc.caPEM, tc.tlsFor))

		require.Error(t, err, name)
		assert.Zero(t, server.accepted.Load(), "%s: nothing may be sent to the server", name)
	}
}

func TestTheDatabaseViewPrefersTLSForAServiceWithTLSOff(t *testing.T) {
	for _, mode := range []string{"", "off"} {
		server := startFakePostgres(t, nil)

		_ = connectToDB(databaseFor(t, mode, server, nil, ""))

		assert.Positive(t, server.plaintext.Load(), "mode %q falls back to plaintext", mode)
	}
}
