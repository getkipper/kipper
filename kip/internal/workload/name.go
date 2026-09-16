// Package workload applies Kipper's cross-kind workload rules to the CLI's
// dynamic client.
package workload

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/controller/pkg/workload"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

// gvrByKind maps each competing workload kind to the resource it is stored as.
var gvrByKind = map[string]schema.GroupVersionResource{
	"app":      manifest.AppGVR,
	"function": manifest.FunctionGVR,
	"job":      manifest.JobGVR,
}

// EnsureNameFree checks competing workload kinds, allowing redeploy when
// the same-kind workload is incumbent. It returns workload.NameTakenError
// for another holder and propagates lookup failures. Same-kind create
// collisions are left to the API's AlreadyExists check.
func EnsureNameFree(ctx context.Context, dyn dynamic.Interface, namespace, name, creating string) error {
	mine, err := dyn.Resource(gvrByKind[creating]).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
	case apierrors.IsNotFound(err):
		mine = nil
	default:
		return fmt.Errorf("checking whether the name %q is free: %w", name, err)
	}

	for _, kind := range workload.Kinds {
		if kind == creating {
			continue
		}
		other, err := dyn.Resource(gvrByKind[kind]).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return fmt.Errorf("checking whether the name %q is free: %w", name, err)
		}
		// A name this kind already holds is normally its own, since every caller
		// here upserts. On a cluster that upgraded into an existing collision
		// neither side holds a reservation yet, and then the incumbent is
		// whichever workload is older — the same rule the controllers use, so an
		// ordinary re-apply of the newer one cannot take a name the controllers
		// would award to the older.
		if mine != nil && !workload.Incumbent(kind, other.GetCreationTimestamp().Time, creating, mine.GetCreationTimestamp().Time) {
			continue
		}
		return workload.NameTakenError{Name: name, Kind: kind}
	}
	return nil
}
