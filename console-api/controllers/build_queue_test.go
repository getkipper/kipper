package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/builder"
)

// queueGitSource is the source every test App builds from.
var queueGitSource = &kipperv1.AppGitSource{URL: "https://git.example.com/shop/web.git", Branch: "main"}

func queuedBuild(name string, created time.Time, suspended bool, status batchv1.JobStatus) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "kipper-builds",
			CreationTimestamp: metav1.NewTime(created),
			Annotations:       map[string]string{builder.SourceFingerprintAnnotation: builder.GitSourceFingerprint(queueGitSource)},
			Labels: map[string]string{
				"kipper.run/build":            "true",
				"kipper.run/app":              name,
				"kipper.run/source-namespace": "shop-prod",
			},
		},
		Spec:   batchv1.JobSpec{Suspend: ptr.To(suspended)},
		Status: status,
	}
}

// withApps adds the App each build Job belongs to, so the queue finds them.
func withApps(objs ...crclient.Object) []crclient.Object {
	out := append([]crclient.Object{}, objs...)
	seen := map[string]bool{}
	for _, o := range objs {
		if job, ok := o.(*batchv1.Job); ok && !seen[job.Labels["kipper.run/app"]] {
			seen[job.Labels["kipper.run/app"]] = true
			out = append(out, &kipperv1.App{ObjectMeta: metav1.ObjectMeta{
				Name: job.Labels["kipper.run/app"], Namespace: job.Labels["kipper.run/source-namespace"],
				UID: types.UID(job.Labels["kipper.run/app-uid"]),
			}, Spec: kipperv1.AppSpec{Git: queueGitSource}})
		}
	}
	return out
}

func suspendedBuilds(t *testing.T, c crclient.Client) map[string]bool {
	t.Helper()
	var jobs batchv1.JobList
	require.NoError(t, c.List(context.Background(), &jobs, crclient.InNamespace("kipper-builds")))
	out := map[string]bool{}
	for _, j := range jobs.Items {
		out[j.Name] = j.Spec.Suspend != nil && *j.Spec.Suspend
	}
	return out
}

func reconcileBuild(t *testing.T, r *BuildReconciler, name string) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "kipper-builds", Name: name}})
	require.NoError(t, err)
}

// Every build of a migration started at once, and four of them together took
// an 8 GB node down. Builds wait their turn instead, oldest first.
func TestBuildQueue_RunsOneBuildAtATimeByDefault(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	now := time.Now()
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(withApps(
		queuedBuild("web", now.Add(-3*time.Minute), true, batchv1.JobStatus{}),
		queuedBuild("api", now.Add(-2*time.Minute), true, batchv1.JobStatus{}),
		queuedBuild("worker", now.Add(-time.Minute), true, batchv1.JobStatus{}),
	)...).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	reconcileBuild(t, r, "worker")

	assert.Equal(t, map[string]bool{"web": false, "api": true, "worker": true}, suspendedBuilds(t, c))
}

func TestBuildQueue_ReleasesTheNextBuildWhenOneFinishes(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	now := time.Now()
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(withApps(
		queuedBuild("web", now.Add(-3*time.Minute), false, batchv1.JobStatus{Succeeded: 1}),
		queuedBuild("api", now.Add(-2*time.Minute), false, batchv1.JobStatus{Active: 1}),
		queuedBuild("worker", now.Add(-time.Minute), true, batchv1.JobStatus{}),
	)...).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	reconcileBuild(t, r, "web")
	assert.True(t, suspendedBuilds(t, c)["worker"], "a build is still running")

	var api batchv1.Job
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "kipper-builds", Name: "api"}, &api))
	api.Status = batchv1.JobStatus{Failed: 1}
	require.NoError(t, c.Status().Update(context.Background(), &api))
	reconcileBuild(t, r, "api")

	assert.False(t, suspendedBuilds(t, c)["worker"], "the slot a failed build freed goes to the next one")
}

func TestBuildQueue_RunsAsManyAsTheClusterAllows(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "2")
	now := time.Now()
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(withApps(
		queuedBuild("web", now.Add(-3*time.Minute), true, batchv1.JobStatus{}),
		queuedBuild("api", now.Add(-2*time.Minute), true, batchv1.JobStatus{}),
		queuedBuild("worker", now.Add(-time.Minute), true, batchv1.JobStatus{}),
	)...).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	reconcileBuild(t, r, "web")

	assert.Equal(t, map[string]bool{"web": false, "api": false, "worker": true}, suspendedBuilds(t, c))
}

// A deleted Job with no remaining pod frees its slot, even though reconciliation
// can no longer read the Job.
func TestBuildQueue_ReleasesAfterARunningBuildIsDeleted(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(withApps(
		queuedBuild("worker", time.Now(), true, batchv1.JobStatus{}),
	)...).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	reconcileBuild(t, r, "gone")

	assert.False(t, suspendedBuilds(t, c)["worker"])
}

func runningBuildPod(job string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: job + "-pod", Namespace: "kipper-builds",
			Labels: map[string]string{"kipper.run/build": "true", "batch.kubernetes.io/job-name": job},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// A deleted build's pod can go on running, and holding its memory, until it
// stops. Its slot stays taken until then, and the queue looks again while it
// waits, because the pod ending produces no build event.
func TestBuildQueue_WaitsForADeletedBuildsPodToStop(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	pod := runningBuildPod("gone")
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(withApps(
		queuedBuild("worker", time.Now(), true, batchv1.JobStatus{}), pod,
	)...).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "kipper-builds", Name: "gone"}}

	result, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, suspendedBuilds(t, c)["worker"], "the deleted build's pod still runs")
	assert.Positive(t, result.RequeueAfter, "nothing else would wake the queue")

	pod.Status.Phase = corev1.PodSucceeded
	require.NoError(t, c.Status().Update(context.Background(), pod))
	result, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.False(t, suspendedBuilds(t, c)["worker"])
	assert.Zero(t, result.RequeueAfter)
}

// Foreground deletion marks a Job before its pod has stopped.
func TestBuildQueue_CountsABuildBeingDeletedUntilItsPodStops(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	now := time.Now()
	deleting := queuedBuild("web", now.Add(-time.Minute), false, batchv1.JobStatus{Active: 1})
	deleting.Finalizers = []string{"foregroundDeletion"}
	deleting.DeletionTimestamp = &metav1.Time{Time: now}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(withApps(
		deleting, runningBuildPod("web"),
		queuedBuild("worker", now, true, batchv1.JobStatus{}),
	)...).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "kipper-builds", Name: "web"}})

	require.NoError(t, err)
	assert.True(t, suspendedBuilds(t, c)["worker"])
	assert.Positive(t, result.RequeueAfter)
}

func buildsLeft(t *testing.T, c crclient.Client) []string {
	t.Helper()
	var names []string
	for name := range suspendedBuilds(t, c) {
		names = append(names, name)
	}
	return names
}

// Five quick pushes to one app would otherwise run five builds in a row and
// keep only the last result, while every other project waits behind them.
func TestBuildQueue_DropsAWaitingBuildANewerOneReplaces(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	now := time.Now()
	older := queuedBuild("web-1", now.Add(-2*time.Minute), true, batchv1.JobStatus{})
	newer := queuedBuild("web-2", now.Add(-time.Minute), true, batchv1.JobStatus{})
	for _, j := range []*batchv1.Job{older, newer} {
		j.Labels["kipper.run/app"] = "web"
		j.Labels["kipper.run/app-uid"] = "uid-web"
	}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(withApps(older, newer)...).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	reconcileBuild(t, r, "web-2")

	assert.Equal(t, map[string]bool{"web-2": false}, suspendedBuilds(t, c))
}

// An aborted migration deletes the project it created, and a retry queues the
// same builds again behind the ones nobody wants any more.
func TestBuildQueue_DropsAWaitingBuildWhoseAppIsGone(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	now := time.Now()
	orphan := queuedBuild("old-web", now.Add(-2*time.Minute), true, batchv1.JobStatus{})
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(append(
		[]crclient.Object{orphan},
		withApps(queuedBuild("web", now.Add(-time.Minute), true, batchv1.JobStatus{}))...)...).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	reconcileBuild(t, r, "web")

	assert.Equal(t, map[string]bool{"web": false}, suspendedBuilds(t, c))
}

// A same-named App created after the build was queued is a different App.
func TestBuildQueue_DropsAWaitingBuildOfAReplacedApp(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	stale := queuedBuild("web", time.Now(), true, batchv1.JobStatus{})
	stale.Labels["kipper.run/app-uid"] = "uid-deleted"
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(stale, &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod", UID: "uid-current"},
	}).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	reconcileBuild(t, r, "web")

	assert.Empty(t, buildsLeft(t, c))
}

// A request that read the App before a source change can create its Job after
// the request for the new source did. The newer Job is the stale one.
func TestBuildQueue_KeepsTheCurrentSourceBuildWhenAStaleOneIsNewer(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	now := time.Now()
	running := queuedBuild("other", now.Add(-3*time.Minute), false, batchv1.JobStatus{Active: 1})
	current := queuedBuild("web-current", now.Add(-2*time.Minute), true, batchv1.JobStatus{})
	stale := queuedBuild("web-stale", now.Add(-time.Minute), true, batchv1.JobStatus{})
	stale.Annotations[builder.SourceFingerprintAnnotation] = builder.GitSourceFingerprint(
		&kipperv1.AppGitSource{URL: "https://git.example.com/shop/web.git", Branch: "old"})
	for _, j := range []*batchv1.Job{current, stale} {
		j.Labels["kipper.run/app"] = "web"
	}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(withApps(running, current, stale)...).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	reconcileBuild(t, r, "web-stale")

	assert.Equal(t, map[string]bool{"other": false, "web-current": true}, suspendedBuilds(t, c))
}

// Creation times have a resolution of one second, and the Job names that
// break a tie are random, so a tie says nothing about which request came last.
func TestBuildQueue_KeepsBuildsCreatedInTheSameSecond(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	now := time.Now()
	first := queuedBuild("web-a", now, true, batchv1.JobStatus{})
	second := queuedBuild("web-b", now, true, batchv1.JobStatus{})
	for _, j := range []*batchv1.Job{first, second} {
		j.Labels["kipper.run/app"] = "web"
	}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(withApps(first, second)...).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	reconcileBuild(t, r, "web-b")

	assert.Len(t, suspendedBuilds(t, c), 2)
}

// A pod on a node that stopped answering can stay terminating forever. Once
// its grace period is long gone it no longer holds up every other build.
func TestBuildQueue_StopsCountingAPodLongPastItsDeletion(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	stuck := runningBuildPod("gone")
	stuck.Finalizers = []string{"example.com/hold"}
	stuck.DeletionTimestamp = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(withApps(
		queuedBuild("worker", time.Now(), true, batchv1.JobStatus{}), stuck,
	)...).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	reconcileBuild(t, r, "gone")

	assert.False(t, suspendedBuilds(t, c)["worker"])
}

// A queue that cannot start the next build still records the one that ended.
func TestBuildQueue_RecordsAFinishedBuildWhenTheNextOneCannotStart(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	now := time.Now()
	done := queuedBuild("web", now.Add(-time.Minute), false, batchv1.JobStatus{Failed: 1})
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).
		WithObjects(withApps(done, queuedBuild("api", now, true, batchv1.JobStatus{}))...).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(context.Context, crclient.WithWatch, crclient.Object, crclient.Patch, ...crclient.PatchOption) error {
			return errors.New("the API server is unavailable")
		}}).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "kipper-builds", Name: "web"}})

	require.Error(t, err, "the queue is retried")
	var app kipperv1.App
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "shop-prod", Name: "web"}, &app))
	require.NotNil(t, app.Status.Build)
	assert.Equal(t, "Failed", app.Status.Build.Phase)
}

// Canceling a build uses foreground deletion, so a stuck terminating pod
// also keeps its Job around. The queue releases their slot once both deletion
// timestamps are more than stuckPodAfter in the past.
func TestBuildQueue_StopsCountingABuildLongPastItsDeletion(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	now := time.Now()
	long := &metav1.Time{Time: now.Add(-time.Hour)}
	deleting := queuedBuild("web", now.Add(-2*time.Hour), false, batchv1.JobStatus{Active: 1})
	deleting.Finalizers = []string{"foregroundDeletion"}
	deleting.DeletionTimestamp = long
	stuck := runningBuildPod("web")
	stuck.Finalizers = []string{"example.com/hold"}
	stuck.DeletionTimestamp = long
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(withApps(
		deleting, stuck, queuedBuild("worker", now, true, batchv1.JobStatus{}),
	)...).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	reconcileBuild(t, r, "web")

	assert.False(t, suspendedBuilds(t, c)["worker"])
}

// Record why a source change discarded the build tracked by the App so its
// status reflects the discard instead of showing a build in progress until
// the next deploy.
func TestBuildQueue_RecordsWhyItDroppedTheBuildAnAppWaitsFor(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	stale := queuedBuild("web", time.Now(), true, batchv1.JobStatus{})
	stale.Annotations[builder.SourceFingerprintAnnotation] = builder.GitSourceFingerprint(
		&kipperv1.AppGitSource{URL: "https://git.example.com/shop/web.git", Branch: "old"})
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Git: queueGitSource},
		Status:     kipperv1.AppStatus{Build: &kipperv1.AppBuildStatus{Build: "web", Phase: "Pending"}},
	}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(stale, app).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	reconcileBuild(t, r, "web")

	assert.Empty(t, buildsLeft(t, c))
	var got kipperv1.App
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "shop-prod", Name: "web"}, &got))
	assert.Equal(t, "Discarded", got.Status.Build.Phase)
}

// TestBuildQueue_RecordsWhyItDroppedABuildNotYetNamedInTheStatus covers the race
// between creating a migration build and recording its Job name in App status.
// The queue can discard a stale build first and record the result in the Pending
// status for that commit. The migration then preserves that result.
func TestBuildQueue_RecordsWhyItDroppedABuildNotYetNamedInTheStatus(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	stale := queuedBuild("web", time.Now(), true, batchv1.JobStatus{})
	stale.Annotations["kipper.run/commit"] = "abc123"
	stale.Annotations[builder.SourceFingerprintAnnotation] = builder.GitSourceFingerprint(
		&kipperv1.AppGitSource{URL: "https://git.example.com/shop/web.git", Branch: "old"})
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Git: queueGitSource},
		Status:     kipperv1.AppStatus{Build: &kipperv1.AppBuildStatus{Phase: "Pending", Commit: "abc123"}},
	}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(stale, app).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	reconcileBuild(t, r, "web")

	assert.Empty(t, buildsLeft(t, c))
	var got kipperv1.App
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "shop-prod", Name: "web"}, &got))
	assert.Equal(t, "Discarded", got.Status.Build.Phase)
	assert.Equal(t, "web", got.Status.Build.Build)
}

func TestBuildQueue_LeavesAPendingStatusForAnotherCommit(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	stale := queuedBuild("web", time.Now(), true, batchv1.JobStatus{})
	stale.Annotations["kipper.run/commit"] = "abc123"
	stale.Annotations[builder.SourceFingerprintAnnotation] = builder.GitSourceFingerprint(
		&kipperv1.AppGitSource{URL: "https://git.example.com/shop/web.git", Branch: "old"})
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Git: queueGitSource},
		Status:     kipperv1.AppStatus{Build: &kipperv1.AppBuildStatus{Phase: "Pending", Commit: "def456"}},
	}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(stale, app).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	reconcileBuild(t, r, "web")

	assert.Empty(t, buildsLeft(t, c))
	var got kipperv1.App
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "shop-prod", Name: "web"}, &got))
	assert.Equal(t, "Pending", got.Status.Build.Phase)
	assert.Empty(t, got.Status.Build.Build)
}

// TestBuildQueue_LeavesANewerAttemptsStatusWhenDroppingAStaleBuild simulates a
// migration retry resetting App status after either the discard decision reads
// the App or the status writer reads it. The discard reason belongs to the stale
// build, so the newer attempt keeps its status. In the second case, the update
// conflicts and the retry reads the newer attempt.
func TestBuildQueue_LeavesANewerAttemptsStatusWhenDroppingAStaleBuild(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	for name, resetOn := range map[string]int{"before the decision read returns": 1, "after the status writer reads": 2} {
		t.Run(name, func(t *testing.T) { leavesANewerAttemptsStatus(t, resetOn) })
	}
}

func leavesANewerAttemptsStatus(t *testing.T, resetOn int) {
	stale := queuedBuild("web", time.Now(), true, batchv1.JobStatus{})
	stale.Annotations["kipper.run/commit"] = "abc123"
	stale.Annotations[builder.SourceFingerprintAnnotation] = builder.GitSourceFingerprint(
		&kipperv1.AppGitSource{URL: "https://git.example.com/shop/web.git", Branch: "old"})
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Git: queueGitSource},
		Status:     kipperv1.AppStatus{Build: &kipperv1.AppBuildStatus{Build: "web", Phase: "Pending", Commit: "abc123"}},
	}
	appGets := 0
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(stale, app).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, w crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if err := w.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*kipperv1.App); !ok {
				return nil
			}
			appGets++
			if appGets == resetOn {
				var retry kipperv1.App
				require.NoError(t, w.Get(ctx, key, &retry))
				retry.Status.Build = &kipperv1.AppBuildStatus{Phase: "Pending", Commit: "def456"}
				require.NoError(t, w.Status().Update(ctx, &retry))
			}
			return nil
		}}).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	reconcileBuild(t, r, "web")

	assert.Empty(t, buildsLeft(t, c))
	var got kipperv1.App
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "shop-prod", Name: "web"}, &got))
	assert.Equal(t, "Pending", got.Status.Build.Phase)
	assert.Equal(t, "def456", got.Status.Build.Commit)
	assert.Empty(t, got.Status.Build.Build)
}

func TestBuildQueue_StartsOtherBuildsWhenOneAppCannotBeRead(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	now := time.Now()
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(withApps(
		queuedBuild("web", now.Add(-time.Minute), true, batchv1.JobStatus{}),
		queuedBuild("worker", now, true, batchv1.JobStatus{}),
	)...).WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, w crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
		if _, ok := obj.(*kipperv1.App); ok && key.Name == "web" {
			return errors.New("the API server is unavailable")
		}
		return w.Get(ctx, key, obj, opts...)
	}}).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "kipper-builds", Name: "web"}})

	require.Error(t, err, "the queue is retried")
	assert.Equal(t, map[string]bool{"web": true, "worker": false}, suspendedBuilds(t, c))
}

// TestBuildQueue_CountsAStartWhoseResponseWasLost verifies that a successful
// update with a lost response holds its slot for the rest of the pass, so the
// next build waits.
func TestBuildQueue_CountsAStartWhoseResponseWasLost(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	now := time.Now()
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(withApps(
		queuedBuild("web", now.Add(-time.Minute), true, batchv1.JobStatus{}),
		queuedBuild("worker", now, true, batchv1.JobStatus{}),
	)...).WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, w crclient.WithWatch, obj crclient.Object, patch crclient.Patch, opts ...crclient.PatchOption) error {
		if err := w.Patch(ctx, obj, patch, opts...); err != nil {
			return err
		}
		return errors.New("connection reset by peer")
	}}).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "kipper-builds", Name: "web"}})

	require.Error(t, err, "the queue is retried")
	assert.Equal(t, map[string]bool{"web": false, "worker": true}, suspendedBuilds(t, c))
}

// TestBuildQueue_KeepsARecordedDiscardWhileTheDeleteFails verifies that a deletion
// failure returns an error for retry while preserving the App's recorded discard
// and original completion time.
func TestBuildQueue_KeepsARecordedDiscardWhileTheDeleteFails(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	stale := queuedBuild("web", time.Now(), true, batchv1.JobStatus{})
	stale.Annotations[builder.SourceFingerprintAnnotation] = builder.GitSourceFingerprint(
		&kipperv1.AppGitSource{URL: "https://git.example.com/shop/web.git", Branch: "old"})
	recorded := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Git: queueGitSource},
		Status:     kipperv1.AppStatus{Build: &kipperv1.AppBuildStatus{Build: "web", Phase: "Discarded", CompletedAt: &recorded}},
	}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(stale, app).
		WithInterceptorFuncs(interceptor.Funcs{Delete: func(context.Context, crclient.WithWatch, crclient.Object, ...crclient.DeleteOption) error {
			return errors.New("the API server is unavailable")
		}}).Build()
	r := &BuildReconciler{Client: c, Scheme: testScheme()}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "kipper-builds", Name: "web"}})

	require.Error(t, err, "the queue is retried")
	var got kipperv1.App
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "shop-prod", Name: "web"}, &got))
	assert.True(t, recorded.Equal(got.Status.Build.CompletedAt), "completed at %v", got.Status.Build.CompletedAt)
}
