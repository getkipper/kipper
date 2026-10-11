package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// The App's saved build ordering protects its image after the newer Job expires.
func TestAnOlderBuildReplayedAfterTheNewerJobAgedOutChangesNothing(t *testing.T) {
	scheme := testScheme()
	source := &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"}
	app := newTestApp()
	app.Spec.Git = source
	app.Spec.Image = "image-from-newer:latest"
	newerStarted := metav1.NewTime(time.Now().Add(-time.Minute))
	app.Status.Build = &kipperv1.AppBuildStatus{Phase: "Succeeded", Build: "my-app-build-newer", JobCreatedAt: &newerStarted}

	// Use the current source to exercise the build-order guard.
	older := buildJobForSource(t, source, batchv1.JobStatus{Succeeded: 1})
	older.CreationTimestamp = metav1.NewTime(time.Now().Add(-10 * time.Minute))
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, older).WithStatusSubresource(app).Build()
	r := &BuildReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: older.Name, Namespace: older.Namespace}})
	require.NoError(t, err)

	var got kipperv1.App
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &got))
	assert.Equal(t, "image-from-newer:latest", got.Spec.Image)
	require.NotNil(t, got.Status.Build)
	assert.Equal(t, "my-app-build-newer", got.Status.Build.Build, "the status still describes the newer build")
	assert.Equal(t, "Succeeded", got.Status.Build.Phase)
}

func TestTrackingABuildRecordsWhenItsJobWasCreated(t *testing.T) {
	scheme := testScheme()
	app := newTestApp()
	app.UID = types.UID("uid-1")
	created := time.Now().Add(-3 * time.Minute).Truncate(time.Second)
	job := buildJob("my-app-build-new", "uid-1", created, batchv1.JobStatus{Active: 1})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	r := &BuildReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.updateBuildStatus(context.Background(), "project-test", "my-app", "uid-1", job, "Building", ""))

	var got kipperv1.App
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &got))
	require.NotNil(t, got.Status.Build.JobCreatedAt)
	assert.True(t, got.Status.Build.JobCreatedAt.Time.Equal(created), "got %v want %v", got.Status.Build.JobCreatedAt.Time, created)
}

// Replacing the status on completion must preserve the Job's creation time.
func TestAFinishedBuildKeepsTheAgeOfItsJob(t *testing.T) {
	scheme := testScheme()
	source := &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"}
	app := newTestApp()
	app.Spec.Git = source
	created := time.Now().Add(-5 * time.Minute).Truncate(time.Second)
	job := buildJobForSource(t, source, batchv1.JobStatus{Failed: 1})
	job.CreationTimestamp = metav1.NewTime(created)
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	r := &BuildReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: job.Name, Namespace: job.Namespace}})
	require.NoError(t, err)

	var got kipperv1.App
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &got))
	require.NotNil(t, got.Status.Build)
	require.NotNil(t, got.Status.Build.JobCreatedAt, "phase %q", got.Status.Build.Phase)
	assert.True(t, got.Status.Build.JobCreatedAt.Time.Equal(created))
}

func replayWithTrackedBuild(t *testing.T, tracked string, trackedStarted time.Time, replay string, replayCreated time.Time, alsoLive ...*batchv1.Job) kipperv1.App {
	t.Helper()
	scheme := testScheme()
	source := &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"}
	app := newTestApp()
	app.Spec.Git = source
	app.Spec.Image = "image-from-tracked:latest"
	started := metav1.NewTime(trackedStarted)
	app.Status.Build = &kipperv1.AppBuildStatus{Phase: "Succeeded", Build: tracked, JobCreatedAt: &started}

	job := buildJobForSource(t, source, batchv1.JobStatus{Succeeded: 1})
	job.Name = replay
	job.CreationTimestamp = metav1.NewTime(replayCreated)
	objs := []crclient.Object{app, job}
	for _, j := range alsoLive {
		objs = append(objs, j)
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(app).Build()
	r := &BuildReconciler{Client: c, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: job.Name, Namespace: job.Namespace}})
	require.NoError(t, err)
	var got kipperv1.App
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &got))
	return got
}

// Name ordering keeps timestamp ties consistent after the winning Job expires.
func TestATiedBuildReplayedAfterTheWinnerAgedOutChangesNothing(t *testing.T) {
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	got := replayWithTrackedBuild(t, "my-app-build-b", at, "my-app-build-a", at)
	assert.Equal(t, "image-from-tracked:latest", got.Spec.Image)
	assert.Equal(t, "my-app-build-b", got.Status.Build.Build)
}

func TestATiedBuildThatSortsLaterStillDeploys(t *testing.T) {
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	got := replayWithTrackedBuild(t, "my-app-build-a", at, "my-app-build-b", at)
	assert.NotEqual(t, "image-from-tracked:latest", got.Spec.Image, "the name orders the tie in this build's favour")
}

// While the tracked Job exists, its creation time takes precedence over the
// saved timestamp.
func TestASkewedRecordDoesNotBlockANewerBuildWhileItsJobExists(t *testing.T) {
	trackedCreated := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	tracked := buildJob("my-app-build-tracked", "", trackedCreated, batchv1.JobStatus{Failed: 1})
	got := replayWithTrackedBuild(t, "my-app-build-tracked", trackedCreated.Add(5*time.Minute),
		"my-app-build-newer", trackedCreated.Add(2*time.Minute), tracked)
	assert.NotEqual(t, "image-from-tracked:latest", got.Spec.Image, "a skewed record held back a genuinely newer build")
}

// Legacy startedAt values use the console-api clock; only Job creation times
// are reliable for ordering builds.
func TestARecordWithoutTheJobCreationTimeDoesNotOrderBuilds(t *testing.T) {
	scheme := testScheme()
	source := &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"}
	app := newTestApp()
	app.Spec.Git = source
	app.Spec.Image = "image-from-tracked:latest"
	skewed := metav1.NewTime(time.Now().Add(time.Hour))
	app.Status.Build = &kipperv1.AppBuildStatus{Phase: "Discarded", Build: "my-app-build-gone", StartedAt: &skewed}

	newer := buildJobForSource(t, source, batchv1.JobStatus{Succeeded: 1})
	newer.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, newer).WithStatusSubresource(app).Build()
	r := &BuildReconciler{Client: c, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: newer.Name, Namespace: newer.Namespace}})
	require.NoError(t, err)

	var got kipperv1.App
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &got))
	assert.NotEqual(t, "image-from-tracked:latest", got.Spec.Image, "a legacy clock value held back a newer build")
}
