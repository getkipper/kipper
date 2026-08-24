package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

func TestProjectReconciler_CreatesNamespacesForEachEnvironment(t *testing.T) {
	scheme := testScheme()

	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: kipperv1.ProjectSpec{
			Environments: []kipperv1.ProjectEnvironment{
				{Name: "test"},
				{Name: "prod"},
			},
		},
	}

	fakeClient := projectFakeBuilder().
		WithScheme(scheme).
		WithObjects(project, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()

	r := &ProjectReconciler{Client: fakeClient, Scheme: scheme, APIReader: fakeClient}

	// First reconcile adds the finalizer; second reconcile creates namespaces.
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "demo"}})
	require.NoError(t, err)
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "demo"}})
	require.NoError(t, err)

	for _, ns := range []string{"demo-test", "demo-prod"} {
		var got corev1.Namespace
		err := fakeClient.Get(context.Background(), crclient.ObjectKey{Name: ns}, &got)
		assert.NoError(t, err, "namespace %s should exist", ns)
		assert.Equal(t, "demo", got.Labels[kipperlabels.Project])
	}
}

func envList(names ...string) []kipperv1.ProjectEnvironment {
	out := make([]kipperv1.ProjectEnvironment, 0, len(names))
	for _, n := range names {
		out = append(out, kipperv1.ProjectEnvironment{Name: n})
	}
	return out
}

func TestProjectReconciler_CapsNamespaceCreationAtLimit(t *testing.T) {
	scheme := testScheme()

	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "big", Finalizers: []string{projectFinalizer}},
		Spec: kipperv1.ProjectSpec{
			Tier:         kipperv1.TierSmall, // limit 4
			Environments: envList("e1", "e2", "e3", "e4", "e5", "e6"),
		},
	}

	fakeClient := projectFakeBuilder().
		WithScheme(scheme).
		WithObjects(project, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()

	r := &ProjectReconciler{Client: fakeClient, Scheme: scheme, APIReader: fakeClient}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "big"}})
	require.NoError(t, err)

	var nsList corev1.NamespaceList
	require.NoError(t, fakeClient.List(context.Background(), &nsList, crclient.MatchingLabels{kipperlabels.Project: "big"}))
	assert.Len(t, nsList.Items, 4, "namespace creation must stop at the tier limit")

	for _, ns := range []string{"big-e1", "big-e2", "big-e3", "big-e4"} {
		var got corev1.Namespace
		assert.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Name: ns}, &got), "%s should be created", ns)
	}
	for _, ns := range []string{"big-e5", "big-e6"} {
		var got corev1.Namespace
		assert.True(t, errors.IsNotFound(fakeClient.Get(context.Background(), crclient.ObjectKey{Name: ns}, &got)), "%s should not be created beyond the limit", ns)
	}

	var got kipperv1.Project
	require.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Name: "big"}, &got))
	assert.NotNil(t, apimeta.FindStatusCondition(got.Status.Conditions, conditionEnvLimitExceeded), "over-limit project must carry the condition")
}

func TestProjectReconciler_TierlessCreatesNoQuotaObjects(t *testing.T) {
	scheme := testScheme()

	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Finalizers: []string{projectFinalizer}},
		Spec: kipperv1.ProjectSpec{
			Environments: envList("test"),
		},
	}

	fakeClient := projectFakeBuilder().
		WithScheme(scheme).
		WithObjects(project, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()

	r := &ProjectReconciler{Client: fakeClient, Scheme: scheme, APIReader: fakeClient}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "shop"}})
	require.NoError(t, err)

	var ns corev1.Namespace
	require.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Name: "shop-test"}, &ns))

	var quota corev1.ResourceQuota
	assert.True(t, errors.IsNotFound(fakeClient.Get(context.Background(),
		crclient.ObjectKey{Name: kipperv1.ProjectQuotaName, Namespace: "shop-test"}, &quota)),
		"tierless environment must not get a ResourceQuota")
	var lr corev1.LimitRange
	assert.True(t, errors.IsNotFound(fakeClient.Get(context.Background(),
		crclient.ObjectKey{Name: kipperv1.ProjectLimitRangeName, Namespace: "shop-test"}, &lr)),
		"tierless environment must not get a LimitRange")
}

func TestProjectReconciler_TierRemovalDeletesQuotaObjects(t *testing.T) {
	scheme := testScheme()

	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Finalizers: []string{projectFinalizer}},
		Spec: kipperv1.ProjectSpec{
			Tier:         kipperv1.TierSmall,
			Environments: envList("test"),
		},
	}

	fakeClient := projectFakeBuilder().
		WithScheme(scheme).
		WithObjects(project, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()

	r := &ProjectReconciler{Client: fakeClient, Scheme: scheme, APIReader: fakeClient}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "shop"}})
	require.NoError(t, err)

	var quota corev1.ResourceQuota
	require.NoError(t, fakeClient.Get(context.Background(),
		crclient.ObjectKey{Name: kipperv1.ProjectQuotaName, Namespace: "shop-test"}, &quota),
		"tiered environment gets a ResourceQuota first")

	var current kipperv1.Project
	require.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Name: "shop"}, &current))
	current.Spec.Tier = ""
	require.NoError(t, fakeClient.Update(context.Background(), &current))

	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "shop"}})
	require.NoError(t, err)

	assert.True(t, errors.IsNotFound(fakeClient.Get(context.Background(),
		crclient.ObjectKey{Name: kipperv1.ProjectQuotaName, Namespace: "shop-test"}, &quota)),
		"clearing the tier must remove the ResourceQuota")
	var lr corev1.LimitRange
	assert.True(t, errors.IsNotFound(fakeClient.Get(context.Background(),
		crclient.ObjectKey{Name: kipperv1.ProjectLimitRangeName, Namespace: "shop-test"}, &lr)),
		"clearing the tier must remove the LimitRange")
}

func TestProjectReconciler_TierlessDirectCreateCappedAtSix(t *testing.T) {
	// The CEL rule on ProjectSpec references oldSelf, so it never fires on
	// create: a GitOps manifest with seven environments is admitted and the
	// reconciler is the backstop that caps namespace creation at the
	// tierless limit and flags the project.
	scheme := testScheme()

	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "branchy", Finalizers: []string{projectFinalizer}},
		Spec: kipperv1.ProjectSpec{
			Environments: envList("e1", "e2", "e3", "e4", "e5", "e6", "e7"),
		},
	}

	fakeClient := projectFakeBuilder().
		WithScheme(scheme).
		WithObjects(project, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()

	r := &ProjectReconciler{Client: fakeClient, Scheme: scheme, APIReader: fakeClient}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "branchy"}})
	require.NoError(t, err)

	var nsList corev1.NamespaceList
	require.NoError(t, fakeClient.List(context.Background(), &nsList, crclient.MatchingLabels{kipperlabels.Project: "branchy"}))
	assert.Len(t, nsList.Items, 6, "tierless namespace creation must stop at 6")

	var got kipperv1.Project
	require.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Name: "branchy"}, &got))
	assert.NotNil(t, apimeta.FindStatusCondition(got.Status.Conditions, conditionEnvLimitExceeded),
		"over-limit tierless project must carry the condition")
}

func TestProjectReconciler_KeepsExistingNamespacesOverLimit(t *testing.T) {
	scheme := testScheme()

	// Five environments already have namespaces (as after a tier downgrade to
	// small, limit 4). None must be pruned, and none created.
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Finalizers: []string{projectFinalizer}},
		Spec:       kipperv1.ProjectSpec{Tier: kipperv1.TierSmall, Environments: envList("e1", "e2", "e3", "e4", "e5")},
	}
	objs := []crclient.Object{project}
	for _, e := range []string{"e1", "e2", "e3", "e4", "e5"} {
		objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: "shop-" + e, UID: types.UID("uid-shop-" + e),
			Labels: map[string]string{kipperlabels.Project: "shop"}}})
	}

	fakeClient := projectFakeBuilder().
		WithScheme(scheme).
		WithObjects(append(objs, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9"))...).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()

	r := &ProjectReconciler{Client: fakeClient, Scheme: scheme, APIReader: fakeClient}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "shop"}})
	require.NoError(t, err)

	for _, e := range []string{"e1", "e2", "e3", "e4", "e5"} {
		var got corev1.Namespace
		assert.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Name: "shop-" + e}, &got), "existing over-limit namespace shop-%s must be preserved", e)
	}

	var got kipperv1.Project
	require.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Name: "shop"}, &got))
	assert.NotNil(t, apimeta.FindStatusCondition(got.Status.Conditions, conditionEnvLimitExceeded), "over-limit project must carry the condition")
}

func TestProjectReconciler_ReorderDoesNotStrandExisting(t *testing.T) {
	scheme := testScheme()

	// A new env sorts FIRST, ahead of four already-existing ones, at the small
	// limit of 4. Existence, not spec order, must decide: the four existing
	// namespaces survive and the new one is not created.
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Finalizers: []string{projectFinalizer}},
		Spec:       kipperv1.ProjectSpec{Tier: kipperv1.TierSmall, Environments: envList("newone", "e1", "e2", "e3", "e4")},
	}
	objs := []crclient.Object{project}
	for _, e := range []string{"e1", "e2", "e3", "e4"} {
		objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: "shop-" + e, UID: types.UID("uid-shop-" + e),
			Labels: map[string]string{kipperlabels.Project: "shop"}}})
	}

	fakeClient := projectFakeBuilder().
		WithScheme(scheme).
		WithObjects(append(objs, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9"))...).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()

	r := &ProjectReconciler{Client: fakeClient, Scheme: scheme, APIReader: fakeClient}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "shop"}})
	require.NoError(t, err)

	for _, e := range []string{"e1", "e2", "e3", "e4"} {
		var got corev1.Namespace
		assert.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Name: "shop-" + e}, &got), "existing namespace shop-%s must not be stranded by a reordered spec", e)
	}
	var got corev1.Namespace
	assert.True(t, errors.IsNotFound(fakeClient.Get(context.Background(), crclient.ObjectKey{Name: "shop-newone"}, &got)), "a new env beyond the limit must not be created")
}

func TestProjectReconciler_PrunesNamespacesRemovedFromSpec(t *testing.T) {
	scheme := testScheme()

	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "demo",
			Finalizers: []string{projectFinalizer},
		},
		Spec: kipperv1.ProjectSpec{
			Environments: []kipperv1.ProjectEnvironment{{Name: "test"}},
		},
		// What the pass before this one recorded, back when prod was still
		// declared. Pruning deletes from what the project has a record of
		// holding, so this is the difference between a namespace it is dropping
		// and one somebody has pointed at it.
		Status: kipperv1.ProjectStatus{Namespaces: []string{"demo-test", "demo-prod"}},
	}

	keep := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "demo-test",
			UID:    "uid-demo-test",
			Labels: map[string]string{kipperlabels.Project: "demo"},
		},
	}
	stale := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "demo-prod",
			UID:    "uid-demo-prod",
			Labels: map[string]string{kipperlabels.Project: "demo"},
		},
	}
	unrelated := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "other-prod",
			UID:    "uid-other-prod",
			Labels: map[string]string{kipperlabels.Project: "other"},
		},
	}

	fakeClient := projectFakeBuilder().
		WithScheme(scheme).
		WithObjects(project, keep, stale, unrelated, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()

	r := &ProjectReconciler{Client: fakeClient, Scheme: scheme, APIReader: fakeClient}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "demo"}})
	require.NoError(t, err)

	var got corev1.Namespace
	err = fakeClient.Get(context.Background(), crclient.ObjectKey{Name: "demo-test"}, &got)
	assert.NoError(t, err, "kept env namespace must survive")

	err = fakeClient.Get(context.Background(), crclient.ObjectKey{Name: "demo-prod"}, &got)
	assert.True(t, errors.IsNotFound(err), "stale env namespace must be deleted, got err=%v", err)

	err = fakeClient.Get(context.Background(), crclient.ObjectKey{Name: "other-prod"}, &got)
	assert.NoError(t, err, "unrelated project namespace must not be touched")
}

func TestProjectReconciler_FinalizerDeletesAllProjectNamespaces(t *testing.T) {
	scheme := testScheme()

	now := metav1.NewTime(time.Now())
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "demo",
			Finalizers:        []string{projectFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: kipperv1.ProjectSpec{
			Environments: []kipperv1.ProjectEnvironment{{Name: "test"}, {Name: "prod"}},
		},
		Status: kipperv1.ProjectStatus{Namespaces: []string{"demo-test", "demo-prod"}},
	}

	nsTest := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "demo-test",
			UID:    "uid-demo-test",
			Labels: map[string]string{kipperlabels.Project: "demo"},
		},
	}
	nsProd := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "demo-prod",
			UID:    "uid-demo-prod",
			Labels: map[string]string{kipperlabels.Project: "demo"},
		},
	}

	fakeClient := projectFakeBuilder().
		WithScheme(scheme).
		WithObjects(project, nsTest, nsProd, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()

	r := &ProjectReconciler{Client: fakeClient, Scheme: scheme, APIReader: fakeClient}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "demo"}})
	require.NoError(t, err)

	for _, name := range []string{"demo-test", "demo-prod"} {
		var got corev1.Namespace
		err := fakeClient.Get(context.Background(), crclient.ObjectKey{Name: name}, &got)
		assert.True(t, errors.IsNotFound(err), "namespace %s should be deleted on project teardown, got err=%v", name, err)
	}
}
