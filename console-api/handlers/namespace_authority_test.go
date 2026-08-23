package handlers

import (
	"context"
	goerrors "errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// namespaceBelongsTo guards privileged work: copy-preview reads the Secret
// names in a namespace, promote and copy write into one, and quota reports its
// usage. It answers "is this namespace this project's" through the shared owner
// lookup, because the label it used to read is writable by anyone who can write
// the namespace. What that lookup requires, and the release it starts requiring
// the claim, is stated once in nsowner.Of.
func authorityFixture(t *testing.T, ns *corev1.Namespace, projects ...*kipperv1.Project) (*k8sfake.Clientset, crclient.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kipperv1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering the kipper scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering the core scheme: %v", err)
	}
	objs := []crclient.Object{ns}
	for _, p := range projects {
		objs = append(objs, p)
	}
	return k8sfake.NewSimpleClientset(ns), crfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func labelled(name, project, uid string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		UID:    types.UID(uid),
		Labels: map[string]string{kipperlabels.Project: project},
	}}
}

func claiming(name string, claims ...kipperv1.NamespaceClaim) *kipperv1.Project {
	return &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     kipperv1.ProjectStatus{NamespaceClaims: claims},
	}
}

// A forged label still moves a namespace in this release.
//
// nsowner pins the same window, and this pins that it reaches here too. The
// privileged reads behind this call are the ones worth making strict early, so
// an undocumented strict subset is exactly what somebody would build here by
// accident; every call site answers alike or the answer depends on which one
// you went through.
//
// Release 2 deletes fallbackToLabel, and this is one of the tests that fails
// when it does. Invert it then: the fixture is already the attack, and the
// assertion becomes the foreignNamespaceError this used to want.
func TestAForgedLabelStillMovesANamespaceWhileTheClaimIsOnlySeeded(t *testing.T) {
	// The victim's namespace, relabelled by anyone who can write namespace
	// metadata. The victim's own claim records the truth, and for one release
	// nothing reads it.
	ns := labelled("victim-prod", "attacker", "the-object")
	clientset, reader := authorityFixture(t, ns,
		claiming("attacker"),
		claiming("victim", kipperv1.NamespaceClaim{Name: "victim-prod", UID: "the-object"}),
	)

	if err := namespaceBelongsTo(context.Background(), clientset, reader, "victim-prod", "attacker"); err != nil {
		t.Fatalf("namespaceBelongsTo = %v, want nil: release 1 answers as the released version does, and refusing here would refuse every project whose claim an older pod had just erased", err)
	}
}

func TestAClaimedNamespaceIsTheProjectsOwn(t *testing.T) {
	ns := labelled("shop-prod", "shop", "the-object")
	clientset, reader := authorityFixture(t, ns,
		claiming("shop", kipperv1.NamespaceClaim{Name: "shop-prod", UID: "the-object"}),
	)

	if err := namespaceBelongsTo(context.Background(), clientset, reader, "shop-prod", "shop"); err != nil {
		t.Fatalf("a project was refused its own claimed namespace: %v", err)
	}
}

// The absent case is a different answer from the foreign one and the callers
// branch on it: copy-preview treats an absent target as a name it may create.
func TestAMissingNamespaceIsReportedAbsentRatherThanForeign(t *testing.T) {
	clientset, reader := authorityFixture(t, labelled("other", "shop", "x"), claiming("shop"))

	err := namespaceBelongsTo(context.Background(), clientset, reader, "shop-prod", "shop")

	var foreign *foreignNamespaceError
	if !goerrors.As(err, &foreign) {
		t.Fatalf("a namespace that does not exist gave %v, not a foreignNamespaceError", err)
	}
	if !foreign.absent {
		t.Error("a namespace that does not exist was reported as somebody else's, so copy-preview refuses a name that is free")
	}
}
