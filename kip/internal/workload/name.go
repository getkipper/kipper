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

// EnsureNameFree reports whether name is available for a workload of kind
// creating in namespace, failing with a workload.NameTakenError when another
// kind holds it. See workload.Kinds for why the kinds compete.
//
// A name this kind already holds is free for it: every caller here upserts, so
// refusing would block the redeploy of the workload that got there first. In a
// namespace where a collision already exists that is the legitimate holder, and
// telling it to rename over a name it owns would break the working half. The
// intruder's own re-apply still reaches the controller, which refuses it and now
// says so on its status.
//
// A same-kind clash on a create-only caller is left to the API's own
// AlreadyExists. A lookup that fails is reported as a lookup failure rather than
// as a taken name.
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
