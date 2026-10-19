package cmd

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

// consistentBackupsOnCreate enables dumps for Postgres unless explicitly
// disabled. Other service types reject an explicit true value and accept false.
func consistentBackupsOnCreate(serviceType string, set, value bool) (bool, error) {
	if serviceType != "postgres" {
		if set && value {
			return false, fmt.Errorf("--consistent-backups is supported for postgres services only")
		}
		return false, nil
	}
	return !set || value, nil
}

// setConsistentBackups sets spec.consistentBackups on a Postgres service's CR
// and reports whether the value changed.
func setConsistentBackups(ctx context.Context, dyn dynamic.Interface, namespace, name string, on bool) (bool, error) {
	svc, err := dyn.Resource(manifest.ServiceGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("reading service %s: %w", name, err)
	}
	if t, _, _ := unstructured.NestedString(svc.Object, "spec", "type"); t != "postgres" {
		return false, fmt.Errorf("--consistent-backups is supported for postgres services only. %s is %s", name, t)
	}
	current, _, _ := unstructured.NestedBool(svc.Object, "spec", "consistentBackups")
	if current == on {
		return false, nil
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"consistentBackups":%t}}`, on))
	if _, err := dyn.Resource(manifest.ServiceGVR).Namespace(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return false, fmt.Errorf("setting consistent backups on %s: %w", name, err)
	}
	return true, nil
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
