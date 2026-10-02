package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
)

// The Function reconciler rebuilds the container on every pass. A tuned size
// has to survive that rebuild, or the reconciler and the auto-sizer undo each
// other forever.
func TestFunctionKeepsItsTunedSizeAcrossReconciles(t *testing.T) {
	ctx := context.Background()
	fn := httpFunction()
	rec := &kipperv1.ResourceTuning{
		ObjectMeta: metav1.ObjectMeta{Name: resourcebounds.TuningName("Function", fn.Name), Namespace: fn.Namespace, OwnerReferences: controlledBy("Function", fn.Name, fn.UID)},
		Spec:       kipperv1.ResourceTuningSpec{Kind: "Function", Name: fn.Name},
		Status: kipperv1.ResourceTuningStatus{Recommendation: kipperv1.TunedResources{
			MemoryRequest: "512Mi", MemoryLimit: "512Mi",
		}},
	}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(fn, rec).Build()
	r := &FunctionReconciler{Client: c, Scheme: testScheme()}

	for pass := 0; pass < 3; pass++ {
		require.NoError(t, r.reconcileDeployment(ctx, fn, "", nil, "", nil))
	}

	var deploy appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: fn.Namespace, Name: fn.Name}, &deploy))
	res := deploy.Spec.Template.Spec.Containers[0].Resources
	assert.Equal(t, "512Mi", res.Requests.Memory().String(), "the tuned request must survive the rebuild")
	assert.Equal(t, "512Mi", res.Limits.Memory().String(), "the tuned limit must survive the rebuild")
}

func appTuning(app *kipperv1.App, memory string) *kipperv1.ResourceTuning {
	return &kipperv1.ResourceTuning{
		ObjectMeta: metav1.ObjectMeta{Name: resourcebounds.TuningName("App", app.Name), Namespace: app.Namespace, OwnerReferences: controlledBy("App", app.Name, app.UID)},
		Spec:       kipperv1.ResourceTuningSpec{Kind: "App", Name: app.Name},
		Status:     kipperv1.ResourceTuningStatus{Recommendation: kipperv1.TunedResources{MemoryRequest: memory, MemoryLimit: memory}},
	}
}

func appMemory(t *testing.T, c crclient.Client, app *kipperv1.App) (string, string) {
	t.Helper()
	var deploy appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: app.Namespace, Name: app.Name}, &deploy))
	res := deploy.Spec.Template.Spec.Containers[0].Resources
	return res.Requests.Memory().String(), res.Limits.Memory().String()
}

func TestAutomaticAppFollowsItsRecommendation(t *testing.T) {
	ctx := context.Background()
	app := newTestApp()
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app, appTuning(app, "384Mi")).Build()
	r := &AppReconciler{Client: c, Scheme: testScheme()}

	require.NoError(t, r.reconcileDeployment(ctx, app, nil, "gen-1", ""))
	// The tuner waits for the first rollout to finish.
	settleDeployment(t, c, types.NamespacedName{Namespace: app.Namespace, Name: app.Name})
	require.NoError(t, r.reconcileDeployment(ctx, app, nil, "gen-1", ""))
	req, lim := appMemory(t, c, app)
	assert.Equal(t, "384Mi", req)
	assert.Equal(t, "384Mi", lim)
}

// An App value last written by the pre-upgrade auto-sizer is a starting point
// only: the recommendation replaces it, and without one the live size stays.
func TestAppValueFromTheOldAutoSizerFollowsTheRecommendation(t *testing.T) {
	ctx := context.Background()
	app := newTestApp()
	app.Spec.Resources.MemoryRequest = "512Mi"
	app.Spec.Resources.MemoryLimit = "512Mi"
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(appTuning(app, "256Mi")).WithReturnManagedFields().Build()
	require.NoError(t, crclient.WithFieldOwner(c, "console-api").Create(ctx, app))
	var stored kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: app.Name}, &stored))
	spec, err := resourcebounds.AppSpec(&stored)
	require.NoError(t, err)
	require.Equal(t, resourcebounds.Automatic, spec.MemoryRequest.Source, "the value must read as written by the old auto-sizer")
	r := &AppReconciler{Client: c, Scheme: testScheme()}

	require.NoError(t, r.reconcileDeployment(ctx, &stored, nil, "gen-1", ""))
	// The tuner waits for the first rollout to finish.
	settleDeployment(t, c, types.NamespacedName{Namespace: app.Namespace, Name: app.Name})
	require.NoError(t, r.reconcileDeployment(ctx, &stored, nil, "gen-1", ""))
	req, lim := appMemory(t, c, &stored)
	assert.Equal(t, "256Mi", req)
	assert.Equal(t, "256Mi", lim)
}
