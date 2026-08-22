// Package nsowner answers one question, in one place: which project owns a
// namespace.
//
// It exists because that question was answered independently in nine places,
// each reading `kipper.run/project` off the namespace and believing it. The
// label is writable by anyone who can write a namespace, so every one of those
// was an authorization decision resting on a value the caller does not control.
// Rewriting one label moved a namespace, and with it the credentials, images
// and links its workloads reach.
//
// The label is still where the answer starts, because finding the candidate any
// other way means listing every project. It is a hint rather than evidence: the
// project it names must also claim the namespace, by name and by the object's
// UID, and a project's claim is written by its own reconcile. A forged label
// points at a project that claims nothing, and resolves to nobody.
//
// The UID matters as much as the name. A namespace deleted and recreated is a
// different object, so a claim naming the old one says nothing about the new
// one, and matching on name alone would hand a replacement to whoever held its
// predecessor.
package nsowner

import (
	"context"
	"fmt"

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

// Of returns the project that owns a namespace.
//
// ok is false when nothing owns it: a namespace with no label, a label naming a
// project that does not exist, or one naming a project whose claims do not
// cover this object. All three mean the same thing to a caller, which is that
// this namespace is not a project's to act on.
//
// An error is a failure to find out, which is not the same as an answer. A
// caller that treats it as "not owned" fails closed; one that treats it as
// owned has misread this.
func Of(ctx context.Context, reader Reader, namespace string) (project string, ok bool, err error) {
	var ns corev1.Namespace
	if err := reader.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading namespace %s: %w", namespace, err)
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

	for _, claim := range p.Status.NamespaceClaims {
		if claim.Name == namespace && claim.UID == ns.UID {
			return candidate, true, nil
		}
	}
	return "", false, nil
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
