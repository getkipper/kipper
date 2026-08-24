package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// Two projects can resolve to one namespace name: project shop with an
// environment prod, and project shop-prod with an environment that resolves to
// the project's own name, both derive shop-prod. A new collision is refused to both, because deciding it by
// whoever reconciles first decides ownership by a race.
//
// A collision that a previous release already settled is a different thing. One
// of the two has been running in that namespace for months, and refusing it now
// takes the namespace away from the project that legitimately holds it: no claim
// is written, so the namespace drops out of the project's own record, and once
// the claim is what resolves ownership its members cannot reach it at all. The
// upgrade would break a cluster that worked.
//
// So a settled collision is adopted rather than reopened, on the evidence the
// previous release left: the namespace carries this project's label and this
// project's own record says it held it. A relabel supplies the first and cannot
// supply the second.
func contested(t *testing.T, holder string) crclient.Client {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-prod",
		UID:    "the-contested-object",
		Labels: map[string]string{kipperlabels.Project: holder, kipperlabels.Environment: "default"},
	}}
	// The holder: its environment resolves to the project's own name, which is
	// the contested one, and its record from the previous release says it took
	// it. A project that declares nothing gets an environment called test, so
	// the collision needs the default environment spelled out.
	settled := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-prod", Finalizers: []string{projectFinalizer}},
		Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "default"}}},
		Status:     kipperv1.ProjectStatus{Namespaces: []string{"shop-prod"}},
	}
	// The rival: its prod environment resolves to the same name, and it has
	// never held it.
	rival := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Finalizers: []string{projectFinalizer}},
		Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "prod"}}},
	}
	return claimFixture(t, ns, settled, rival)
}

func TestTheSettledHolderOfAContestedNameKeepsItThroughTheUpgrade(t *testing.T) {
	c := contested(t, "shop-prod")

	reconcileNamed(t, c, "shop-prod")

	claim, ok := namespaceClaimFor(t, c, "shop-prod", "shop-prod")
	require.True(t, ok, "a project running in this namespace since before claims existed was refused it, so the upgrade takes the namespace away from whoever legitimately holds it")
	assert.Equal(t, types.UID("the-contested-object"), claim.UID)
}

// The rival is still refused, so adoption settles the collision rather than
// handing the namespace to both.
func TestTheRivalForAContestedNameIsStillRefused(t *testing.T) {
	c := contested(t, "shop-prod")

	reconcileNamed(t, c, "shop-prod")
	reconcileNamed(t, c, "shop")

	if _, ok := namespaceClaimFor(t, c, "shop", "shop-prod"); ok {
		t.Error("both projects came away holding the same namespace")
	}
	var rival kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &rival))
	assert.NotNil(t, apimeta.FindStatusCondition(rival.Status.Conditions, conditionNamespaceConflict),
		"the refused project was told nothing about why its environment has no namespace")
}

// The label on its own adopts nothing. This is the same fixture with the label
// pointed at the rival, which is what an attacker who can write namespace
// metadata produces, and the rival has no record of ever holding it.
func TestAContestedNameIsNotAdoptedOnTheLabelAlone(t *testing.T) {
	c := contested(t, "shop")

	reconcileNamed(t, c, "shop")

	if claim, ok := namespaceClaimFor(t, c, "shop", "shop-prod"); ok {
		t.Errorf("rewriting a namespace's label was enough to take it from the project that held it: %+v", claim)
	}
}
