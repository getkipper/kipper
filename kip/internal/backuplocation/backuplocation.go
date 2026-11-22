// Package backuplocation reads Velero's default backup storage location.
package backuplocation

import (
	"context"
	"errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// GVR is Velero's BackupStorageLocation resource.
var GVR = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "backupstoragelocations"}

// ErrReadOnly indicates that backup creation was refused because storage is read-only.
var ErrReadOnly = errors.New("this cluster's backup storage is read-only, as it is on recovery installs, so kip cannot save a backup there. Restores and drills from this storage work as usual")

// IsReadOnly reports whether location has the ReadOnly access mode.
// The location must be non-nil. A missing or non-string access mode returns false.
func IsReadOnly(location *unstructured.Unstructured) bool {
	mode, _, _ := unstructured.NestedString(location.Object, "spec", "accessMode")
	return mode == "ReadOnly"
}

// ReadOnly reports whether velero/default, the location kip's backups use, is
// read-only. It returns false when the location cannot be read, leaving the
// backup attempt to report the problem.
func ReadOnly(ctx context.Context, dyn dynamic.Interface) bool {
	location, err := dyn.Resource(GVR).Namespace("velero").Get(ctx, "default", metav1.GetOptions{})
	if err != nil {
		return false
	}
	return IsReadOnly(location)
}
