package cmd

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/service"
)

// serviceTLSOnCreate returns the tls value a new service starts with: "on"
// for Postgres unless flag says otherwise, or empty for other types. It rejects
// unsupported values and any explicit TLS flag for other service types.
func serviceTLSOnCreate(serviceType, flag string) (string, error) {
	if serviceType != "postgres" {
		if flag != "" {
			return "", fmt.Errorf("--tls is supported for postgres services only")
		}
		return "", nil
	}
	switch flag {
	case "":
		return "on", nil
	case "on", "off":
		return flag, nil
	}
	return "", fmt.Errorf("--tls must be on or off, not %q", flag)
}

// describeServiceTLS says whether a Postgres service serves TLS, and why not
// when TLS is on but not served yet.
func describeServiceTLS(s service.Snapshot) string {
	switch {
	case s.TLS != "on":
		return "off"
	case s.TLSServing:
		return "on"
	case s.TLSWaiting != "":
		return "on, not served yet: " + s.TLSWaiting
	}
	return "on, not served yet: the controller has not reported it"
}

// setServiceTLS sets spec.tls on a Postgres service's CR, which the service
// controller turns into the matching pod. It reports whether the value
// changed. An absent value counts as off.
func setServiceTLS(ctx context.Context, dyn dynamic.Interface, namespace, name, tls string) (bool, error) {
	svc, err := dyn.Resource(manifest.ServiceGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("reading service %s: %w", name, err)
	}
	if t, _, _ := unstructured.NestedString(svc.Object, "spec", "type"); t != "postgres" {
		return false, fmt.Errorf("--tls is supported for postgres services only. %s is %s", name, t)
	}
	current, _, _ := unstructured.NestedString(svc.Object, "spec", "tls")
	if current == "" {
		current = "off"
	}
	if current == tls {
		return false, nil
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"tls":%q}}`, tls))
	if _, err := dyn.Resource(manifest.ServiceGVR).Namespace(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return false, fmt.Errorf("setting TLS on %s: %w", name, err)
	}
	return true, nil
}
