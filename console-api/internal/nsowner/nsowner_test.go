package nsowner

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
)

func reader(t *testing.T, objs ...crclient.Object) crclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kipperv1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering the kipper scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering the core scheme: %v", err)
	}
	return crfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func namespace(name, project, uid string) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)}}
	if project != "" {
		ns.Labels = map[string]string{labels.Project: project}
	}
	return ns
}

func project(name string, claims ...kipperv1.NamespaceClaim) *kipperv1.Project {
	return &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     kipperv1.ProjectStatus{NamespaceClaims: claims},
	}
}

// The claim is what the label is a hint towards, and it is tested here on its
// own terms because release 2 answers from it alone.
func TestAClaimCoversOneObjectUnderOneName(t *testing.T) {
	claims := []kipperv1.NamespaceClaim{{Name: "shop-test", UID: "the-object"}}

	if !Claimed(claims, "shop-test", "the-object") {
		t.Error("a project's claim on the namespace it holds did not cover it")
	}
	// A namespace deleted and recreated is a different object, and a claim
	// naming the one that is gone must not carry over to its replacement.
	if Claimed(claims, "shop-test", "the-new-object") {
		t.Error("a claim naming an object that is gone covered its replacement, so whoever held the old namespace holds the new one")
	}
	if Claimed(claims, "shop-prod", "the-object") {
		t.Error("a claim on one namespace covered another")
	}
	if Claimed(nil, "shop-test", "the-object") {
		t.Error("a project that claims nothing covered a namespace")
	}
}

// A label naming a project that claims nothing still resolves in this release.
//
// This is the compatibility fallback, not the intended answer. A pod running
// the previous release erases namespaceClaims on its next status write, so a
// build that refused this would lock every non-admin out of their project for
// the length of a rolling upgrade. The claim is seeded now and required next
// release; see the package comment.
//
// Release 2 deletes fallbackToLabel. This test and
// TestAStaleClaimFallsBackLikeNoClaimAtAll are the two in this file that fail
// when it does. Invert them then: the answer becomes ("", false).
func TestALabelWithNoClaimStillResolvesWhileTheClaimIsOnlySeeded(t *testing.T) {
	c := reader(t, namespace("shop-test", "shop", "the-object"), project("shop"))

	owner, ok, err := Of(context.Background(), c, "shop-test")
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if !ok || owner != "shop" {
		t.Errorf("Of = (%q, %v), want (shop, true): a namespace an older pod had erased the claim for resolved to nobody, which is every non-admin locked out for the rolling window", owner, ok)
	}
}

func TestAClaimedNamespaceResolvesToItsProject(t *testing.T) {
	c := reader(t,
		namespace("shop-test", "shop", "the-object"),
		project("shop", kipperv1.NamespaceClaim{Name: "shop-test", UID: "the-object"}),
	)

	owner, ok, err := Of(context.Background(), c, "shop-test")
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if !ok || owner != "shop" {
		t.Errorf("Of = (%q, %v), want (shop, true): a namespace its project claims resolved to nobody", owner, ok)
	}
}

// A stale claim is the same case as no claim: it covers nothing, so the answer
// comes from the fallback for as long as there is one.
func TestAStaleClaimFallsBackLikeNoClaimAtAll(t *testing.T) {
	c := reader(t,
		namespace("shop-test", "shop", "the-new-object"),
		project("shop", kipperv1.NamespaceClaim{Name: "shop-test", UID: "the-old-object"}),
	)

	owner, ok, err := Of(context.Background(), c, "shop-test")
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if !ok || owner != "shop" {
		t.Errorf("Of = (%q, %v), want (shop, true): a stale claim answered differently from no claim, so the release-1 answer depends on how the claim went missing", owner, ok)
	}
}

func TestAnUnlabelledNamespaceOwnsNothing(t *testing.T) {
	c := reader(t, namespace("orphan", "", "whatever"))

	_, ok, err := Of(context.Background(), c, "orphan")
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if ok {
		t.Error("a namespace with no project label resolved to a project")
	}
}

func TestALabelNamingAProjectThatIsGoneOwnsNothing(t *testing.T) {
	c := reader(t, namespace("shop-test", "shop", "the-object"))

	_, ok, err := Of(context.Background(), c, "shop-test")
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if ok {
		t.Error("a label naming a project that does not exist resolved to it")
	}
}

func TestAMissingNamespaceOwnsNothingAndIsNotAnError(t *testing.T) {
	c := reader(t)

	_, ok, err := Of(context.Background(), c, "never-existed")
	if err != nil {
		t.Fatalf("a namespace that is not there is an answer, not a failure: %v", err)
	}
	if ok {
		t.Error("a namespace that does not exist resolved to a project")
	}
}

func TestOwnsAnswersForOneProject(t *testing.T) {
	c := reader(t,
		namespace("shop-test", "shop", "the-object"),
		project("shop", kipperv1.NamespaceClaim{Name: "shop-test", UID: "the-object"}),
		project("rival"),
	)

	yes, err := Owns(context.Background(), c, "shop", "shop-test")
	if err != nil || !yes {
		t.Errorf("Owns(shop) = (%v, %v), want true", yes, err)
	}
	no, err := Owns(context.Background(), c, "rival", "shop-test")
	if err != nil || no {
		t.Errorf("Owns(rival) = (%v, %v), want false: a project was told it owns another's namespace", no, err)
	}
}

// A caller with no reader gets no answer. Resolving is an authorization
// decision, so the absence of the thing that answers it means nobody owns the
// namespace rather than that the caller does.
func TestNoReaderOwnsNothing(t *testing.T) {
	_, ok, err := Of(context.Background(), nil, "shop-test")
	if err != nil {
		t.Fatalf("a nil reader is an answer, not a failure: %v", err)
	}
	if ok {
		t.Error("a nil reader resolved a namespace to a project")
	}
}

// The name-only record predates claims, so it says nothing about which object
// carried the name. A claim does, and when one names this namespace at a
// different object that is positive evidence the object has been replaced.
// Letting the older record answer over it would hand a project a namespace
// somebody recreated under a name it used to hold.
func TestAStaleClaimStopsTheOlderRecordAnsweringForAReplacement(t *testing.T) {
	status := kipperv1.ProjectStatus{
		Namespaces:      []string{"shop-test"},
		NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-test", UID: "the-old-object"}},
	}

	if HoldsObject(status, "shop-test", "the-new-object") {
		t.Error("a namespace recreated under a name this project used to hold was read as still held, so its members reach an object nobody gave them")
	}
	if !HoldsObject(status, "shop-test", "the-old-object") {
		t.Error("the project was refused the object its own claim names")
	}
}

// With no claim naming it at all there is nothing better than the name, which
// is every namespace on a cluster that has not yet run a pass under this
// release.
func TestTheOlderRecordAnswersWhenNoClaimNamesTheNamespace(t *testing.T) {
	status := kipperv1.ProjectStatus{Namespaces: []string{"shop-test"}}

	if !HoldsObject(status, "shop-test", "any-object") {
		t.Error("a namespace the project recorded holding before claims existed was read as not held")
	}
	if HoldsObject(status, "shop-prod", "any-object") {
		t.Error("a namespace the project never recorded was read as held")
	}
}
