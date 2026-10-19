package doctor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"time"

	"github.com/getkipper/kipper/controller/pkg/spki"
)

const traefikFallbackCommonName = "TRAEFIK DEFAULT CERT"

// dialHop returns the leaf certificate addr serves for serverName, without
// verifying it: the hop certificate is self-signed and identified by its key.
var dialHop = func(ctx context.Context, addr, serverName string) (*x509.Certificate, error) {
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second},
		Config:    &tls.Config{ServerName: serverName, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // the key is compared below instead
	}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, errors.New("no certificate presented")
	}
	return certs[0], nil
}

var httpClient = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// CheckServedCertificate dials host:443 the way the kipper.run gateway does,
// with serverName as SNI, and compares the key it serves with wantKeys, the
// SPKI fingerprints of the hop certificate and any rotation candidate. It fails
// on a handshake error, Traefik fallback certificate or key mismatch. Empty
// wantKeys produces a warning unless the handshake or fallback check fails.
func CheckServedCertificate(ctx context.Context, host, serverName string, wantKeys []string) Result {
	const id, label = "served-cert", "Served certificate"
	leaf, err := dialHop(ctx, net.JoinHostPort(host, "443"), serverName)
	if err != nil {
		return Result{ID: id, Label: label, Status: Fail, Message: "the server does not complete a TLS handshake on port 443", Detail: err.Error()}
	}
	if leaf.Subject.CommonName == traefikFallbackCommonName {
		return Result{ID: id, Label: label, Status: Fail,
			Message: "port 443 serves Traefik's built-in fallback certificate, not the hop certificate. " +
				"The gateway refuses any other key, so visitors get an error page. Traefik has not loaded the default TLSStore."}
	}
	if len(wantKeys) == 0 {
		return Result{ID: id, Label: label, Status: Warn, Message: "port 443 answers, but there was no hop certificate to compare it with"}
	}
	if slices.Contains(wantKeys, spki.Fingerprint(leaf)) {
		return Result{ID: id, Label: label, Status: Pass, Message: "port 443 serves the hop certificate"}
	}
	return Result{ID: id, Label: label, Status: Fail,
		Message: "port 443 serves a different key than the hop certificate, so the gateway refuses the connection",
		Detail:  fmt.Sprintf("served: %s, issuer: %s", leaf.Subject.CommonName, leaf.Issuer.CommonName)}
}

// CheckGateway passes only when healthURL returns HTTP 200, without following
// redirects. Request errors and other status codes fail the check.
func CheckGateway(ctx context.Context, healthURL string) Result {
	const id, label = "gateway", "kipper.run gateway"
	code, err := get(ctx, healthURL)
	if err != nil {
		return Result{ID: id, Label: label, Status: Fail, Message: "could not reach the kipper.run gateway", Detail: err.Error()}
	}
	if code != http.StatusOK {
		return Result{ID: id, Label: label, Status: Fail, Message: fmt.Sprintf("the kipper.run gateway answered %d", code)}
	}
	return Result{ID: id, Label: label, Status: Pass, Message: "the kipper.run gateway is up"}
}

// CheckRoute requests url through the gateway and passes only on a 2xx response,
// without following redirects or checking the body. Use an endpoint that returns
// 200 on a healthy cluster so failures help diagnose the route. Request errors
// and other status codes fail the check.
func CheckRoute(ctx context.Context, url string) Result {
	const id, label = "route", "Route through the gateway"
	code, err := get(ctx, url)
	switch {
	case err != nil:
		return Result{ID: id, Label: label, Status: Fail, Message: "could not reach the cluster through the gateway", Detail: err.Error()}
	case code == http.StatusNotFound:
		return Result{ID: id, Label: label, Status: Fail,
			Message: "the gateway is not routing this name: it is unregistered, lapsed, or has not proven control (see the heartbeat)"}
	case code == http.StatusBadGateway || code == http.StatusGatewayTimeout:
		return Result{ID: id, Label: label, Status: Fail,
			Message: fmt.Sprintf("the gateway routes this name but cannot reach the cluster (%d, see the served certificate)", code)}
	case code < 200 || code > 299:
		return Result{ID: id, Label: label, Status: Fail, Message: fmt.Sprintf("the route through the gateway answered %d instead of Dex's discovery document", code)}
	}
	return Result{ID: id, Label: label, Status: Pass, Message: "the gateway routes this name to the cluster"}
}

func get(ctx context.Context, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
