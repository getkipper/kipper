package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
	"github.com/getkipper/kipper/controller/pkg/rollout"
)

type stopHarness struct {
	t   *testing.T
	c   client.Client
	r   *AppReconciler
	key types.NamespacedName
}

func newStopHarness(t *testing.T, app *kipperv1.App, objs ...client.Object) *stopHarness {
	t.Helper()
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(append([]client.Object{app}, objs...)...).WithStatusSubresource(app).Build()
	return &stopHarness{t: t, c: c, r: &AppReconciler{Client: c, Scheme: scheme},
		key: types.NamespacedName{Name: app.Name, Namespace: app.Namespace}}
}

func (h *stopHarness) reconcile() {
	h.t.Helper()
	_, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key})
	require.NoError(h.t, err)
}

func (h *stopHarness) edit(f func(*kipperv1.App)) {
	h.t.Helper()
	var app kipperv1.App
	require.NoError(h.t, h.c.Get(context.Background(), h.key, &app))
	f(&app)
	require.NoError(h.t, h.c.Update(context.Background(), &app))
}

func (h *stopHarness) deployment() *appsv1.Deployment {
	h.t.Helper()
	var d appsv1.Deployment
	require.NoError(h.t, h.c.Get(context.Background(), h.key, &d))
	return &d
}

func (h *stopHarness) setLiveReplicas(n int32) {
	h.t.Helper()
	d := h.deployment()
	d.Spec.Replicas = &n
	require.NoError(h.t, h.c.Update(context.Background(), d))
}

func stop(app *kipperv1.App)  { app.Spec.Stopped = &kipperv1.AppStopped{Reason: "testing"} }
func start(app *kipperv1.App) { app.Spec.Stopped = nil }

func TestStop_PlainAppScalesToZeroAndStartRestoresItsCount(t *testing.T) {
	app := newTestApp()
	app.Spec.Replicas = i32(3)
	h := newStopHarness(t, app)
	h.reconcile()
	before := h.deployment().Spec.Template

	h.edit(stop)
	h.reconcile()
	d := h.deployment()
	assert.Equal(t, int32(0), *d.Spec.Replicas)
	assert.Equal(t, "true", d.Annotations[stoppedAnnotation], "the stop has to be marked on the Deployment for start to find")
	assert.Equal(t, before, d.Spec.Template, "a stop must not change the pod template")

	h.edit(start)
	h.reconcile()
	d = h.deployment()
	assert.Equal(t, int32(3), *d.Spec.Replicas)
	assert.NotContains(t, d.Annotations, stoppedAnnotation)
	assert.Equal(t, before, d.Spec.Template, "a start must not roll a new template")
}

func TestStop_AutoscaledAppScalesToZeroAndStartsAtMinReplicas(t *testing.T) {
	app := newTestApp()
	app.Spec.Autoscale = &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](80)}
	h := newStopHarness(t, app)
	h.reconcile()
	h.setLiveReplicas(4)
	var hpa autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, h.c.Get(context.Background(), h.key, &hpa))

	h.edit(stop)
	h.reconcile()
	assert.Equal(t, int32(0), *h.deployment().Spec.Replicas, "a stop overrides the autoscaler's count")
	var during autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, h.c.Get(context.Background(), h.key, &during), "the autoscaler stays in place while stopped")
	assert.Equal(t, hpa.Spec, during.Spec)

	h.edit(start)
	h.reconcile()
	d := h.deployment()
	assert.Equal(t, int32(2), *d.Spec.Replicas, "an autoscaler does not scale up from zero, so start writes minReplicas")
	assert.NotContains(t, d.Annotations, stoppedAnnotation)

	// From here the autoscaler owns the count again.
	h.setLiveReplicas(5)
	h.reconcile()
	assert.Equal(t, int32(5), *h.deployment().Spec.Replicas)
}

func TestStop_AppCreatedStoppedStartsAtMinReplicas(t *testing.T) {
	app := newTestApp()
	app.Spec.Autoscale = &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](80)}
	stop(app)
	h := newStopHarness(t, app)
	h.reconcile()
	d := h.deployment()
	assert.Equal(t, int32(0), *d.Spec.Replicas)
	assert.Equal(t, "true", d.Annotations[stoppedAnnotation], "a Deployment created for a stopped app carries the marker from the start")

	h.edit(start)
	h.reconcile()
	assert.Equal(t, int32(2), *h.deployment().Spec.Replicas)
}

func TestStop_StartOfAnAppWithZeroReplicasRunsNone(t *testing.T) {
	app := newTestApp()
	app.Spec.Replicas = i32(0)
	stop(app)
	h := newStopHarness(t, app)
	h.reconcile()
	h.edit(start)
	h.reconcile()
	d := h.deployment()
	assert.Equal(t, int32(0), *d.Spec.Replicas)
	assert.NotContains(t, d.Annotations, stoppedAnnotation)
}

func TestStop_ReplicaEditWhileStoppedAppliesOnStart(t *testing.T) {
	app := newTestApp()
	app.Spec.Replicas = i32(2)
	stop(app)
	h := newStopHarness(t, app)
	h.reconcile()
	h.edit(func(a *kipperv1.App) { a.Spec.Replicas = i32(4) })
	h.reconcile()
	assert.Equal(t, int32(0), *h.deployment().Spec.Replicas)
	h.edit(start)
	h.reconcile()
	assert.Equal(t, int32(4), *h.deployment().Spec.Replicas)
}

// The stop record takes precedence over replicas that are still draining.
func TestStop_PhaseFollowsTheStop(t *testing.T) {
	app := newTestApp()
	app.Spec.Replicas = i32(2)
	h := newStopHarness(t, app)
	h.reconcile()
	d := h.deployment()
	d.Status.Replicas, d.Status.ReadyReplicas, d.Status.AvailableReplicas = 2, 2, 2
	require.NoError(t, h.c.Status().Update(context.Background(), d))

	h.edit(stop)
	var got kipperv1.App
	require.NoError(t, h.c.Get(context.Background(), h.key, &got))
	_, err := h.r.observeWorkload(context.Background(), &got)
	require.NoError(t, err)
	assert.Equal(t, "Stopped", got.Status.Phase, "pods still draining")

	got.Spec.Stopped = nil
	_, err = h.r.observeWorkload(context.Background(), &got)
	require.NoError(t, err)
	assert.Equal(t, "Running", got.Status.Phase)
}

func TestStop_PhaseOfAStoppedAppWithoutADeployment(t *testing.T) {
	app := newTestApp()
	stop(app)
	h := newStopHarness(t, app)
	_, err := h.r.observeWorkload(context.Background(), app)
	require.NoError(t, err)
	assert.Equal(t, "Stopped", app.Status.Phase)
}

func TestStop_RolloutConditionWhileStoppingAndStopped(t *testing.T) {
	app := newTestApp()
	app.Spec.Replicas = i32(2)
	h := newStopHarness(t, app)
	h.reconcile()
	h.edit(stop)
	h.reconcile()
	d := h.deployment()
	d.Status.Replicas = 2
	require.NoError(t, h.c.Status().Update(context.Background(), d))

	var got kipperv1.App
	require.NoError(t, h.c.Get(context.Background(), h.key, &got))
	requeue, err := h.r.observeRollout(context.Background(), &got, d)
	require.NoError(t, err)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionRolloutComplete)
	require.NotNil(t, cond)
	assert.Equal(t, string(rollout.Stopping), cond.Reason)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "a stop is not a rollout waiting on anything")
	assert.Equal(t, rolloutRequeue, requeue, "keep looking while pods drain")

	d.Status.Replicas = 0
	requeue, err = h.r.observeRollout(context.Background(), &got, d)
	require.NoError(t, err)
	cond = apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionRolloutComplete)
	assert.Equal(t, string(rollout.Stopped), cond.Reason)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Zero(t, requeue, "nothing to watch once the pods are gone")
}

// Stop/start alone must preserve the live size, even if a new recommendation
// appears after the stop cleared the previous one.
func TestStop_RecommendationWaitsUntilAfterTheStart(t *testing.T) {
	app := newTestApp()
	app.UID = "uid-my-app"
	controller := true
	rec := &kipperv1.ResourceTuning{
		ObjectMeta: metav1.ObjectMeta{
			Name: resourcebounds.TuningName("App", app.Name), Namespace: app.Namespace,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: kipperv1.GroupVersion.String(), Kind: "App", Name: app.Name, UID: app.UID, Controller: &controller}},
		},
		Spec:   kipperv1.ResourceTuningSpec{Kind: "App", Name: app.Name},
		Status: kipperv1.ResourceTuningStatus{Recommendation: kipperv1.TunedResources{MemoryRequest: "1Gi", MemoryLimit: "1Gi"}},
	}
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, rec).WithStatusSubresource(app, rec).Build()
	h := &stopHarness{t: t, c: c, r: &AppReconciler{Client: c, Scheme: scheme},
		key: types.NamespacedName{Name: app.Name, Namespace: app.Namespace}}
	// The stop clears the recommendation; one recorded afterwards is what the
	// hold has to keep out.
	recommendAgain := func() {
		var got kipperv1.ResourceTuning
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: app.Namespace, Name: rec.Name}, &got))
		got.Status.Recommendation = kipperv1.TunedResources{MemoryRequest: "1Gi", MemoryLimit: "1Gi"}
		require.NoError(t, c.Status().Update(context.Background(), &got))
	}
	settleAtZero := func() {
		d := h.deployment()
		d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation}
		require.NoError(t, h.c.Status().Update(context.Background(), d))
	}

	h.edit(stop)
	h.reconcile()
	before := h.deployment().Spec.Template
	recommendAgain()
	settleAtZero()
	h.reconcile()
	assert.Equal(t, before, h.deployment().Spec.Template, "a stopped app must not be resized")

	h.edit(start)
	h.reconcile()
	assert.Equal(t, before, h.deployment().Spec.Template, "the start must bring back the template the app stopped with")
}

// A pod still shutting down does not count in the Deployment's replicas, so
// the count alone would call the app stopped while it still runs.
func TestStop_StoppingUntilTheLastPodIsGone(t *testing.T) {
	app := newTestApp()
	stop(app)
	now := metav1.Now()
	terminating := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "my-app-abc", Namespace: app.Namespace, Labels: map[string]string{"app": app.Name},
		DeletionTimestamp: &now, Finalizers: []string{"example.com/hold"},
	}}
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, terminating).WithStatusSubresource(app).Build()
	h := &stopHarness{t: t, c: c, r: &AppReconciler{Client: c, Scheme: scheme},
		key: types.NamespacedName{Name: app.Name, Namespace: app.Namespace}}
	h.reconcile()
	d := h.deployment()

	var got kipperv1.App
	require.NoError(t, h.c.Get(context.Background(), h.key, &got))
	requeue, err := h.r.observeRollout(context.Background(), &got, d)
	require.NoError(t, err)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionRolloutComplete)
	assert.Equal(t, string(rollout.Stopping), cond.Reason)
	assert.Equal(t, rolloutRequeue, requeue)
}

// The stop itself clears what the auto-sizer recommended from the app's pods,
// so a start soon after, before the auto-sizer looks again, cannot apply it.
func TestStop_ClearsThePendingRecommendation(t *testing.T) {
	app := newTestApp()
	app.UID = "uid-my-app"
	controller := true
	rec := &kipperv1.ResourceTuning{
		ObjectMeta: metav1.ObjectMeta{
			Name: resourcebounds.TuningName("App", app.Name), Namespace: app.Namespace,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: kipperv1.GroupVersion.String(), Kind: "App", Name: app.Name, UID: app.UID, Controller: &controller}},
		},
		Spec:   kipperv1.ResourceTuningSpec{Kind: "App", Name: app.Name},
		Status: kipperv1.ResourceTuningStatus{Recommendation: kipperv1.TunedResources{MemoryRequest: "1Gi", MemoryLimit: "1Gi"}, RecommendedAt: &metav1.Time{Time: time.Now()}},
	}
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, rec).WithStatusSubresource(app, rec).Build()
	h := &stopHarness{t: t, c: c, r: &AppReconciler{Client: c, Scheme: scheme},
		key: types.NamespacedName{Name: app.Name, Namespace: app.Namespace}}
	h.reconcile()

	h.edit(stop)
	h.reconcile()

	var got kipperv1.ResourceTuning
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: app.Namespace, Name: rec.Name}, &got))
	assert.Equal(t, kipperv1.TunedResources{}, got.Status.Recommendation)
	assert.Nil(t, got.Status.RecommendedAt)
	assert.NotEmpty(t, h.deployment().Annotations[lastStopAnnotation], "the resource controller needs a mark that outlives the start")

	h.edit(start)
	h.reconcile()
	assert.NotEmpty(t, h.deployment().Annotations[lastStopAnnotation])
}

// A Deployment created for an app that is already stopped resets the same
// state as stopping a running one.
func TestStop_CreatingAStoppedDeploymentResetsTheStopState(t *testing.T) {
	app := newTestApp()
	app.UID = "uid-my-app"
	stop(app)
	controller := true
	rec := &kipperv1.ResourceTuning{
		ObjectMeta: metav1.ObjectMeta{
			Name: resourcebounds.TuningName("App", app.Name), Namespace: app.Namespace,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: kipperv1.GroupVersion.String(), Kind: "App", Name: app.Name, UID: app.UID, Controller: &controller}},
		},
		Spec:   kipperv1.ResourceTuningSpec{Kind: "App", Name: app.Name},
		Status: kipperv1.ResourceTuningStatus{Recommendation: kipperv1.TunedResources{MemoryRequest: "1Gi", MemoryLimit: "1Gi"}},
	}
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, rec).WithStatusSubresource(app, rec).Build()
	h := &stopHarness{t: t, c: c, r: &AppReconciler{Client: c, Scheme: scheme},
		key: types.NamespacedName{Name: app.Name, Namespace: app.Namespace}}

	h.reconcile()

	var got kipperv1.ResourceTuning
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: app.Namespace, Name: rec.Name}, &got))
	assert.Equal(t, kipperv1.TunedResources{}, got.Status.Recommendation)
	assert.NotEmpty(t, h.deployment().Annotations[lastStopAnnotation])
}
