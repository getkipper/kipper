package controllers

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

var migrateKey = types.NamespacedName{Name: "migrate", Namespace: "shop-test"}

func reconcileAgain(t *testing.T, r *JobReconciler) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: migrateKey})
	require.NoError(t, err)
}

func nativeJob(t *testing.T, r *JobReconciler) *batchv1.Job {
	t.Helper()
	var nj batchv1.Job
	require.NoError(t, r.Get(context.Background(), migrateKey, &nj))
	return &nj
}

func setRun(t *testing.T, r *JobReconciler, set func(*batchv1.JobStatus)) {
	t.Helper()
	nj := nativeJob(t, r)
	set(&nj.Status)
	require.NoError(t, r.Status().Update(context.Background(), nj))
}

func finish(t *testing.T, r *JobReconciler, condition batchv1.JobConditionType) {
	t.Helper()
	setRun(t, r, func(s *batchv1.JobStatus) {
		if condition == batchv1.JobComplete {
			s.Succeeded = 1
		} else {
			s.Failed = 1
		}
		s.Conditions = append(s.Conditions, batchv1.JobCondition{Type: condition, Status: corev1.ConditionTrue})
	})
}

func removeRun(t *testing.T, r *JobReconciler) {
	t.Helper()
	require.NoError(t, r.Delete(context.Background(), nativeJob(t, r)))
}

func assertNoRun(t *testing.T, r *JobReconciler) {
	t.Helper()
	var nj batchv1.Job
	err := r.Get(context.Background(), migrateKey, &nj)
	assert.True(t, apierrors.IsNotFound(err), "the native Job is still there or was created again: %v", err)
}

func jobStatus(t *testing.T, r *JobReconciler) kipperv1.JobStatus {
	t.Helper()
	var job kipperv1.Job
	require.NoError(t, r.Get(context.Background(), migrateKey, &job))
	return job.Status
}

func TestOneOffJobDoesNotRunAgainAfterTheTTLRemovesIt(t *testing.T) {
	r := reconcileJob(t, newTestJob(""))
	finish(t, r, batchv1.JobComplete)
	reconcileAgain(t, r)

	removeRun(t, r)
	reconcileAgain(t, r)
	reconcileAgain(t, r)

	assertNoRun(t, r)
	status := jobStatus(t, r)
	assert.Equal(t, "Completed", status.Phase, "the CR keeps the outcome of the run it records")
	assert.Equal(t, "Succeeded", status.LastResult)
}

func TestOneOffJobRemovedBeforeItWasSeenRunningIsRecordedAsFailed(t *testing.T) {
	r := reconcileJob(t, newTestJob(""))

	removeRun(t, r)
	reconcileAgain(t, r)
	reconcileAgain(t, r)

	assertNoRun(t, r)
	status := jobStatus(t, r)
	assert.Equal(t, "Failed", status.Phase, "a run removed before it finished did not complete")
	assert.Equal(t, "Failed", status.LastResult)
}

func TestOneOffJobRemovedWhileRunningIsRecordedAsFailed(t *testing.T) {
	r := reconcileJob(t, newTestJob(""))
	setRun(t, r, func(s *batchv1.JobStatus) { s.Active = 1 })
	reconcileAgain(t, r)
	require.Equal(t, "Running", jobStatus(t, r).Phase)

	removeRun(t, r)
	reconcileAgain(t, r)
	reconcileAgain(t, r)

	assertNoRun(t, r)
	assert.Equal(t, "Failed", jobStatus(t, r).LastResult)
}

func TestOneOffJobRunsOnceWhenItsStatusCouldNotBeWritten(t *testing.T) {
	job := newTestJob("")
	scheme := testScheme()
	failStatus := true
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(job).WithStatusSubresource(job, &batchv1.Job{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl crclient.Client, sub string, obj crclient.Object, opts ...crclient.SubResourceUpdateOption) error {
				if _, isJob := obj.(*kipperv1.Job); isJob && failStatus {
					return errors.New("conflict")
				}
				return cl.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	r := &JobReconciler{Client: c, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: migrateKey})
	require.Error(t, err, "the status write fails after the native Job was created")
	nativeJob(t, r)

	removeRun(t, r)
	failStatus = false
	reconcileAgain(t, r)
	reconcileAgain(t, r)

	assertNoRun(t, r)
	assert.Equal(t, "Failed", jobStatus(t, r).LastResult)
}

func TestOneOffJobNeverWritesAPhaseTheSchemaRejects(t *testing.T) {
	r := reconcileJob(t, newTestJob(""))

	assert.Contains(t, []string{"", "Scheduled", "Running", "Completed", "Failed"}, jobStatus(t, r).Phase)
}

func TestRemovingAScheduleStartsTheFirstOneOffRun(t *testing.T) {
	r := reconcileJob(t, newTestJob("0 3 * * *"))
	var job kipperv1.Job
	require.NoError(t, r.Get(context.Background(), migrateKey, &job))
	job.Status.Phase = "Scheduled"
	job.Status.LastRun = jobStatus(t, r).LastRun
	require.NoError(t, r.Status().Update(context.Background(), &job))
	require.NoError(t, r.Get(context.Background(), migrateKey, &job))
	job.Spec.Schedule = ""
	require.NoError(t, r.Update(context.Background(), &job))

	reconcileAgain(t, r)

	nativeJob(t, r)
}

func TestDeletingAOneOffJobReleasesItsRun(t *testing.T) {
	r := reconcileJob(t, newTestJob(""))
	var job kipperv1.Job
	require.NoError(t, r.Get(context.Background(), migrateKey, &job))
	require.NoError(t, r.Delete(context.Background(), &job))

	reconcileAgain(t, r)

	nj := nativeJob(t, r)
	assert.NotContains(t, nj.Finalizers, runOutcomeFinalizer, "garbage collection must be able to remove the run once its job is gone")
}

func TestAFailedAttemptIsNotTheRunsOutcome(t *testing.T) {
	r := reconcileJob(t, newTestJob(""))
	setRun(t, r, func(s *batchv1.JobStatus) { s.Failed = 1; s.Active = 1 })
	reconcileAgain(t, r)

	status := jobStatus(t, r)
	assert.Equal(t, "Running", status.Phase)
	assert.Empty(t, status.LastResult)
	assert.Contains(t, nativeJob(t, r).Finalizers, runOutcomeFinalizer)

	finish(t, r, batchv1.JobComplete)
	reconcileAgain(t, r)
	assert.Equal(t, "Succeeded", jobStatus(t, r).LastResult)
}

func TestAOneOffJobFailsWhenTheJobReportsFailure(t *testing.T) {
	r := reconcileJob(t, newTestJob(""))
	finish(t, r, batchv1.JobFailed)
	reconcileAgain(t, r)

	status := jobStatus(t, r)
	assert.Equal(t, "Failed", status.Phase)
	assert.Equal(t, "Failed", status.LastResult)
	assert.NotContains(t, nativeJob(t, r).Finalizers, runOutcomeFinalizer)
}

func TestScheduledJobReleasesALeftoverOneOffRun(t *testing.T) {
	r := reconcileJob(t, newTestJob(""))
	var job kipperv1.Job
	require.NoError(t, r.Get(context.Background(), migrateKey, &job))
	job.Spec.Schedule = "0 3 * * *"
	require.NoError(t, r.Update(context.Background(), &job))

	reconcileAgain(t, r)

	assert.NotContains(t, nativeJob(t, r).Finalizers, runOutcomeFinalizer,
		"nothing records a one-off outcome once the job is scheduled, so its run must be free to go")
}

// Simulate a cached CR that predates the recorded outcome and native Job cleanup.
func TestOneOffJobChecksTheServerBeforeStartingARun(t *testing.T) {
	scheme := testScheme()
	stale := newTestJob("")
	stale.Finalizers = []string{jobFinalizer}
	recorded := stale.DeepCopy()
	recorded.Status.Phase = "Failed"
	recorded.Status.LastResult = "Failed"

	cached := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(stale).WithStatusSubresource(stale).Build()
	server := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(recorded).WithStatusSubresource(recorded).Build()
	r := &JobReconciler{Client: cached, APIReader: server, Scheme: scheme}

	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: migrateKey})

	assertNoRun(t, r)
}

// Simulate deletion of the CR before its native Job reaches the cache.
func TestDeletingAOneOffJobReleasesARunTheCacheHasNotSeen(t *testing.T) {
	scheme := testScheme()
	job := newTestJob("")
	job.Finalizers = []string{jobFinalizer}
	server := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(job).WithStatusSubresource(job).Build()
	r := &JobReconciler{Client: server, APIReader: server, Scheme: scheme}
	reconcileAgain(t, r)
	require.Contains(t, nativeJob(t, r).Finalizers, runOutcomeFinalizer)

	r.Client = interceptorClient(server, func(obj crclient.Object) bool {
		_, isRun := obj.(*batchv1.Job)
		return isRun
	})
	var current kipperv1.Job
	require.NoError(t, server.Get(context.Background(), migrateKey, &current))
	require.NoError(t, server.Delete(context.Background(), &current))

	reconcileAgain(t, r)

	var nj batchv1.Job
	require.NoError(t, server.Get(context.Background(), migrateKey, &nj))
	assert.NotContains(t, nj.Finalizers, runOutcomeFinalizer)
}

// interceptorClient simulates cache lag by returning NotFound for objects
// matched by hidden. All other operations use the underlying client.
func interceptorClient(c crclient.WithWatch, hidden func(crclient.Object) bool) crclient.Client {
	return interceptor.NewClient(c, interceptor.Funcs{
		Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if hidden(obj) {
				return apierrors.NewNotFound(batchv1.Resource("jobs"), key.Name)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	})
}
