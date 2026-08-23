package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// A claim names an object, not a name. When the namespace is deleted and
// recreated out of band, the claim in status points at an object that is gone,
// and a claim pointing at a gone object is worth nothing: nsowner reads it as
// unowned, which is the whole reason the UID is in there.
//
// So the mid-pass publication has to replace it. Stopping because a claim with
// that name is already present leaves the stale one standing until some later
// pass gets all the way to the end without failing, and the add-environment
// flow waits on a claim that never arrives.
func TestPublishingAClaimReplacesOneNamingAnObjectThatIsGone(t *testing.T) {
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status: kipperv1.ProjectStatus{
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-test", UID: "the-old-object"}},
		},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-test",
		UID:    "the-new-object",
		Labels: map[string]string{kipperlabels.Project: "shop"},
	}}

	c := projectFakeBuilder().
		WithScheme(testScheme()).
		WithObjects(project, ns).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()

	r := &ProjectReconciler{Client: c, Scheme: testScheme(), APIReader: c}
	claimed := []kipperv1.NamespaceClaim{{Name: "shop-test", UID: "the-new-object"}}
	require.NoError(t, r.publishClaim(context.Background(), project, "shop-test", claimed))

	var stored kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))

	require.Len(t, stored.Status.NamespaceClaims, 1,
		"the namespace is claimed twice under one name, so which object the project owns depends on read order")
	assert.Equal(t, types.UID("the-new-object"), stored.Status.NamespaceClaims[0].UID,
		"the claim still names the object that was deleted, so the project resolves to nobody until a whole pass completes")
}

// The ordinary case still writes nothing twice. Republishing an identical claim
// on every pass would be a status write per reconcile per namespace.
func TestPublishingAClaimThatIsAlreadyCurrentWritesNothing(t *testing.T) {
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Status: kipperv1.ProjectStatus{
			NamespaceClaims: []kipperv1.NamespaceClaim{{Name: "shop-test", UID: "the-object"}},
		},
	}
	c := projectFakeBuilder().
		WithScheme(testScheme()).
		WithObjects(project).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()

	var before kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &before))

	r := &ProjectReconciler{Client: c, Scheme: testScheme(), APIReader: c}
	claimed := []kipperv1.NamespaceClaim{{Name: "shop-test", UID: "the-object"}}
	require.NoError(t, r.publishClaim(context.Background(), project, "shop-test", claimed))

	var after kipperv1.Project
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "shop"}, &after))
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion,
		"a claim that was already current was written again, so every reconcile of every project writes status for nothing")
}
