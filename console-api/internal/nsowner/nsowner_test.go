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

func TestTheLabelAloneOwnsNothing(t *testing.T) {
	// The label says shop, and shop claims nothing. A forged label is exactly
	// this shape, and it must resolve to nobody.
	c := reader(t, namespace("shop-test", "shop", "the-object"), project("shop"))

	owner, ok, err := Of(context.Background(), c, "shop-test")
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if ok {
		t.Errorf("a namespace whose label names a project that claims nothing resolved to %q; the label is writable by anyone who can write the namespace", owner)
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

func TestAClaimForADifferentObjectDoesNotCount(t *testing.T) {
	// The namespace was deleted and recreated; the claim names the one that is
	// gone. Matching on name alone would hand the replacement to whoever held
	// its predecessor.
	c := reader(t,
		namespace("shop-test", "shop", "the-new-object"),
		project("shop", kipperv1.NamespaceClaim{Name: "shop-test", UID: "the-old-object"}),
	)

	_, ok, err := Of(context.Background(), c, "shop-test")
	if err != nil {
		t.Fatalf("resolving: %v", err)
	}
	if ok {
		t.Error("a claim naming an object that is gone was read as ownership of its replacement")
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
