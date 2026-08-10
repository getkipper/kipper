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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func TestBuildReconciler_SucceededJob(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Git = &kipperv1.AppGitSource{
		URL:    "https://github.com/example/app.git",
		Branch: "main",
	}
	app.Spec.Image = "old-image:latest"

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app-build-abc12345",
			Namespace: "project-test",
			Labels: map[string]string{
				"kipper.run/build":            "true",
				"kipper.run/app":              "my-app",
				"kipper.run/source-namespace": "project-test",
			},
		},
		Status: batchv1.JobStatus{
			Succeeded: 1,
		},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app, job).
		WithStatusSubresource(app).
		Build()

	reconciler := &BuildReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"},
	})
	require.NoError(t, err)

	// App image should be updated to the Zot registry image
	var updated kipperv1.App
	err = fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated)
	require.NoError(t, err)

	assert.Contains(t, updated.Spec.Image, "zot.kipper-system.svc.cluster.local:5000/project-test/my-app:abc12345")
	assert.Equal(t, "Succeeded", updated.Status.Build.Phase)
	assert.NotNil(t, updated.Status.Build.CompletedAt)

	// Deploy history should have an entry
	assert.Contains(t, updated.Annotations["kipper.run/deploy-history"], "abc12345")
	assert.Contains(t, updated.Annotations["kipper.run/deploy-history"], "build")
}

func TestBuildReconciler_SkipsRecreatedApp(t *testing.T) {
	// A build that finishes after its App was deleted and a same-named App
	// recreated (new UID) must not write its image or status onto the new App.
	scheme := testScheme()
	app := newTestApp()
	app.UID = types.UID("new-uid")
	app.Spec.Image = "current-image:latest"

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app-build-stale",
			Namespace: "kipper-builds",
			Labels: map[string]string{
				"kipper.run/build":            "true",
				"kipper.run/app":              "my-app",
				"kipper.run/source-namespace": "project-test",
				"kipper.run/app-uid":          "old-uid",
			},
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	}
	fakeClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-stale", Namespace: "kipper-builds"},
	})
	require.NoError(t, err)

	var updated kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated))
	assert.Equal(t, "current-image:latest", updated.Spec.Image, "the recreated App's image must be untouched by the old build")
	assert.Nil(t, updated.Status.Build, "no build status is written onto the recreated App")
}

func buildJob(name, appUID string, created time.Time, status batchv1.JobStatus) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "kipper-builds",
			CreationTimestamp: metav1.NewTime(created),
			Labels: map[string]string{
				"kipper.run/build":            "true",
				"kipper.run/app":              "my-app",
				"kipper.run/source-namespace": "project-test",
				"kipper.run/app-uid":          appUID,
			},
			Annotations: map[string]string{"kipper.run/commit": "oldcommit"},
		},
		Status: status,
	}
}

func TestBuildReconciler_OlderBuildDoesNotOverwriteNewer(t *testing.T) {
	// Two concurrent builds for the same App: the older one succeeds late and
	// must not overwrite the newer build's image.
	scheme := testScheme()
	app := newTestApp()
	app.UID = types.UID("uid-1")
	app.Spec.Image = "image-from-newer:latest"

	older := buildJob("my-app-build-older", "uid-1", time.Now().Add(-10*time.Minute), batchv1.JobStatus{Succeeded: 1})
	newer := buildJob("my-app-build-newer", "uid-1", time.Now(), batchv1.JobStatus{Active: 1})
	fakeClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, older, newer).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-older", Namespace: "kipper-builds"},
	})
	require.NoError(t, err)

	var updated kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated))
	assert.Equal(t, "image-from-newer:latest", updated.Spec.Image, "a late older build must not overwrite the newer build's image")
	assert.Nil(t, updated.Status.Build, "the superseded older build must not write build status either")
	assert.NotContains(t, updated.Annotations, deployHistoryAnnKey, "the superseded older build must not append deploy history")
}

func TestSupersededByNewerBuild(t *testing.T) {
	// Kubernetes CreationTimestamps are second-precision; truncate so the
	// equal-timestamp tie-break cases compare a stored (truncated) sibling
	// against the in-memory subject on the same footing.
	now := time.Now().Truncate(time.Second)
	tests := []struct {
		name     string
		subject  *batchv1.Job   // the job being reconciled
		others   []*batchv1.Job // sibling jobs in kipper-builds
		expected bool
	}{
		{
			name:     "newer same-uid job supersedes",
			subject:  buildJob("my-app-build-a", "uid-1", now.Add(-time.Minute), batchv1.JobStatus{Succeeded: 1}),
			others:   []*batchv1.Job{buildJob("my-app-build-b", "uid-1", now, batchv1.JobStatus{Active: 1})},
			expected: true,
		},
		{
			name:     "older same-uid job does not supersede",
			subject:  buildJob("my-app-build-b", "uid-1", now, batchv1.JobStatus{Active: 1}),
			others:   []*batchv1.Job{buildJob("my-app-build-a", "uid-1", now.Add(-time.Minute), batchv1.JobStatus{Succeeded: 1})},
			expected: false,
		},
		{
			name:     "equal timestamp, greater-named other wins so this job is superseded",
			subject:  buildJob("my-app-build-a", "uid-1", now, batchv1.JobStatus{Active: 1}),
			others:   []*batchv1.Job{buildJob("my-app-build-b", "uid-1", now, batchv1.JobStatus{Active: 1})},
			expected: true,
		},
		{
			name:     "equal timestamp, smaller-named other loses so this job proceeds",
			subject:  buildJob("my-app-build-b", "uid-1", now, batchv1.JobStatus{Active: 1}),
			others:   []*batchv1.Job{buildJob("my-app-build-a", "uid-1", now, batchv1.JobStatus{Active: 1})},
			expected: false,
		},
		{
			name:     "newer job with a different uid does not supersede",
			subject:  buildJob("my-app-build-a", "uid-1", now.Add(-time.Minute), batchv1.JobStatus{Succeeded: 1}),
			others:   []*batchv1.Job{buildJob("my-app-build-b", "uid-2", now, batchv1.JobStatus{Active: 1})},
			expected: false,
		},
		{
			name:     "only self in the namespace is not superseded",
			subject:  buildJob("my-app-build-a", "uid-1", now, batchv1.JobStatus{Succeeded: 1}),
			others:   nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := testScheme()
			objs := []crclient.Object{tt.subject}
			for _, o := range tt.others {
				objs = append(objs, o)
			}
			fakeClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

			got, err := reconciler.supersededByNewerBuild(context.Background(), tt.subject)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestBuildReconciler_ListErrorRequeuesWithoutOverwrite(t *testing.T) {
	// A transient failure of the supersession list must not let the build write
	// its image; the reconcile returns the error so it is requeued instead.
	scheme := testScheme()
	app := newTestApp()
	app.UID = types.UID("uid-1")
	app.Spec.Image = "current-image:latest"

	job := buildJob("my-app-build-a", "uid-1", time.Now(), batchv1.JobStatus{Succeeded: 1})
	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app, job).
		WithStatusSubresource(app).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ crclient.WithWatch, list crclient.ObjectList, _ ...crclient.ListOption) error {
				if _, ok := list.(*batchv1.JobList); ok {
					return apierrors.NewServiceUnavailable("cache not ready")
				}
				return errors.New("unexpected list")
			},
		}).
		Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-a", Namespace: "kipper-builds"},
	})
	require.Error(t, err, "a supersession list failure must surface as a reconcile error to trigger requeue")

	var updated kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated))
	assert.Equal(t, "current-image:latest", updated.Spec.Image, "the image must not be mutated when supersession cannot be determined")
}

func TestBuildReconciler_SupersessionUsesUncachedReader(t *testing.T) {
	// The manager's Job cache can lag a just-created newer Job. The supersession
	// list must read through the uncached APIReader, so a stale cache (Client)
	// that is missing the newer Job does not let an older build overwrite it.
	scheme := testScheme()
	app := newTestApp()
	app.UID = types.UID("uid-1")
	app.Spec.Image = "image-from-newer:latest"

	older := buildJob("my-app-build-older", "uid-1", time.Now().Add(-10*time.Minute), batchv1.JobStatus{Succeeded: 1})
	newer := buildJob("my-app-build-newer", "uid-1", time.Now(), batchv1.JobStatus{Active: 1})

	// Cached client: the App and the older Job, but NOT the newer Job.
	cached := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, older).WithStatusSubresource(app).Build()
	// Direct API reader: both Jobs, as the real API server would return.
	apiReader := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(older, newer).Build()

	reconciler := &BuildReconciler{Client: cached, Scheme: scheme, APIReader: apiReader}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-older", Namespace: "kipper-builds"},
	})
	require.NoError(t, err)

	var updated kipperv1.App
	require.NoError(t, cached.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated))
	assert.Equal(t, "image-from-newer:latest", updated.Spec.Image,
		"supersession must read the newer Job via APIReader even when the cache omits it")
}

func TestBuildReconciler_SupersededActiveDoesNotWriteBuilding(t *testing.T) {
	// An older, still-active build that a newer build has superseded must not
	// write "Building" onto the App.
	scheme := testScheme()
	app := newTestApp()
	app.UID = types.UID("uid-1")

	older := buildJob("my-app-build-older", "uid-1", time.Now().Add(-10*time.Minute), batchv1.JobStatus{Active: 1})
	newer := buildJob("my-app-build-newer", "uid-1", time.Now(), batchv1.JobStatus{Active: 1})
	fakeClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, older, newer).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-older", Namespace: "kipper-builds"},
	})
	require.NoError(t, err)

	var updated kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated))
	assert.Nil(t, updated.Status.Build, "a superseded older build must not write Building status")
}

func TestBuildReconciler_ActiveStaleUIDDoesNotWriteBuilding(t *testing.T) {
	// A still-running build whose App was deleted and recreated must not write
	// "Building" onto the new App — the UID guard runs before the Building path.
	scheme := testScheme()
	app := newTestApp()
	app.UID = types.UID("uid-new")

	job := buildJob("my-app-build-stale", "uid-old", time.Now(), batchv1.JobStatus{Active: 1})
	fakeClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-stale", Namespace: "kipper-builds"},
	})
	require.NoError(t, err)

	var updated kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated))
	assert.Nil(t, updated.Status.Build, "no Building status written onto the recreated App")
}

func TestBuildReconciler_FailedJob(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Git = &kipperv1.AppGitSource{URL: "https://github.com/example/app.git"}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app-build-def789",
			Namespace: "project-test",
			Labels: map[string]string{
				"kipper.run/build":            "true",
				"kipper.run/app":              "my-app",
				"kipper.run/source-namespace": "project-test",
			},
		},
		Status: batchv1.JobStatus{
			Failed: 1,
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Message: "Dockerfile not found"},
			},
		},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app, job).
		WithStatusSubresource(app).
		Build()

	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-def789", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var updated kipperv1.App
	err = fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated)
	require.NoError(t, err)

	// Image should NOT be changed
	assert.Equal(t, "myimage:latest", updated.Spec.Image)
	assert.Equal(t, "Failed", updated.Status.Build.Phase)
	assert.Equal(t, "Dockerfile not found", updated.Status.Build.Message)
}

func TestBuildReconciler_ActiveJob(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Git = &kipperv1.AppGitSource{URL: "https://github.com/example/app.git"}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app-build-ghi456",
			Namespace: "project-test",
			Labels: map[string]string{
				"kipper.run/build":            "true",
				"kipper.run/app":              "my-app",
				"kipper.run/source-namespace": "project-test",
			},
		},
		Status: batchv1.JobStatus{
			Active: 1,
		},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app, job).
		WithStatusSubresource(app).
		Build()

	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-ghi456", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var updated kipperv1.App
	err = fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated)
	require.NoError(t, err)

	assert.Equal(t, "Building", updated.Status.Build.Phase)
	assert.Equal(t, "myimage:latest", updated.Spec.Image) // unchanged
}

func TestExtractCommitFromJobName(t *testing.T) {
	assert.Equal(t, "abc12345", extractCommitFromJobName("my-app-build-abc12345", "my-app"))
	assert.Equal(t, "manual-1234567890", extractCommitFromJobName("my-app-build-manual-1234567890", "my-app"))
	assert.Equal(t, "", extractCommitFromJobName("my-app-build-", "my-app"))
}

// The OOM hint must come from a pod THIS Job owns. A same-name earlier
// build can leave an OOMKilled pod behind, and it must not hijack the
// failure message of a later, non-OOM build.
func TestBuildReconciler_OOMHintRequiresOwningJob(t *testing.T) {
	tru := true
	oom := corev1.ContainerStatus{Name: "build", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}}
	notOOM := corev1.ContainerStatus{Name: "build", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}}

	buildAndReconcile := func(t *testing.T, curStatus corev1.ContainerStatus) string {
		t.Helper()
		scheme := testScheme()
		app := newTestApp()
		app.Spec.Git = &kipperv1.AppGitSource{URL: "https://github.com/example/app.git"}
		jobUID := types.UID("job-uid-current")
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name: "my-app-build-def789", Namespace: "project-test", UID: jobUID,
				Labels: map[string]string{"kipper.run/build": "true", "kipper.run/app": "my-app", "kipper.run/source-namespace": "project-test"},
			},
			Status: batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Message: "Dockerfile not found"}}},
		}
		owner := func(uid types.UID) []metav1.OwnerReference {
			return []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: uid, Controller: &tru}}
		}
		// Stale OOM pod from an earlier same-name Job (different owner UID).
		stale := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "project-test",
			Labels: map[string]string{"job-name": job.Name}, OwnerReferences: owner("job-uid-old")},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{oom}}}
		// Current pod owned by this Job.
		cur := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "cur", Namespace: "project-test",
			Labels: map[string]string{"job-name": job.Name}, OwnerReferences: owner(jobUID)},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{curStatus}}}
		fakeClient := crfake.NewClientBuilder().WithScheme(scheme).
			WithObjects(app, job, stale, cur).WithStatusSubresource(app).Build()
		reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}
		_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: job.Name, Namespace: "project-test"}})
		require.NoError(t, err)
		var updated kipperv1.App
		require.NoError(t, fakeClient.Get(context.Background(),
			crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated))
		return updated.Status.Build.Message
	}

	// Current build failed for another reason: the stale OOM pod is ignored.
	msg := buildAndReconcile(t, notOOM)
	assert.Equal(t, "Dockerfile not found", msg)

	// Current build itself OOM-killed: the hint appears.
	msg = buildAndReconcile(t, oom)
	assert.Contains(t, msg, "OOMKilled")
	assert.Contains(t, msg, "build-memory")
}
