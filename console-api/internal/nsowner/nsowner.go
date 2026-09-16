// Package nsowner resolves namespace ownership from the project label and
// Project status records. UID-bearing claims distinguish namespace replacements;
// legacy namespace-name records remain supported. Cleanup also checks competing
// claims through ClaimedElsewhere.
//
// During the compatibility rollout, fallbackToLabel still accepts an existing
// project named by the label when records do not match. Older reconcilers can
// erase new claims during status updates, so requiring claims immediately would
// interrupt access. A forged label remains effective in this mode.
//
// Retiring the fallback requires an explicit migration decision. Resolution checks
// only the labeled project; cleanup also checks other projects' claims. Before
// enforcing records, resolve that difference or retire legacy name-only records.
// Initial adoption of unclaimed namespaces remains a reconciler responsibility.
package nsowner

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
)

// Reader is what resolving needs: the namespace and the project it points at.
type Reader interface {
	Get(ctx context.Context, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error
}

// Of resolves ownership under the current compatibility policy. Missing
// namespaces, labels or Projects return ok=false; read failures return an error.
// See fallbackToLabel for namespaces whose ownership records do not match.
func Of(ctx context.Context, reader Reader, namespace string) (project string, ok bool, err error) {
	if reader == nil {
		return "", false, nil
	}

	var ns corev1.Namespace
	if err := reader.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading namespace %s: %w", namespace, err)
	}
	return OfNamespace(ctx, reader, &ns)
}

// OfNamespace resolves ownership using an already-read namespace, avoiding
// a duplicate API lookup.
func OfNamespace(ctx context.Context, reader Reader, ns *corev1.Namespace) (project string, ok bool, err error) {
	if reader == nil || ns == nil {
		return "", false, nil
	}

	candidate := ns.Labels[labels.Project]
	if candidate == "" {
		return "", false, nil
	}

	var p kipperv1.Project
	if err := reader.Get(ctx, types.NamespacedName{Name: candidate}, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading project %s: %w", candidate, err)
	}

	if HoldsObject(p.Status, ns.Name, ns.UID) {
		return candidate, true, nil
	}
	if fallbackToLabel {
		return candidate, true, nil
	}
	return "", false, nil
}

// HoldsObject checks UID claims before the legacy namespace-name record.
// A claim for another UID overrides that name record, distinguishing a
// recreated namespace from the object the project acquired.
func HoldsObject(status kipperv1.ProjectStatus, namespace string, uid types.UID) bool {
	if Claimed(status.NamespaceClaims, namespace, uid) {
		return true
	}
	for _, claim := range status.NamespaceClaims {
		if claim.Name == namespace {
			return false
		}
	}
	return slices.Contains(status.Namespaces, namespace)
}

// ClaimedElsewhere checks other projects' claims for the exact namespace UID.
// Cleanup uses it to protect a current holder from another project's legacy
// name record. Resolution does not perform this cross-project check: before
// removing the label fallback, either add the check there or retire legacy
// name records once UID claims are complete.
func ClaimedElsewhere(projects []kipperv1.Project, self, namespace string, uid types.UID) bool {
	for i := range projects {
		if projects[i].Name == self {
			continue
		}
		if Claimed(projects[i].Status.NamespaceClaims, namespace, uid) {
			return true
		}
	}
	return false
}

// EverHeld accepts an exact UID claim or a legacy namespace-name record,
// including a name now carried by a recreated object. Cleanup must also check
// the project label and ClaimedElsewhere to protect another project's holder.
func EverHeld(status kipperv1.ProjectStatus, namespace string, uid types.UID) bool {
	if Claimed(status.NamespaceClaims, namespace, uid) {
		return true
	}
	return slices.Contains(status.Namespaces, namespace)
}

// Claimed reports whether these claims cover this exact object.
//
// This is the evidence the label is a hint towards, and it is what release 2
// answers from alone. It is separate from Of so that it stays under test on its
// own terms while Of still carries the compatibility fallback above, and it is
// exported because the reconciler decides from the same rule which namespaces
// it may delete. There is one definition of what a claim covers.
func Claimed(claims []kipperv1.NamespaceClaim, namespace string, uid types.UID) bool {
	for _, claim := range claims {
		if claim.Name == namespace && claim.UID == uid {
			return true
		}
	}
	return false
}

// fallbackToLabel preserves access while ownership records are populated.
// Removing it denies access to namespaces missing from both records; migration
// must also address the cross-project check described by ClaimedElsewhere.
const fallbackToLabel = true

// OwnsNamespace is Owns for a caller that has already read the namespace.
func OwnsNamespace(ctx context.Context, reader Reader, project string, ns *corev1.Namespace) (bool, error) {
	owner, ok, err := OfNamespace(ctx, reader, ns)
	if err != nil || !ok {
		return false, err
	}
	return owner == project, nil
}

// Owns reports whether a named project owns a namespace.
//
// The same question as Of, asked by the callers that already know which project
// they mean and only need it confirmed.
func Owns(ctx context.Context, reader Reader, project, namespace string) (bool, error) {
	owner, ok, err := Of(ctx, reader, namespace)
	if err != nil || !ok {
		return false, err
	}
	return owner == project, nil
}
