package handlers

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// RouteHealth describes the live serving state of a route. It is intended
// for display in the UI alongside the configured spec, and is built from
// what the cluster currently shows — never from a cached status field.
//
// Both the per-app Settings tab and the cluster-wide Routes page render
// this same struct, so the two views can never disagree about whether a
// route is live.
type RouteHealth struct {
	// IngressReady is true once the App reconciler has created the
	// Ingress for the configured host.
	IngressReady bool `json:"ingress_ready"`

	// TLSReady is true once cert-manager has populated the TLS Secret
	// referenced by the Ingress with a valid certificate.
	TLSReady bool `json:"tls_ready"`

	// Message is a short, human-readable status line for the UI.
	Message string `json:"message,omitempty"`
}

// routeHealth returns the live health of an app's route. It looks up the
// Ingress named after the app — which is exactly what the App reconciler
// creates — so per-app status stays accurate even when several apps share
// a host (route group). The host argument is used only for human-readable
// messaging.
//
// Nil or empty arguments yield a zero-valued health (everything false,
// empty message) — callers should treat that as "unknown" rather than as
// a definitive negative.
func routeHealth(ctx context.Context, kubeClient kubernetes.Interface, namespace, appName, host string) RouteHealth {
	if kubeClient == nil || namespace == "" || appName == "" {
		return RouteHealth{}
	}

	ing, err := kubeClient.NetworkingV1().Ingresses(namespace).Get(ctx, appName, metav1.GetOptions{})
	if kerrors.IsNotFound(err) {
		target := host
		if target == "" {
			target = appName
		}
		return RouteHealth{
			Message: fmt.Sprintf("No Ingress found for %s.", target),
		}
	}
	if err != nil {
		return RouteHealth{Message: "Unable to read Ingress."}
	}

	// When the route's host changes, the existing Ingress (which is named
	// after the app and so still found above) keeps the old host until
	// the controller reconciles. Treat it as not-yet-ready for the new
	// host, otherwise we'd report TLS pending or HTTP-only for a host
	// that the Ingress isn't actually serving.
	if host != "" {
		hostMatches := false
		for _, rule := range ing.Spec.Rules {
			if rule.Host == host {
				hostMatches = true
				break
			}
		}
		if !hostMatches {
			return RouteHealth{
				Message: fmt.Sprintf("Waiting for Ingress to serve %s.", host),
			}
		}
	}

	var tlsSecret string
	for _, tls := range ing.Spec.TLS {
		if tls.SecretName == "" {
			continue
		}
		// If a host was passed in, prefer a TLS section that lists it.
		// When no host filter is given, take the first TLS section.
		if host == "" {
			tlsSecret = tls.SecretName
			break
		}
		for _, h := range tls.Hosts {
			if h == host {
				tlsSecret = tls.SecretName
				break
			}
		}
		if tlsSecret != "" {
			break
		}
	}

	displayHost := host
	if displayHost == "" && len(ing.Spec.Rules) > 0 {
		displayHost = ing.Spec.Rules[0].Host
	}
	return certificateHealth(ctx, kubeClient, namespace, displayHost, tlsSecret)
}

// certificateHealth completes the health check once the Ingress is known
// to exist. If no TLS secret is referenced, TLS is off by configuration
// rather than waiting to be issued — that's still "ready" from the
// platform's point of view, just unencrypted.
func certificateHealth(ctx context.Context, kubeClient kubernetes.Interface, namespace, host, tlsSecret string) RouteHealth {
	if tlsSecret == "" {
		return RouteHealth{
			IngressReady: true,
			TLSReady:     false,
			Message:      fmt.Sprintf("%s is reachable over HTTP only: no TLS configured.", host),
		}
	}

	secret, err := kubeClient.CoreV1().Secrets(namespace).Get(ctx, tlsSecret, metav1.GetOptions{})
	if err != nil {
		return RouteHealth{
			IngressReady: true,
			TLSReady:     false,
			Message:      "Waiting for TLS certificate to be issued.",
		}
	}

	certPEM := secret.Data["tls.crt"]
	if len(certPEM) == 0 {
		return RouteHealth{
			IngressReady: true,
			TLSReady:     false,
			Message:      "TLS certificate is provisioning.",
		}
	}

	// When the route's host changes, cert-manager keeps the same secret
	// name and the old certificate stays in place until the new one is
	// issued. Verify the cert actually covers the requested host so we
	// don't report a stale cert as active.
	if host != "" && !certCoversHost(certPEM, host) {
		return RouteHealth{
			IngressReady: true,
			TLSReady:     false,
			Message:      fmt.Sprintf("Waiting for TLS certificate for %s.", host),
		}
	}

	return RouteHealth{
		IngressReady: true,
		TLSReady:     true,
		Message:      "TLS certificate is active.",
	}
}

// certCoversHost reports whether the PEM-encoded certificate is valid
// right now AND its SAN list authorises the given hostname. Returns
// false on any parse error, an expired cert, or a not-yet-valid cert,
// so a stale or malformed secret never gets reported as ready.
func certCoversHost(certPEM []byte, host string) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	now := time.Now()
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return false
	}
	return cert.VerifyHostname(host) == nil
}
