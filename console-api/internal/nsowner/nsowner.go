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
// other way means listing every project. What changed is that it has to be
// backed: the project the label names must also have its own record of holding
// the namespace, and only its own reconcile writes that record. HoldsObject is
// the rule the resolver answers from, and EverHeld is the one cleanup asks.
//
// The UID matters as much as the name, for the record that carries one. A
// namespace deleted and recreated is a different object, so a claim naming the
// old one says nothing about the new one, and matching on name alone would hand
// a replacement to whoever held its predecessor.
//
// # What a backed label does not prove
//
// A namespace nobody has claimed is adopted by the project its label names, and
// the reconcile then writes a record for it. So the first adoption of any
// namespace does rest on the label, and there is no way around that: an
// unclaimed namespace has no other evidence, and refusing to adopt on the label
// would mean no namespace could ever be adopted, including one a restore or kip
// created. What the records buy is everything after the first adoption. A
// namespace another project already holds is refused, a contested name is
// adopted only by the project whose own record already covers it, and a
// relabel cannot manufacture either record.
//
// # The claim is seeded in this release and required in the next
//
// A Project's status is written whole by every pod that reconciles it, so a pod
// running the previous release drops namespaceClaims on its next write, having
// no such field. Both releases run at once for the length of a rolling upgrade.
// A build that required the claim would therefore answer "nobody owns this" for
// every namespace an older pod had just erased, and every non-admin would lose
// their project, builds would lose their shared credentials, and staged
// registry credentials would be withdrawn — for the whole window, and again
// each time an old pod wrote status.
//
// So this release falls back to the label when the claim is absent, which makes
// its answer identical to the released version's. The evidence is written now
// and believed a release later, once no pod erases it. Release 2 deletes
// fallbackToLabel and its callers here, and nothing else changes.
//
// What that costs is stated plainly: for one release a forged label still moves
// a namespace, exactly as it does today. Writing that label needs cluster-level
// namespaces update, which none of the three project roles grants, so the hole
// is reachable by a cluster admin or by a compromised console-api — and either
// of those can write the claim in the same breath, which is why closing it a
// release early buys less than it looks.
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

// Of returns the project that owns a namespace.
//
// ok is false when nothing owns it: a namespace with no label, or a label
// naming a project that does not exist. Both mean the same thing to a caller,
// which is that this namespace is not a project's to act on.
//
// A project that exists but claims nothing for this namespace still owns it in
// this release. See the package comment: the claim is seeded now and required
// in the next release, because a pod running the previous one erases it.
//
// An error is a failure to find out, which is not the same as an answer. A
// caller that treats it as "not owned" fails closed; one that treats it as
// owned has misread this.
func Of(ctx context.Context, reader Reader, namespace string) (project string, ok bool, err error) {
	// No reader is no answer, and this decides authorization, so it is not
	// owned by anybody rather than owned by whoever asked.
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

// OfNamespace is Of for a caller that has already read the namespace.
//
// The same answer, without the read. Several callers hold the object already,
// having just fetched it to tell "there is no such namespace" apart from "it is
// not yours", and re-fetching it through a second client doubles the API cost
// of every project-scoped request for nothing.
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

// HoldsObject reports whether a project's own records say it holds this exact
// namespace object.
//
// Two records, and neither is reachable by writing a label. A claim names the
// object and is published once the namespace is proven this project's and
// isolated, which leaves a window where the project holds a namespace no claim
// names yet. The namespace list is the older record, written by the reconciler
// at the end of every pass that held the namespace, and it is already there on a
// cluster upgrading from a build that wrote no claims. Writing either needs
// projects/status, which on a stock cluster only console-api holds.
//
// The list carries only a name, because that is all the older record ever
// carried, so a claim naming this namespace at a different object overrides it:
// the claim knows which object the project took and this one is not it. Without
// that a namespace deleted and recreated under a name the project used to hold
// would still resolve to it.
//
// This is what the resolver answers from once the label no longer counts. It is
// deliberately not what cleanup asks: see EverHeld.
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

// ClaimedElsewhere reports whether a project other than this one holds a claim
// on this exact object.
//
// A project's own records are evidence about the project. A claim is evidence
// about the object, and it is the stronger of the two, because the older record
// carries only a name and a name outlives what carried it. Two projects can both
// have a namespace on record — one held it, lost it, and still declares the
// environment whose name resolves to it — and only one of them can hold the live
// object. Without this, the loser's name-only record plus a rewritten label is
// enough to delete the winner's namespace, and rewriting the label is the move
// every gate here exists to survive.
//
// Cleanup asks this and resolution does not, and that is a gap rather than a
// property. Resolution starts from the label, so it has one named project and
// would have to read every other one to ask this, on every request. It does not,
// so in the release that stops falling back to the label a namespace another
// project claims still resolves to whoever the label names, provided that
// project has the name on its own older record and no claim of its own naming
// it — which is exactly the state this function exists because of.
//
// HoldsObject does not close it. Its mismatched-claim rule needs the resolving
// project to hold a claim for the name, and in this state it holds none.
//
// Release 1 is unaffected: the label answers there anyway. What the release that
// deletes the fallback has to decide is whether resolution reads the other
// projects, or whether the record branch is retired once claims are everywhere,
// which would remove the state instead. That decision is not made here.
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

// EverHeld reports whether a project's own records say it ever took this
// namespace name.
//
// Cleanup asks this rather than HoldsObject, and the difference is the object.
// What cleanup is deciding is whether a namespace carrying this project's label
// is the project's to collect, and the thing it would otherwise leave behind is
// a namespace with workloads and member bindings in it and, once the project is
// gone, nothing left to collect them. A namespace recreated out of band is the
// case that most needs collecting and the one object identity refuses, because
// the claim names the object that went away.
//
// It is no weaker than HoldsObject where it matters, but it is not sufficient
// on its own: a name-only record says nothing about which object carries the
// name, so every caller that deletes must also refuse an object another project
// claims. See ClaimedElsewhere, which is the other half of the rule.
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

// fallbackToLabel keeps this release answering as the released version does,
// for the namespaces neither record covers.
//
// It is the last resort and not the mechanism. HoldsObject above is what
// release 2 answers from, and it is answerable today: a cluster upgrading from
// a build that wrote no claims still carries the namespace list every release-0
// reconcile wrote. That is what makes deleting this constant safe even on a
// cluster whose floating image tag carried it from release 0 straight to
// release 2 with no upgrade ever run.
//
// What is left when it goes is a namespace neither record covers, one whose
// project never finished a pass over it. That is a deny where the released
// version allowed, which is why it waits for release 2 rather than going now.
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
