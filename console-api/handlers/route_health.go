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

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
)

// RouteHealth describes route readiness for the Settings tab and Routes page.
type RouteHealth struct {
	IngressReady bool   `json:"ingress_ready"`
	TLSReady     bool   `json:"tls_ready"`
	Message      string `json:"message,omitempty"`
	Stopped      bool   `json:"stopped,omitempty"`
	Unpublished  bool   `json:"unpublished,omitempty"`
}

// unpublishedHealth confirms that a closed route has no usable public paths
// and both its app and public-exception Ingresses are absent. Otherwise, callers
// use live health because an existing Ingress may still serve traffic.
func unpublishedHealth(ctx context.Context, kubeClient kubernetes.Interface, app *kipperv1.App) (RouteHealth, bool) {
	route := app.Spec.Route
	if route == nil || !route.Closed || len(controllers.ClosedRoutePaths(route)) > 0 || kubeClient == nil {
		return RouteHealth{}, false
	}
	for _, name := range []string{app.Name, controllers.PublicPathsIngressName(app.Name)} {
		_, err := kubeClient.NetworkingV1().Ingresses(app.Namespace).Get(ctx, name, metav1.GetOptions{})
		if !kerrors.IsNotFound(err) {
			return RouteHealth{}, false
		}
	}
	h := closedRouteHealth()
	h.Stopped = app.Spec.Stopped != nil
	return h, true
}

func closedRouteHealth() RouteHealth {
	return RouteHealth{Unpublished: true, Message: "This route is closed with no usable public paths, so nothing is published."}
}

// stoppedRouteHealth adds stop status while preserving Ingress and TLS readiness.
func stoppedRouteHealth(h RouteHealth) RouteHealth {
	h.Stopped = true
	h.Message = "The app is stopped, so its route answers with the stopped page."
	return h
}

// routeHealth checks the app's Ingress and TLS certificate, matching the host
// when supplied. Looking up the Ingress by app keeps checks specific to each
// app on a shared host. A missing client, namespace, or app name yields unknown health.
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

// certificateHealth checks TLS for an existing Ingress. Without a TLS secret
// reference, it reports HTTP-only service with TLSReady false.
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
