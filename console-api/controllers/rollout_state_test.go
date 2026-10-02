package controllers

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// rolloutWorld creates a two-replica Deployment with either a settled or
// unfinished rollout. The supplied pods belong to revision 2.
func rolloutWorld(t *testing.T, settled bool, newPods ...corev1.Pod) (*appsv1.Deployment, crclient.Client, *int) {
	t.Helper()
	two := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "shop-prod", UID: "dep-uid", Generation: 4},
		Spec:       appsv1.DeploymentSpec{Replicas: &two, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "shop"}}},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 4, Replicas: 3, UpdatedReplicas: 1, AvailableReplicas: 2},
	}
	if settled {
		dep.Status = appsv1.DeploymentStatus{ObservedGeneration: 4, Replicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2, ReadyReplicas: 2}
	}
	controller := true
	rs := func(name, rev string) *appsv1.ReplicaSet {
		return &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "shop-prod", UID: types.UID(name), Labels: map[string]string{"app": "shop"},
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": rev},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "shop", UID: "dep-uid", Controller: &controller}},
		}}
	}
	objects := []crclient.Object{dep, rs("shop-1", "1"), rs("shop-2", "2")}
	for i := range newPods {
		p := newPods[i].DeepCopy()
		p.Namespace = "shop-prod"
		p.Labels = map[string]string{"app": "shop"}
		p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "shop-2", UID: "shop-2", Controller: &controller}}
		objects = append(objects, p)
	}
	podLists := 0
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c crclient.WithWatch, list crclient.ObjectList, opts ...crclient.ListOption) error {
				if _, ok := list.(*corev1.PodList); ok {
					podLists++
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
	return dep, c, &podLists
}

func unschedulablePod() corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-2-a"},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			Message: "0/1 nodes are available: 1 Insufficient cpu.",
		}}},
	}
}

func TestObserveRollout(t *testing.T) {
	t.Run("a settled rollout is complete and reads no pods", func(t *testing.T) {
		dep, c, podLists := rolloutWorld(t, true)
		r := &AppReconciler{Client: c, Scheme: testScheme()}
		app := healthApp(nil)

		requeue, err := r.observeRollout(context.Background(), app, dep)
		require.NoError(t, err)

		assert.Zero(t, requeue)
		cond := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionRolloutComplete)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Zero(t, *podLists)
	})

	t.Run("a pod the scheduler cannot place is named, warned about once, and looked at again", func(t *testing.T) {
		dep, c, _ := rolloutWorld(t, false, unschedulablePod())
		recorder := record.NewFakeRecorder(10)
		r := &AppReconciler{Client: c, Scheme: testScheme(), Recorder: recorder}
		app := healthApp(nil)

		for range 2 {
			requeue, err := r.observeRollout(context.Background(), app, dep)
			require.NoError(t, err)
			assert.Equal(t, 30*time.Second, requeue, "a pod turning unschedulable changes nothing the watch sees")
		}

		cond := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionRolloutComplete)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, "Unschedulable", cond.Reason)
		assert.Contains(t, cond.Message, "Insufficient cpu")
		require.Len(t, recorder.Events, 1)
		assert.Contains(t, <-recorder.Events, "RolloutWaitingForCapacity")
	})

	t.Run("new pods that never pass their check say which check", func(t *testing.T) {
		started := metav1.NewTime(time.Now().Add(-10 * time.Minute))
		stuck := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "shop-2-a"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, StartTime: &started, Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			}},
		}
		dep, c, _ := rolloutWorld(t, false, stuck)
		r := &AppReconciler{Client: c, Scheme: testScheme()}
		app := healthApp(&kipperv1.AppHealth{Type: "http", Path: "/ready"})

		_, err := r.observeRollout(context.Background(), app, dep)
		require.NoError(t, err)

		cond := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionRolloutComplete)
		require.NotNil(t, cond)
		assert.Equal(t, "NotBecomingReady", cond.Reason)
		assert.Contains(t, cond.Message, "HTTP check of /ready on port 8080")
	})

	t.Run("pods of an older ReplicaSet are not blamed", func(t *testing.T) {
		dep, c, _ := rolloutWorld(t, false)
		old := unschedulablePod()
		old.Name = "shop-1-a"
		old.Namespace = "shop-prod"
		old.Labels = map[string]string{"app": "shop"}
		controller := true
		old.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "shop-1", UID: "shop-1", Controller: &controller}}
		require.NoError(t, c.Create(context.Background(), &old))
		r := &AppReconciler{Client: c, Scheme: testScheme()}
		app := healthApp(nil)

		_, err := r.observeRollout(context.Background(), app, dep)
		require.NoError(t, err)

		cond := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionRolloutComplete)
		require.NotNil(t, cond)
		assert.Equal(t, "InProgress", cond.Reason)
	})
}

func TestHealthCheckStatus(t *testing.T) {
	i32 := func(v int32) *int32 { return &v }
	tpl := func(annotation string) *corev1.PodTemplateSpec {
		t := shapeTemplate(false)
		if annotation != "" {
			t.Annotations = map[string]string{inferredReadinessAnnotation: annotation}
		}
		return t
	}
	running := func(app *kipperv1.App) *corev1.PodTemplateSpec {
		t := shapeTemplate(false)
		t.Spec.Containers[0].ReadinessProbe = declaredReadiness(app)
		return t
	}
	tests := []struct {
		name string
		app  *kipperv1.App
		live *corev1.PodTemplateSpec
		want kipperv1.AppHealthCheckStatus
	}{
		{name: "declared http", app: healthApp(&kipperv1.AppHealth{Type: "http", Path: "/ready", Port: i32(8081)}),
			live: running(healthApp(&kipperv1.AppHealth{Type: "http", Path: "/ready", Port: i32(8081)})),
			want: kipperv1.AppHealthCheckStatus{Type: "http", Port: 8081, Path: "/ready", Source: "declared"}},
		{name: "declared but not yet in the pods", app: healthApp(&kipperv1.AppHealth{Type: "http", Path: "/ready"}),
			live: running(healthApp(&kipperv1.AppHealth{Type: "http", Path: "/old"})),
			want: kipperv1.AppHealthCheckStatus{Type: "http", Port: 8080, Path: "/ready", Source: "applying"}},
		{name: "declared none", app: healthApp(&kipperv1.AppHealth{Type: "none"}), live: tpl(""),
			want: kipperv1.AppHealthCheckStatus{Type: "none", Source: "declared"}},
		{name: "inferred tcp", app: healthApp(nil), live: tpl("tcp"),
			want: kipperv1.AppHealthCheckStatus{Type: "tcp", Port: 8080, Source: "inferred"}},
		{name: "inferred none", app: healthApp(nil), live: tpl("none"),
			want: kipperv1.AppHealthCheckStatus{Type: "none", Source: "inferred"}},
		{name: "pending", app: healthApp(nil), live: tpl(""),
			want: kipperv1.AppHealthCheckStatus{Type: "none", Source: "pending"}},
		{name: "building with a declared check", app: placeholderApp(&kipperv1.AppHealth{Type: "http", Path: "/ready"}), live: tpl(""),
			want: kipperv1.AppHealthCheckStatus{Type: "tcp", Port: 8080, Source: "building"}},
		{name: "building, automatic", app: placeholderApp(nil), live: tpl(""),
			want: kipperv1.AppHealthCheckStatus{Type: "none", Source: "building"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, healthCheckStatus(tt.app, tt.live))
		})
	}
}

// settleDeployment reports the Deployment's rollout as finished, the way the
// Deployment controller would once every new pod is available.
func settleDeployment(t *testing.T, c crclient.Client, key types.NamespacedName) {
	t.Helper()
	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), key, &dep))
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	dep.Status = appsv1.DeploymentStatus{
		ObservedGeneration: dep.Generation, Replicas: want, UpdatedReplicas: want, AvailableReplicas: want, ReadyReplicas: want,
	}
	require.NoError(t, c.Status().Update(context.Background(), &dep))
}

func TestReconcile_AnUnfinishedRolloutIsLookedAtAgain(t *testing.T) {
	app := newTestApp()
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: testScheme()}
	req := ctrlRequest(app)

	res, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, rolloutRequeue, res.RequeueAfter, "a new Deployment has pods still to start")

	var got kipperv1.App
	require.NoError(t, c.Get(context.Background(), req.NamespacedName, &got))
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionRolloutComplete)
	require.NotNil(t, cond, "the condition is written to the App")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	require.NotNil(t, got.Status.HealthCheck)
	assert.Equal(t, "pending", got.Status.HealthCheck.Source)

	settleDeployment(t, c, req.NamespacedName)
	res, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)
}

func ctrlRequest(app *kipperv1.App) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: app.Name, Namespace: app.Namespace}}
}

// Postpone recommendations during a rollout to avoid another pod replacement.
func TestReconcile_TheTunerWaitsForTheRollout(t *testing.T) {
	ctx := context.Background()
	app := newTestApp()
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app).WithStatusSubresource(app, &appsv1.Deployment{}).Build()
	r := &AppReconciler{Client: c, Scheme: testScheme()}
	req := ctrlRequest(app)
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)
	before, _ := appMemory(t, c, app)

	require.NoError(t, c.Get(ctx, req.NamespacedName, app))
	require.NoError(t, c.Create(ctx, appTuning(app, "384Mi")))
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	during, _ := appMemory(t, c, app)
	assert.Equal(t, before, during, "the first rollout has not settled, so the recommendation waits")

	settleDeployment(t, c, req.NamespacedName)
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	after, _ := appMemory(t, c, app)
	assert.Equal(t, "384Mi", after, "and applies once it has")
}

func TestCheckHint(t *testing.T) {
	i32 := func(v int32) *int32 { return &v }
	live := func(inf inference) *corev1.PodTemplateSpec {
		tpl := shapeTemplate(false)
		applyPlatformShape(tpl, healthApp(nil), inf, true)
		return tpl
	}
	tests := []struct {
		name      string
		app       *kipperv1.App
		live      *corev1.PodTemplateSpec
		want      []string
		wantNever []string
	}{
		{name: "no check declared", app: healthApp(&kipperv1.AppHealth{Type: "none"}),
			want: []string{"no check", "kip app logs shop"}, wantNever: []string{"--health none", "accepts connections"}},
		{name: "inferred tcp", app: healthApp(nil), live: live(inferredTCP),
			want: []string{"port 8080", "--health tcp --health-startup-timeout"}, wantNever: []string{"localhost", "--health none"}},
		{name: "automatic worker", app: healthApp(nil), live: live(inferredNone),
			want: []string{"no check", "kip app logs shop"}, wantNever: []string{"accepts connections", "--health tcp"}},
		{name: "automatic, not decided yet", app: healthApp(nil), live: live(inferencePending),
			want: []string{"no check", "kip app logs shop"}, wantNever: []string{"accepts connections", "--health tcp"}},
		{name: "declared tcp", app: healthApp(&kipperv1.AppHealth{Type: "tcp", Port: i32(9000)}),
			want: []string{"port 9000", "localhost", "--health-startup-timeout"}},
		{name: "declared http", app: healthApp(&kipperv1.AppHealth{Type: "http", Path: "/ready"}),
			want: []string{"HTTP check of /ready on port 8080", "--health-startup-timeout"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := tt.live
			if l == nil {
				l = shapeTemplate(false)
			}
			got := checkHint(tt.app, l)
			for _, w := range tt.want {
				assert.Contains(t, got, w)
			}
			for _, w := range tt.wantNever {
				assert.NotContains(t, got, w)
			}
		})
	}
}

// A rejected update leaves the latest generation incomplete even when
// the previous Deployment is healthy.
func TestReconcile_ARefusedDeploymentWriteIsNotComplete(t *testing.T) {
	ctx := context.Background()
	app := newTestApp()
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app).WithStatusSubresource(app, &appsv1.Deployment{}).Build()
	r := &AppReconciler{Client: c, Scheme: testScheme()}
	req := ctrlRequest(app)
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)
	settleDeployment(t, c, req.NamespacedName)

	refusing := interceptor.NewClient(c, interceptor.Funcs{
		Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
			if _, ok := obj.(*appsv1.Deployment); ok && !isDryRun(opts) {
				return fmt.Errorf("admission webhook denied the request")
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	r.Client = refusing
	require.NoError(t, c.Get(ctx, req.NamespacedName, app))
	app.Spec.Image = "myimage:2"
	require.NoError(t, c.Update(ctx, app))

	_, err = r.Reconcile(ctx, req)
	require.Error(t, err)

	var got kipperv1.App
	require.NoError(t, c.Get(ctx, req.NamespacedName, &got))
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionRolloutComplete)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "NotApplied", cond.Reason)
	assert.Contains(t, cond.Message, "admission webhook denied")
}

// The progress deadline releases recommendations even when readiness
// remains the reported cause of the stall.
func TestLiveRolloutPhase_TheDeadlineReleasesTheTuner(t *testing.T) {
	started := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	overdue := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-2-a"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, StartTime: &started, Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		}},
	}
	dep, c, _ := rolloutWorld(t, false, overdue)
	r := &AppReconciler{Client: c, Scheme: testScheme()}
	app := healthApp(nil)

	assert.Equal(t, phaseInFlight, r.liveRolloutPhase(context.Background(), app, dep), "before the deadline")

	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
	}}
	assert.Equal(t, phaseFailed, r.liveRolloutPhase(context.Background(), app, dep), "after it")
}

// Crashes after a completed rollout must not block tuning recommendations.
func TestADegradedAppIsNotRollingOut(t *testing.T) {
	started := metav1.NewTime(time.Now().Add(-time.Hour))
	crashing := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-2-a"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, StartTime: &started, Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		}},
	}
	dep, c, _ := rolloutWorld(t, true, crashing)
	dep.Status.AvailableReplicas, dep.Status.UnavailableReplicas = 1, 1
	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable",
	}}
	r := &AppReconciler{Client: c, Scheme: testScheme()}
	app := healthApp(nil)

	requeue, err := r.observeRollout(context.Background(), app, dep)
	require.NoError(t, err)
	assert.Zero(t, requeue)
	cond := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionRolloutComplete)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)

	assert.Equal(t, phaseSettled, r.liveRolloutPhase(context.Background(), app, dep))
}

// A scale-up with an unschedulable pod must report a waiting rollout
// and allow only recommendations that increase neither resource request.
func TestAScaledUpPodThatCannotBePlacedIsWaiting(t *testing.T) {
	dep, c, _ := rolloutWorld(t, true, unschedulablePod())
	three := int32(3)
	dep.Spec.Replicas = &three
	dep.Status.Replicas, dep.Status.UpdatedReplicas, dep.Status.AvailableReplicas, dep.Status.UnavailableReplicas = 3, 3, 2, 1
	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable",
	}}
	r := &AppReconciler{Client: c, Scheme: testScheme()}
	app := healthApp(nil)

	_, err := r.observeRollout(context.Background(), app, dep)
	require.NoError(t, err)
	cond := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionRolloutComplete)
	require.NotNil(t, cond)
	assert.Equal(t, "Unschedulable", cond.Reason)
	assert.Equal(t, phaseUnschedulable, r.liveRolloutPhase(context.Background(), app, dep))
}

// Keep polling Pending pods after a scale-up because scheduling changes
// may not trigger the Deployment watch.
func TestAPendingScaledUpPodIsLookedAtAgain(t *testing.T) {
	waiting := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "shop-2-a"}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
	dep, c, _ := rolloutWorld(t, true, waiting)
	three := int32(3)
	dep.Spec.Replicas = &three
	dep.Status.Replicas, dep.Status.UpdatedReplicas, dep.Status.AvailableReplicas, dep.Status.UnavailableReplicas = 3, 3, 2, 1
	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable",
	}}
	r := &AppReconciler{Client: c, Scheme: testScheme()}

	requeue, err := r.observeRollout(context.Background(), healthApp(nil), dep)
	require.NoError(t, err)
	assert.Equal(t, rolloutRequeue, requeue)
}
