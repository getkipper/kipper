package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/builder"
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
			Annotations: map[string]string{
				builder.SourceFingerprintAnnotation: builder.GitSourceFingerprint(app.Spec.Git),
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
			Annotations: map[string]string{
				builder.SourceFingerprintAnnotation: builder.GitSourceFingerprint(app.Spec.Git),
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
			Annotations: map[string]string{
				builder.SourceFingerprintAnnotation: builder.GitSourceFingerprint(app.Spec.Git),
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
				Labels:      map[string]string{"kipper.run/build": "true", "kipper.run/app": "my-app", "kipper.run/source-namespace": "project-test"},
				Annotations: map[string]string{builder.SourceFingerprintAnnotation: builder.GitSourceFingerprint(app.Spec.Git)},
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

// The interaction between detaching a source and a build already in flight.
// Detaching is how an operator moves an app onto prebuilt images, and a job
// still running at that moment finishes afterwards — putting the app back onto
// an artefact built from the source they just removed, over the image they
// chose, with a succeeded status and a history entry for a source that is gone.
// Supersession does not cover it: that asks whether a newer build exists, and
// after a detach there is none and never will be.
func TestBuildReconciler_ADetachedSourceWritesNothing(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Git = nil // detached while this build was running
	app.Spec.Image = "registry.example.com/shop/checkout:chosen-by-the-operator"

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app-build-abc12345",
			Namespace: "project-test",
			Labels: map[string]string{
				"kipper.run/build":            "true",
				"kipper.run/app":              "my-app",
				"kipper.run/source-namespace": "project-test",
			},
			Annotations: map[string]string{
				builder.SourceFingerprintAnnotation: builder.GitSourceFingerprint(app.Spec.Git),
			},
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var after kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &after))
	assert.Equal(t, "registry.example.com/shop/checkout:chosen-by-the-operator", after.Spec.Image,
		"a build from a removed source overwrote the image the operator chose")
	assert.Nil(t, after.Status.Build,
		"a build from a removed source recreated a status for a source that is gone")
}

// A build that is still running for a detached source must not resurrect the
// Building status the app reconciler has just cleared.
func TestBuildReconciler_ADetachedSourceDoesNotResurrectBuildingStatus(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Git = nil

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app-build-abc12345",
			Namespace: "project-test",
			Labels: map[string]string{
				"kipper.run/build":            "true",
				"kipper.run/app":              "my-app",
				"kipper.run/source-namespace": "project-test",
			},
			Annotations: map[string]string{
				builder.SourceFingerprintAnnotation: builder.GitSourceFingerprint(app.Spec.Git),
			},
		},
		Status: batchv1.JobStatus{Active: 1},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var after kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &after))
	assert.Nil(t, after.Status.Build, "a running build for a removed source reported itself as building")
}

// Detach, then attach a different repository before the old job finishes. The
// app UID matches, no newer job exists because a source edit does not create
// one, and the app does have a source — so every earlier guard passes and the
// old repository's artefact deploys under the new one's name.
func TestBuildReconciler_ABuildFromAReplacedSourceWritesNothing(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Git = &kipperv1.AppGitSource{URL: "https://git.example.com/shop/repository-b.git", Branch: "main"}
	app.Spec.Image = "registry.example.com/shop/checkout:from-repository-b"

	job := buildJobForSource(t,
		&kipperv1.AppGitSource{URL: "https://git.example.com/shop/repository-a.git", Branch: "main"},
		batchv1.JobStatus{Succeeded: 1})

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var after kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &after))
	assert.Equal(t, "registry.example.com/shop/checkout:from-repository-b", after.Spec.Image,
		"an artefact built from the previous repository deployed under the new one's name")
}

// An edit to the branch produces a different artefact from the same repository,
// so a job started before it is just as stale.
func TestBuildReconciler_ABuildFromAReplacedBranchWritesNothing(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Git = &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "release"}
	app.Spec.Image = "registry.example.com/shop/checkout:current"

	job := buildJobForSource(t,
		&kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"},
		batchv1.JobStatus{Succeeded: 1})

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var after kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &after))
	assert.Equal(t, "registry.example.com/shop/checkout:current", after.Spec.Image)
}

// A build for the source the app still declares deploys as it always did.
func TestBuildReconciler_ABuildFromTheCurrentSourceStillDeploys(t *testing.T) {
	scheme := testScheme()

	source := &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"}
	app := newTestApp()
	app.Spec.Git = source
	app.Spec.Image = "old-image:latest"

	job := buildJobForSource(t, source, batchv1.JobStatus{Succeeded: 1})

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var after kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &after))
	assert.NotEqual(t, "old-image:latest", after.Spec.Image, "a current build failed to deploy")
}

// buildJobForSource is a build job carrying the fingerprint of the source it
// was created from, which is what the reconciler compares.
func buildJobForSource(t *testing.T, source *kipperv1.AppGitSource, status batchv1.JobStatus) *batchv1.Job {
	t.Helper()
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app-build-abc12345",
			Namespace: "project-test",
			Labels: map[string]string{
				"kipper.run/build":            "true",
				"kipper.run/app":              "my-app",
				"kipper.run/source-namespace": "project-test",
			},
			Annotations: map[string]string{
				"kipper.run/commit":                 "abc12345",
				builder.SourceFingerprintAnnotation: builder.GitSourceFingerprint(source),
			},
		},
		Status: status,
	}
}

// A build argument decides what the image contains, and editing one launches no
// new build — so an older job finishing afterwards would deploy an artefact
// built from arguments the app no longer declares.
func TestBuildReconciler_ABuildWithStaleBuildArgsWritesNothing(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Git = &kipperv1.AppGitSource{
		URL: "https://git.example.com/shop/checkout.git", Branch: "main",
		BuildArgs: map[string]string{"VERSION": "new"},
	}
	app.Spec.Image = "registry.example.com/shop/checkout:current"

	job := buildJobForSource(t, &kipperv1.AppGitSource{
		URL: "https://git.example.com/shop/checkout.git", Branch: "main",
		BuildArgs: map[string]string{"VERSION": "old"},
	}, batchv1.JobStatus{Succeeded: 1})

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var after kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &after))
	assert.Equal(t, "registry.example.com/shop/checkout:current", after.Spec.Image,
		"an image built from build arguments the app no longer declares was deployed")
}

// A job created before the fingerprint annotation
// existed cannot prove which source it built, and the presence check it used to
// fall back on asks the wrong question: the app still has a source, just not
// the one this job used. The window is open during the rollout of the very
// change that adds the annotation, when old and new console-api pods overlap.
func TestBuildReconciler_APreFingerprintBuildCannotProveItselfAndWritesNothing(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Git = &kipperv1.AppGitSource{
		URL: "https://git.example.com/shop/checkout.git", Branch: "main",
	}
	app.Spec.Image = "registry.example.com/shop/checkout:current"

	job := buildJobForSource(t, app.Spec.Git, batchv1.JobStatus{Succeeded: 1})
	delete(job.Annotations, builder.SourceFingerprintAnnotation)

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var after kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &after))
	assert.Equal(t, "registry.example.com/shop/checkout:current", after.Spec.Image,
		"a build that cannot prove which source it used deployed its artefact anyway")
}

// Discarding a finished build silently leaves the app on
// Building for ever, because nothing else writes a terminal phase for a build
// that produced no deploy. Being unable to tell why a push did not appear is
// the defect this whole change set started from.
func TestBuildReconciler_ADiscardedBuildSaysSoRatherThanStayingOnBuilding(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Git = &kipperv1.AppGitSource{
		URL: "https://git.example.com/shop/checkout.git", Branch: "release",
	}
	app.Status.Build = &kipperv1.AppBuildStatus{Phase: "Building"}

	job := buildJobForSource(t, &kipperv1.AppGitSource{
		URL: "https://git.example.com/shop/checkout.git", Branch: "main",
	}, batchv1.JobStatus{Succeeded: 1})

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var after kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &after))
	require.NotNil(t, after.Status.Build)
	assert.Equal(t, "Discarded", after.Status.Build.Phase,
		"the app is left on Building after a build that will never deploy")
	assert.NotEmpty(t, after.Status.Build.Message, "a discarded build must say why")
	assert.NotNil(t, after.Status.Build.CompletedAt,
		"the console renders a completion time beside the phase, and nothing else writes one here")
}

// A build still running is not discarded yet and Building is the truth, so the
// terminal phase belongs at the point it lands rather than while it runs.
func TestBuildReconciler_AStaleBuildStillRunningIsNotCalledDiscarded(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Git = &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "release"}
	app.Status.Build = &kipperv1.AppBuildStatus{Phase: "Building"}

	job := buildJobForSource(t, &kipperv1.AppGitSource{
		URL: "https://git.example.com/shop/checkout.git", Branch: "main",
	}, batchv1.JobStatus{Active: 1})

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var after kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &after))
	assert.Equal(t, "Building", after.Status.Build.Phase)
}

// Both sides of the comparison compute the fingerprint
// the same way, so a sentinel written when it cannot be computed would compare
// equal to itself and let exactly the completions it exists to catch through.
// This asserts the invariant rather than the branch that carries it, and it
// survives that branch being reverted: the state needs a marshal failure, which
// no CRD-representable field type produces, so the sentinel cannot be reached
// from a real source and the comparison rejects it anyway. Two mechanisms hold
// it, and a test can only reach one of them.
func TestStaleSourceReason_TheUnfingerprintableSentinelIsNeverAccepted(t *testing.T) {
	source := &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"}

	job := buildJobForSource(t, source, batchv1.JobStatus{Succeeded: 1})
	job.Annotations[builder.SourceFingerprintAnnotation] = builder.UnfingerprintableSource

	assert.NotEmpty(t, staleSourceReason(job, source),
		"a build whose source could not be recorded was accepted as belonging to this one")
	assert.NotEqual(t, builder.UnfingerprintableSource, builder.GitSourceFingerprint(source),
		"the sentinel collides with a real fingerprint, so a real source would be rejected")
}

// Discarded exists to replace a Building that will never
// resolve. A terminal phase is the truthful record of a completion that was
// already applied, and overwriting it is worst on upgrade day: the informer
// replays every job still inside its TTL, none of them carries a fingerprint,
// and every app that built in the last hour would be relabelled Discarded with
// a reason that is false for it.
func TestBuildReconciler_ADiscardedBuildDoesNotOverwriteAnAppliedCompletion(t *testing.T) {
	for _, applied := range []string{"Succeeded", "Failed"} {
		t.Run(applied, func(t *testing.T) {
			scheme := testScheme()

			app := newTestApp()
			app.Spec.Git = &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"}
			app.Status.Build = &kipperv1.AppBuildStatus{Phase: applied, Message: "out of memory, raise build-memory"}

			job := buildJobForSource(t, app.Spec.Git, batchv1.JobStatus{Succeeded: 1})
			delete(job.Annotations, builder.SourceFingerprintAnnotation)

			fakeClient := crfake.NewClientBuilder().
				WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
			reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

			_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"},
			})
			require.NoError(t, err)

			var after kipperv1.App
			require.NoError(t, fakeClient.Get(context.Background(),
				types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &after))
			assert.Equal(t, applied, after.Status.Build.Phase,
				"a completion that was already applied was relabelled as discarded")
			assert.Contains(t, after.Status.Build.Message, "build-memory",
				"the actionable message from the real completion was lost")
		})
	}
}

// The reconcile's UID guard runs against the read at the
// top, and the status write does its own. An App deleted and a same-named one
// recreated in between is exactly what the UID label exists to catch, so the
// write has to check it too rather than trusting a decision made earlier.
func TestBuildReconciler_ABuildStatusIsNotWrittenOntoARecreatedApp(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.UID = "the-app-that-exists-now"
	app.Spec.Git = &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).WithObjects(app).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	require.NoError(t, reconciler.updateBuildStatus(context.Background(), "project-test", "my-app",
		"the-app-that-was-deleted", "my-app-build-abc12345", "Building", "", ""))

	var after kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &after))
	assert.Nil(t, after.Status.Build,
		"a build belonging to a deleted app wrote its status onto the app that replaced it")
}

// The job is terminal by the time it is discarded, so no
// further job event will arrive and nothing else re-enqueues this reconcile. A
// status write dropped on a resource-version conflict therefore leaves the app
// on Building for ever, which is the defect the phase was added to close.
func TestBuildReconciler_ADiscardedStatusThatFailsToWriteIsRetried(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Git = &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "release"}
	app.Status.Build = &kipperv1.AppBuildStatus{Phase: "Building"}

	job := buildJobForSource(t, &kipperv1.AppGitSource{
		URL: "https://git.example.com/shop/checkout.git", Branch: "main",
	}, batchv1.JobStatus{Succeeded: 1})

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, crclient.Client, string, crclient.Object, ...crclient.SubResourceUpdateOption) error {
				return apierrors.NewConflict(schema.GroupResource{Resource: "apps"}, "my-app", errors.New("the object has been modified"))
			},
		}).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"},
	})

	require.Error(t, err, "the reconcile reported success, so nothing will ever retry the write")
}

// Discarded's write is retried; these two were still
// best-effort, and the same reasoning applies to them verbatim. The job is
// terminal, so nothing re-enqueues, and a write lost to a conflict with the App
// reconciler leaves the CR on Building for an app that deployed fine.
func TestBuildReconciler_ACompletedBuildStatusThatFailsToWriteIsRetried(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status batchv1.JobStatus
	}{
		{"succeeded", batchv1.JobStatus{Succeeded: 1}},
		{"failed", batchv1.JobStatus{Failed: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := testScheme()

			app := newTestApp()
			app.Spec.Git = &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"}
			app.Status.Build = &kipperv1.AppBuildStatus{Phase: "Building"}

			job := buildJobForSource(t, app.Spec.Git, tc.status)

			fakeClient := crfake.NewClientBuilder().
				WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourceUpdate: func(context.Context, crclient.Client, string, crclient.Object, ...crclient.SubResourceUpdateOption) error {
						return apierrors.NewConflict(schema.GroupResource{Resource: "apps"}, "my-app", errors.New("the object has been modified"))
					},
				}).Build()
			reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

			_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"},
			})

			require.Error(t, err, "the reconcile reported success, so nothing will ever retry the write")
		})
	}
}

// Making the status write retryable made
// this common rather than creating it: the spec write above wakes the App
// reconciler, whose own status write is what conflicts, and the requeue re-runs
// the whole succeeded branch. An informer replay inside the job's TTL does the
// same. Duplicates evict real rollback targets, so "roll back to previous"
// becomes a no-op at the moment someone reaches for it.
func TestBuildReconciler_ARebuiltCompletionDoesNotDuplicateItsDeployHistory(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Git = &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"}

	job := buildJobForSource(t, app.Spec.Git, batchv1.JobStatus{Succeeded: 1})

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"}}
	for i := 0; i < 3; i++ {
		_, err := reconciler.Reconcile(context.Background(), request)
		require.NoError(t, err)
	}

	var after kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &after))

	var history []buildDeployEntry
	require.NoError(t, json.Unmarshal([]byte(after.Annotations[deployHistoryAnnKey]), &history))
	assert.Len(t, history, 1, "the same build was recorded more than once, evicting real rollback targets")
}

// The terminal-phase guard protects a completion that was already applied. It
// could not tell that apart from a completion belonging to an *earlier* build,
// which is what the app still carries when the current build's Pending write is
// dropped. So a build correctly refused for a stale source wrote no verdict at
// all, the previous build's Succeeded stayed, and the job's own Succeeded is
// what the API then reported: refused artefact, success everywhere.
func TestBuildReconciler_ADiscardedBuildOverwritesAnEarlierBuildsCompletion(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Git = &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "release"}
	// The completion of the build before this one, never replaced because this
	// build's Pending write lost a conflict.
	app.Status.Build = &kipperv1.AppBuildStatus{Phase: "Succeeded", Build: "my-app-build-earlier"}

	job := buildJobForSource(t, &kipperv1.AppGitSource{
		URL: "https://git.example.com/shop/checkout.git", Branch: "main",
	}, batchv1.JobStatus{Succeeded: 1})

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var after kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &after))
	assert.Equal(t, "Discarded", after.Status.Build.Phase,
		"a refused build left an earlier build's success standing as its own verdict")
	assert.Equal(t, "my-app-build-abc12345", after.Status.Build.Build,
		"the verdict must name the build it is about")
}

// The case the guard exists for is unchanged: this job's own applied completion
// is not relabelled when the informer replays it.
func TestBuildReconciler_AReplayedJobDoesNotRelabelItsOwnCompletion(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Git = &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "main"}
	app.Status.Build = &kipperv1.AppBuildStatus{
		Phase: "Succeeded", Build: "my-app-build-abc12345", Message: "out of memory, raise build-memory",
	}

	job := buildJobForSource(t, app.Spec.Git, batchv1.JobStatus{Succeeded: 1})
	delete(job.Annotations, builder.SourceFingerprintAnnotation)

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var after kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &after))
	assert.Equal(t, "Succeeded", after.Status.Build.Phase,
		"a replayed job relabelled the completion it had already applied")
	assert.Contains(t, after.Status.Build.Message, "build-memory")
}

// The compatibility direction for a status with no build name was too broad. A
// completion written before the field existed was treated as belonging to any
// job, so the first build after the upgrade whose Pending write was dropped and
// whose source was then edited wrote no verdict, and the live job's own success
// was reported for a build that deployed nothing. The reconciled job says which
// era it belongs to: a job carrying a source fingerprint cannot be the one that
// wrote a status from before the fingerprint existed.
func TestBuildReconciler_ALegacyCompletionDoesNotSuppressACurrentBuildsVerdict(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Git = &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "release"}
	// Written before the identity field existed, and never replaced.
	app.Status.Build = &kipperv1.AppBuildStatus{Phase: "Succeeded"}

	// A job created after the upgrade: it carries a fingerprint, and its source
	// no longer matches the app's.
	job := buildJobForSource(t, &kipperv1.AppGitSource{
		URL: "https://git.example.com/shop/checkout.git", Branch: "main",
	}, batchv1.JobStatus{Succeeded: 1})

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var after kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &after))
	assert.Equal(t, "Discarded", after.Status.Build.Phase,
		"a completion from before the upgrade stood in for a build that deployed nothing")
	assert.Equal(t, "my-app-build-abc12345", after.Status.Build.Build)
}

// A job being deleted must not drive a status write. The TTL controller deletes
// a finished job with foreground propagation, which stamps a deletion timestamp
// and produces an update event while the job is still readable — turning a
// replay that needs a restart into one that arrives on a timer.
func TestBuildReconciler_AJobBeingDeletedWritesNothing(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Git = &kipperv1.AppGitSource{URL: "https://git.example.com/shop/checkout.git", Branch: "release"}
	app.Status.Build = &kipperv1.AppBuildStatus{Phase: "Succeeded", Build: "my-app-build-newer"}
	app.Spec.Image = "registry.example.com/shop/checkout:current"

	job := buildJobForSource(t, &kipperv1.AppGitSource{
		URL: "https://git.example.com/shop/checkout.git", Branch: "main",
	}, batchv1.JobStatus{Succeeded: 1})
	deleting := metav1.Now()
	job.DeletionTimestamp = &deleting
	job.Finalizers = []string{"foregroundDeletion"}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).WithObjects(app, job).WithStatusSubresource(app).Build()
	reconciler := &BuildReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app-build-abc12345", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var after kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &after))
	assert.Equal(t, "Succeeded", after.Status.Build.Phase,
		"a job on its way out relabelled a newer build's applied completion")
	assert.Equal(t, "my-app-build-newer", after.Status.Build.Build)
	assert.Equal(t, "registry.example.com/shop/checkout:current", after.Spec.Image)
}
