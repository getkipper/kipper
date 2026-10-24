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
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

// otherFinalizer keeps a deleting project readable after the project
// controller lets go, so a test can see what the last pass left on it.
const otherFinalizer = "example.com/keep"

func shopNamespace(name string, moving bool) *corev1.Namespace {
	labels := map[string]string{kipperlabels.Project: "shop"}
	if moving {
		labels[platform.StorageMoveNamespaceLabel] = "true"
	}
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name), Labels: labels}}
}

func markedService(namespace, name string) *kipperv1.Service {
	return &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: map[string]string{platform.StorageMoveAnnotation: "kipper-storage-move-1"},
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
}

func shopClaims(names ...string) []kipperv1.NamespaceClaim {
	claims := make([]kipperv1.NamespaceClaim, 0, len(names))
	for _, n := range names {
		claims = append(claims, kipperv1.NamespaceClaim{Name: n, UID: types.UID("uid-" + n)})
	}
	return claims
}

func reconcileShop(t *testing.T, r *ProjectReconciler) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "shop"}})
	require.NoError(t, err)
	return result
}

func namespaceGone(t *testing.T, c crclient.Client, name string) bool {
	t.Helper()
	err := c.Get(context.Background(), crclient.ObjectKey{Name: name}, &corev1.Namespace{})
	if errors.IsNotFound(err) {
		return true
	}
	require.NoError(t, err)
	return false
}

func endMove(t *testing.T, c crclient.Client, name string) {
	t.Helper()
	var ns corev1.Namespace
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Name: name}, &ns))
	delete(ns.Labels, platform.StorageMoveNamespaceLabel)
	require.NoError(t, c.Update(context.Background(), &ns))
}

func shopProject(t *testing.T, c crclient.Client) *kipperv1.Project {
	t.Helper()
	var p kipperv1.Project
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Name: "shop"}, &p))
	return &p
}

func TestADeletingProjectWaitsForItsStorageMoveAndThenFinishes(t *testing.T) {
	scheme := testScheme()
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "shop",
			Finalizers:        []string{projectFinalizer, otherFinalizer},
			DeletionTimestamp: &metav1.Time{Time: time.Now()},
		},
		Spec:   kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "test"}, {Name: "prod"}}},
		Status: kipperv1.ProjectStatus{NamespaceClaims: shopClaims("shop-test", "shop-prod")},
	}
	c := projectFakeBuilder().WithScheme(scheme).
		WithObjects(project, shopNamespace("shop-test", false), shopNamespace("shop-prod", true), markedService("shop-prod", "db")).
		WithStatusSubresource(&kipperv1.Project{}).Build()
	r := &ProjectReconciler{Client: c, APIReader: c, Scheme: scheme}

	result := reconcileShop(t, r)

	assert.True(t, namespaceGone(t, c, "shop-test"), "a namespace without a move must be deleted as before")
	assert.False(t, namespaceGone(t, c, "shop-prod"), "a namespace holding an unfinished move was deleted")
	assert.Equal(t, storageMoveWaitInterval, result.RequeueAfter, "nothing brings the pass back once the move ends")
	waiting := shopProject(t, c)
	assert.Contains(t, waiting.Finalizers, projectFinalizer, "the project let go while its namespace still stands")
	cond := apimeta.FindStatusCondition(waiting.Status.Conditions, kipperv1.ConditionWaitingForStorageMove)
	require.NotNil(t, cond, "nothing tells the operator why the project is still here")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Contains(t, cond.Message, "service db")
	assert.Contains(t, cond.Message, "shop-prod")

	endMove(t, c, "shop-prod")
	result = reconcileShop(t, r)

	assert.True(t, namespaceGone(t, c, "shop-prod"), "the namespace outlived the end of its move")
	assert.Zero(t, result.RequeueAfter)
	done := shopProject(t, c)
	assert.NotContains(t, done.Finalizers, projectFinalizer, "the project controller kept holding a project with nothing left to delete")
	assert.Nil(t, apimeta.FindStatusCondition(done.Status.Conditions, kipperv1.ConditionWaitingForStorageMove),
		"the condition still says the project waits for a move that has ended")
}

// A cache that has not seen the move label yet lets the delete reach the API
// server, where the namespaces policy refuses it. That refusal is the same
// wait as the label, never a failed pass.
func TestAStaleCacheWaitsOnTheNamespacesPolicyRefusal(t *testing.T) {
	scheme := testScheme()
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "shop",
			Finalizers:        []string{projectFinalizer, otherFinalizer},
			DeletionTimestamp: &metav1.Time{Time: time.Now()},
		},
		Spec:   kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "test"}}},
		Status: kipperv1.ProjectStatus{NamespaceClaims: shopClaims("shop-test")},
	}
	refused := &errors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: 422, Reason: metav1.StatusReasonInvalid,
		Message: "ValidatingAdmissionPolicy '" + platform.StorageMoveNamespacesPolicy + "' with binding '" + platform.StorageMoveNamespacesPolicy +
			"' denied request: This namespace holds a volume moving to Longhorn.",
	}}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(project, shopNamespace("shop-test", false), markedService("shop-test", "db")).
		WithStatusSubresource(&kipperv1.Project{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: stampUID,
			Delete: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.DeleteOption) error {
				if _, ok := obj.(*corev1.Namespace); ok && obj.GetName() == "shop-test" {
					return refused
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &ProjectReconciler{Client: c, APIReader: c, Scheme: scheme}

	result := reconcileShop(t, r)

	assert.False(t, namespaceGone(t, c, "shop-test"))
	assert.Equal(t, storageMoveWaitInterval, result.RequeueAfter)
	cond := apimeta.FindStatusCondition(shopProject(t, c).Status.Conditions, kipperv1.ConditionWaitingForStorageMove)
	require.NotNil(t, cond, "the refusal is reported as the wait it is")
	assert.Contains(t, cond.Message, "service db")
}

func TestAWaitWithoutAMarkedServiceNamesAStorageMove(t *testing.T) {
	scheme := testScheme()
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "shop",
			Finalizers:        []string{projectFinalizer},
			DeletionTimestamp: &metav1.Time{Time: time.Now()},
		},
		Status: kipperv1.ProjectStatus{NamespaceClaims: shopClaims("shop-test")},
	}
	c := projectFakeBuilder().WithScheme(scheme).
		WithObjects(project, shopNamespace("shop-test", true)).
		WithStatusSubresource(&kipperv1.Project{}).Build()
	r := &ProjectReconciler{Client: c, APIReader: c, Scheme: scheme}

	reconcileShop(t, r)

	cond := apimeta.FindStatusCondition(shopProject(t, c).Status.Conditions, kipperv1.ConditionWaitingForStorageMove)
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, "a storage move")
	assert.Contains(t, cond.Message, "shop-test")
}

// Both records are checked, because a cluster upgraded from a build that wrote
// no claims holds only the older namespace record.
func TestARemovedEnvironmentWaitsForItsStorageMoveAndThenGoes(t *testing.T) {
	for name, status := range map[string]kipperv1.ProjectStatus{
		"claims":           {NamespaceClaims: shopClaims("shop-test", "shop-prod", "shop-dev")},
		"namespace record": {Namespaces: []string{"shop-test", "shop-prod", "shop-dev"}},
	} {
		t.Run(name, func(t *testing.T) {
			scheme := testScheme()
			project := &kipperv1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: "shop", Finalizers: []string{projectFinalizer}},
				Spec:       kipperv1.ProjectSpec{Environments: []kipperv1.ProjectEnvironment{{Name: "test"}}},
				Status:     status,
			}
			c := projectFakeBuilder().WithScheme(scheme).
				WithObjects(project, shopNamespace("shop-test", false), shopNamespace("shop-prod", true), shopNamespace("shop-dev", false),
					markedService("shop-prod", "db"), markedService("shop-prod", "cache"),
					nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
				WithStatusSubresource(&kipperv1.Project{}).Build()
			r := &ProjectReconciler{Client: c, APIReader: c, Scheme: scheme}

			result := reconcileShop(t, r)

			assert.False(t, namespaceGone(t, c, "shop-test"), "a declared environment was pruned")
			assert.True(t, namespaceGone(t, c, "shop-dev"), "a removed environment without a move must be pruned as before")
			assert.False(t, namespaceGone(t, c, "shop-prod"), "a removed environment holding an unfinished move was pruned")
			assert.LessOrEqual(t, result.RequeueAfter, storageMoveWaitInterval, "nothing brings the pass back once the move ends")
			cond := apimeta.FindStatusCondition(shopProject(t, c).Status.Conditions, kipperv1.ConditionWaitingForStorageMove)
			require.NotNil(t, cond, "nothing tells the operator why the environment's namespace is still here")
			assert.Equal(t, metav1.ConditionTrue, cond.Status)
			assert.Contains(t, cond.Message, "services cache, db")
			assert.Contains(t, cond.Message, "shop-prod")

			endMove(t, c, "shop-prod")
			reconcileShop(t, r)

			assert.True(t, namespaceGone(t, c, "shop-prod"), "the removed environment outlived the end of its move")
			assert.Nil(t, apimeta.FindStatusCondition(shopProject(t, c).Status.Conditions, kipperv1.ConditionWaitingForStorageMove),
				"the condition still says the project waits for a move that has ended")
		})
	}
}
