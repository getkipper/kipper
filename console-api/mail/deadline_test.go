package mail

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A mail server that accepts the connection and then stops answering is an
// ordinary failure, and it used to hang the delivery goroutine for good: the
// plain path had no timeout at all, and the TLS path bounded only the dial. The
// alert batch behind the stuck send was never delivered, and every later batch
// left another blocked goroutine.
//
// This branch made SMTP the only route off the cluster when no webhook is set,
// so that is the alert path itself hanging.

// silentServer accepts connections, sends a greeting, and then says nothing. It
// speaks TLS first where asked, so the TLS path reaches the SMTP conversation
// rather than failing in the handshake and never exercising the deadline.
func silentServer(t *testing.T, useTLS bool) (addr string, stop func()) {
	t.Helper()
	return greetingServerTLS(t, "", useTLS)
}

// greetingServer answers EHLO with the given capability lines and then goes
// quiet.
func greetingServer(t *testing.T, ehlo string) (addr string, stop func()) {
	t.Helper()
	return greetingServerTLS(t, ehlo, false)
}

func greetingServerTLS(t *testing.T, ehlo string, useTLS bool) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var tlsCfg *tls.Config
	if useTLS {
		tlsCfg = &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	}

	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSilently(conn, tlsCfg, ehlo, quit)
		}
	}()

	return ln.Addr().String(), func() { close(quit); _ = ln.Close(); <-done }
}

func serveSilently(conn net.Conn, tlsCfg *tls.Config, ehlo string, quit <-chan struct{}) {
	defer func() { _ = conn.Close() }()

	if tlsCfg != nil {
		tlsConn := tls.Server(conn, tlsCfg)
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		conn = tlsConn
	}

	if _, err := conn.Write([]byte("220 silent.example.com ESMTP\r\n")); err != nil {
		return
	}
	if ehlo == "" {
		<-quit
		return
	}

	// Answer the EHLO and then go quiet, so the client reaches the transaction
	// with the capabilities this server claims.
	buf := make([]byte, 512)
	if _, err := conn.Read(buf); err != nil {
		return
	}
	_, _ = conn.Write([]byte(ehlo))
	<-quit
}

// selfSigned is a throwaway certificate for the in-process server. The client
// skips verification against it, because what is under test is the deadline
// rather than the chain.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestSendGivesUpOnASilentServer(t *testing.T) {
	shortTimeout(t)

	for _, useTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%v", useTLS), func(t *testing.T) {
			addr, stop := silentServer(t, useTLS)
			t.Cleanup(stop)
			host, port := splitHostPort(t, addr)

			done := make(chan error, 1)
			go func() {
				done <- Send(context.Background(), Config{
					Host: host, Port: port, From: "kipper@example.com", TLS: useTLS,
					insecureSkipVerify: true,
				}, "ops@example.com", "subject", "<p>body</p>")
			}()

			select {
			case err := <-done:
				assert.Error(t, err, "reported success against a server that never answered")
			case <-time.After(10 * time.Second):
				t.Fatal("still waiting well past the deadline; the send is not bounded")
			}
		})
	}
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("splitting %q: %v", addr, err)
	}
	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	return host, port
}

// smtp.SendMail errors when credentials are supplied and the server does not
// offer AUTH. Skipping authentication instead sends the mail anonymously, which
// is the operator's explicit configuration being ignored, and on a cleartext
// link it is what an AUTH-stripping attacker would engineer.
func TestSendRefusesToDropAuthenticationSilently(t *testing.T) {
	shortTimeout(t)
	addr, stop := greetingServer(t, "250-noauth.example.com\r\n250 SIZE 10240000\r\n")
	defer stop()
	host, port := splitHostPort(t, addr)

	err := Send(context.Background(), Config{
		Host: host, Port: port, From: "kipper@example.com",
		Username: "kipper", Password: "secret",
	}, "ops@example.com", "subject", "<p>body</p>")

	require.Error(t, err, "credentials were configured and the server offers no way to use them")
	assert.Contains(t, err.Error(), "AUTH")
}

// A relay that accepts mail without credentials is an ordinary configuration,
// and one with no username set must still work against it.
func TestSendWorksAgainstAnAnonymousRelay(t *testing.T) {
	shortTimeout(t)
	addr, stop := greetingServer(t, "250-noauth.example.com\r\n250 SIZE 10240000\r\n")
	defer stop()
	host, port := splitHostPort(t, addr)

	err := Send(context.Background(), Config{Host: host, Port: port, From: "kipper@example.com"},
		"ops@example.com", "subject", "<p>body</p>")

	// The fake server answers no further commands, so this fails somewhere
	// after EHLO. What matters is that it was not refused for want of AUTH.
	if err != nil {
		assert.NotContains(t, err.Error(), "AUTH",
			"no credentials were configured, so AUTH support is not required")
	}
}

// The caller's deadline is the shorter one during an alert batch, and it has to
// reach the socket. Without it a batch of 25 alerts to 3 admins against a silent
// relay runs for 75 minutes rather than the 20 seconds the caller allowed.
func TestSendHonoursTheCallersDeadline(t *testing.T) {
	addr, stop := silentServer(t, false)
	defer stop()
	host, port := splitHostPort(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := Send(ctx, Config{Host: host, Port: port, From: "kipper@example.com"},
		"ops@example.com", "subject", "<p>body</p>")

	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second,
		"the caller allowed 200ms; the transport's own minute must not override it")
}

// shortTimeout makes the transport's own bound observable in a test without
// waiting the minute production allows.
func shortTimeout(t *testing.T) {
	t.Helper()
	restore := transactionTimeout
	transactionTimeout = 300 * time.Millisecond
	t.Cleanup(func() { transactionTimeout = restore })
}
