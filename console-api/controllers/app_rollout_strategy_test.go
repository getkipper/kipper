package controllers

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/version"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	quotapkg "github.com/getkipper/kipper/console-api/quota"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

func assertDeadline(t *testing.T, dep *appsv1.Deployment, wantDeadline int32) {
	t.Helper()
	require.NotNil(t, dep.Spec.ProgressDeadlineSeconds)
	assert.Equal(t, wantDeadline, *dep.Spec.ProgressDeadlineSeconds)
}

// Leave the strategy unset so Kubernetes applies its rolling-update defaults.
func TestReconcileDeployment_NewDeploymentGetsTheDeadlineAndTheDefaultStrategy(t *testing.T) {
	i32 := func(v int32) *int32 { return &v }
	tests := []struct {
		name         string
		health       *kipperv1.AppHealth
		wantDeadline int32
	}{
		{name: "default startup budget", wantDeadline: 600},
		{name: "short startup budget keeps the floor", health: &kipperv1.AppHealth{Type: "tcp", StartupTimeoutSeconds: i32(60)}, wantDeadline: 600},
		{name: "long startup budget extends the deadline", health: &kipperv1.AppHealth{Type: "tcp", StartupTimeoutSeconds: i32(900)}, wantDeadline: 1200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			app := &kipperv1.App{
				ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "shop-prod"},
				Spec:       kipperv1.AppSpec{Image: "registry.example.com/shop:1", Port: 8080, Health: tt.health},
			}
			c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app).Build()
			r := &AppReconciler{Client: c, Scheme: testScheme()}

			require.NoError(t, r.reconcileDeployment(ctx, app, nil, "gen-1", ""))

			var got appsv1.Deployment
			require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "shop", Namespace: "shop-prod"}, &got))
			assertDeadline(t, &got, tt.wantDeadline)
			assert.Equal(t, appsv1.DeploymentStrategy{}, got.Spec.Strategy, "the strategy is the API server's default")
		})
	}
}

// Updating the deadline must preserve the live strategy and pod template.
func TestReconcileDeployment_LiveDeploymentGetsTheDeadlineWithoutRolling(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Image: "registry.example.com/shop:1", Port: 8080},
	}
	updates := 0
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					updates++
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}
	gen := secretname.EnvGeneration(secretname.KindApp, "shop", "d1")
	require.NoError(t, r.reconcileDeployment(ctx, app, nil, gen, ""))

	key := types.NamespacedName{Name: "shop", Namespace: "shop-prod"}
	var legacy appsv1.Deployment
	require.NoError(t, c.Get(ctx, key, &legacy))
	quarter := intstr.FromString("25%")
	legacy.Spec.Strategy = appsv1.DeploymentStrategy{
		Type:          appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{MaxUnavailable: &quarter, MaxSurge: &quarter},
	}
	legacy.Spec.ProgressDeadlineSeconds = nil
	require.NoError(t, c.Update(ctx, &legacy))
	require.NoError(t, c.Get(ctx, key, &legacy))
	updates = 0

	require.NoError(t, r.reconcileDeployment(ctx, app, nil, gen, ""))

	var got appsv1.Deployment
	require.NoError(t, c.Get(ctx, key, &got))
	assertDeadline(t, &got, 600)
	assert.Equal(t, legacy.Spec.Strategy, got.Spec.Strategy, "the strategy is left as it was")
	assert.Equal(t, legacy.Spec.Template, got.Spec.Template, "the pod template is untouched, so no pod restarts")
	assert.Equal(t, 1, updates)

	updates = 0
	require.NoError(t, r.reconcileDeployment(ctx, app, nil, gen, ""))
	assert.Zero(t, updates, "a Deployment that already has the deadline is not written again")
}

func TestEnsureSurgeOnlyStrategy_ReadsPercentagesAsPercentages(t *testing.T) {
	quarter, one := intstr.FromString("25%"), intstr.FromInt32(1)
	dep := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Strategy: appsv1.DeploymentStrategy{
		Type:          appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{MaxUnavailable: &quarter, MaxSurge: &one},
	}}}
	assert.True(t, ensureSurgeOnlyStrategy(dep), "25% unavailable is not zero")
	assert.Equal(t, intstr.FromInt32(0), *dep.Spec.Strategy.RollingUpdate.MaxUnavailable)
	assert.False(t, ensureSurgeOnlyStrategy(dep), "and once set it stays")
}

func TestSupportsPreStopSleep(t *testing.T) {
	tests := []struct {
		name  string
		info  *version.Info
		err   error
		wants bool
	}{
		{name: "the fleet's version", info: &version.Info{Major: "1", Minor: "35"}, wants: true},
		{name: "a provider suffix", info: &version.Info{Major: "1", Minor: "35+"}, wants: true},
		{name: "the first version with the action on by default", info: &version.Info{Major: "1", Minor: "30"}, wants: true},
		{name: "too old", info: &version.Info{Major: "1", Minor: "29"}},
		{name: "a future major", info: &version.Info{Major: "2", Minor: "0"}, wants: true},
		{name: "garbage", info: &version.Info{Major: "one", Minor: "x"}},
		{name: "empty", info: &version.Info{}},
		{name: "the read failed", err: fmt.Errorf("connection refused")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wants, SupportsPreStopSleep(tt.info, tt.err))
		})
	}
}

// Quota projection must account for the extra pods allowed by the strategy.
func TestAppDeploymentIsPricedForTheDefaultSurge(t *testing.T) {
	eight := int32(8)
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Image: "registry.example.com/shop:1", Port: 8080, Replicas: &eight},
	}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app).Build()
	r := &AppReconciler{Client: c, Scheme: testScheme()}

	dep, err := r.buildDeployment(context.Background(), app, nil, "gen-1", "")
	require.NoError(t, err)

	assert.Equal(t, int32(2), quotapkg.DeploymentSurgePods(dep, eight))
}
