package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func buildPodInBuildsNS() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-build-abc123",
			Namespace: "kipper-builds",
			Labels: map[string]string{
				"kipper.run/build":            "true",
				"kipper.run/app":              "api",
				"kipper.run/source-namespace": "blog",
			},
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "build"}},
			Containers:     []corev1.Container{{Name: "push"}},
		},
		Status: corev1.PodStatus{
			Phase:                 corev1.PodSucceeded,
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "build", State: stageDone}},
			ContainerStatuses:     []corev1.ContainerStatus{{Name: "push", State: stageDone}},
		},
	}
}

// The build pod lives in the shared kipper-builds namespace, not the tenant's
// project namespace, so BuildLogs must read logs from where GetBuildPod found
// the pod. This guards the namespace the GetLogs request targets.
func TestBuildLogs_ReadsFromBuildsNamespace(t *testing.T) {
	client := fake.NewClientset(buildPodInBuildsNS())
	h := &Webhooks{Client: client}

	r := chi.NewRouter()
	r.Get("/projects/{name}/apps/{app}/build/logs", h.BuildLogs)
	req := httptest.NewRequest(http.MethodGet, "/projects/blog/apps/api/build/logs", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var logNamespaces []string
	for _, action := range client.Actions() {
		if action.GetVerb() == "get" && action.GetResource().Resource == "pods" && action.GetSubresource() == "log" {
			logNamespaces = append(logNamespaces, action.GetNamespace())
		}
	}
	require.NotEmpty(t, logNamespaces, "expected at least one pod/log request")
	for _, ns := range logNamespaces {
		assert.Equal(t, "kipper-builds", ns, "logs must be read from the build namespace, not the project namespace")
	}
}

// A missing build pod is a 404, not a 500 — the tenant namespace holds no build
// pods now, so a lookup there would silently find nothing.
func TestBuildLogs_NoBuildPodIs404(t *testing.T) {
	client := fake.NewClientset()
	h := &Webhooks{Client: client}

	r := chi.NewRouter()
	r.Get("/projects/{name}/apps/{app}/build/logs", h.BuildLogs)
	req := httptest.NewRequest(http.MethodGet, "/projects/blog/apps/api/build/logs", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

var checkoutBuildLabels = map[string]string{"kipper.run/build": "true", "kipper.run/app": "checkout", "kipper.run/source-namespace": "shop-test"}

func queuedCheckoutBuild(created time.Time) *batchv1.Job {
	suspended := true
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout-build-queued", Namespace: "kipper-builds",
			CreationTimestamp: metav1.NewTime(created),
			Labels:            checkoutBuildLabels,
		},
		Spec: batchv1.JobSpec{Suspend: &suspended},
	}
}

func checkoutBuildPod(name, job, container string, created time.Time) *corev1.Pod {
	labels := map[string]string{batchv1.JobNameLabel: job}
	for k, v := range checkoutBuildLabels {
		labels[k] = v
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kipper-builds", Labels: labels, CreationTimestamp: metav1.NewTime(created)},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: container}}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{{Name: container, State: stageDone}},
		},
	}
}

var (
	stageWaiting = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}
	stageRunning = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	stageDone    = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
)

func releasedCheckoutBuild(created time.Time) *batchv1.Job {
	job := queuedCheckoutBuild(created)
	job.Name = "checkout-build-released"
	job.Spec.Suspend = nil
	return job
}

func fastBuildLogPolling(t *testing.T) {
	t.Helper()
	previous := buildLogPollInterval
	buildLogPollInterval = time.Millisecond
	t.Cleanup(func() { buildLogPollInterval = previous })
}

// The queue has released the newest build, but the Job controller has not
// created its pod yet. The finished previous build's pod is still around, and
// its log must not stand in for the build being started.
func TestBuildLogs_ReleasedBuildWithoutAPodWaitsForItsOwnPod(t *testing.T) {
	fastBuildLogPolling(t)
	now := time.Now()
	client := fake.NewClientset(
		releasedCheckoutBuild(now.Add(-time.Minute)),
		checkoutBuildPod("checkout-build-earlier-x1", "checkout-build-earlier", "earlier-stage", now),
	)
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = client.CoreV1().Pods("kipper-builds").Create(context.Background(),
			checkoutBuildPod("checkout-build-released-x1", "checkout-build-released", "kaniko", now), metav1.CreateOptions{})
	}()

	rec := getCheckoutBuildLogs(t, client)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Starting the build.\n")
	assert.NotContains(t, body, "Waiting for a build slot", "a released build is starting, not queued")
	assert.Contains(t, body, "--- kaniko ---", "the released build's own pod was not streamed")
	assert.NotContains(t, body, "earlier-stage", "the previous build's log was shown for the build being started")
}

// A finished build whose pod is gone has nothing to stream, and an earlier
// build's pod is no substitute.
func TestBuildLogs_FinishedBuildWithoutAPodSaysItHasNoLog(t *testing.T) {
	now := time.Now()
	finished := releasedCheckoutBuild(now.Add(-time.Minute))
	finished.Status.Succeeded = 1
	client := fake.NewClientset(
		finished,
		checkoutBuildPod("checkout-build-earlier-x1", "checkout-build-earlier", "earlier-stage", now),
	)

	rec := getCheckoutBuildLogs(t, client)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "This build has no log to show.")
	assert.NotContains(t, body, "earlier-stage")
}

// A pod can exist while its stages still wait to start. Asking for a stage's
// log before it starts fails, which ended the stream with nothing but the
// waiting line.
func TestBuildLogs_WaitsForEachStageToStart(t *testing.T) {
	fastBuildLogPolling(t)
	snapshot := func(phase corev1.PodPhase, clone, kaniko corev1.ContainerState) *corev1.Pod {
		p := checkoutBuildPod("checkout-build-released-x1", "checkout-build-released", "kaniko", time.Now())
		p.Spec.InitContainers = []corev1.Container{{Name: "clone"}}
		p.Status.Phase = phase
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "clone", State: clone}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "kaniko", State: kaniko}}
		return p
	}
	first := snapshot(corev1.PodPending, stageWaiting, stageWaiting)
	pods := []*corev1.Pod{
		first, first, first,
		snapshot(corev1.PodPending, stageRunning, stageWaiting),
		snapshot(corev1.PodPending, stageDone, stageWaiting),
		snapshot(corev1.PodRunning, stageDone, stageRunning),
		snapshot(corev1.PodSucceeded, stageDone, stageDone),
	}
	client := fake.NewClientset(releasedCheckoutBuild(time.Now()), first)
	served := 0
	var early []string
	client.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "log" {
			current := pods[max(min(served, len(pods))-1, 0)]
			opts := action.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
			if !podStageStarted(current, opts.Container) {
				early = append(early, opts.Container)
			}
			return true, &corev1.Pod{}, nil
		}
		next := pods[min(served, len(pods)-1)]
		served++
		return true, next.DeepCopy(), nil
	})

	rec := getCheckoutBuildLogs(t, client)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, early, "a stage's log was requested before the stage started")
	body := rec.Body.String()
	assert.Contains(t, body, "--- clone ---")
	assert.Contains(t, body, "--- kaniko ---")
}

func podStageStarted(p *corev1.Pod, name string) bool {
	for _, s := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if s.Name == name {
			return s.State.Running != nil || s.State.Terminated != nil
		}
	}
	return false
}

func getCheckoutBuildLogs(t *testing.T, client *fake.Clientset) *httptest.ResponseRecorder {
	t.Helper()
	handler := &Webhooks{Client: client}
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{name}/apps/{app}/build/logs", handler.BuildLogs)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/shop-test/apps/checkout/build/logs", nil))
	return rec
}

// A previous build's pod can remain until its TTL expires. Even if that pod
// is newer, the waiting notice must lead to the queued build's own logs.
func TestBuildLogs_QueuedBuildSaysItIsWaitingThenStreamsItsOwnPod(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	fastBuildLogPolling(t)
	now := time.Now()
	client := fake.NewClientset(
		queuedCheckoutBuild(now.Add(-time.Minute)),
		checkoutBuildPod("checkout-build-earlier-x1", "checkout-build-earlier", "earlier-stage", now),
	)
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = client.CoreV1().Pods("kipper-builds").Create(context.Background(),
			checkoutBuildPod("checkout-build-queued-x1", "checkout-build-queued", "kaniko", now), metav1.CreateOptions{})
	}()

	rec := getCheckoutBuildLogs(t, client)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Waiting for a build slot. Builds run one at a time on this cluster.\n")
	assert.Contains(t, body, "--- kaniko ---", "the queued build's own pod was not streamed")
	assert.NotContains(t, body, "earlier-stage", "the previous build's log was shown for the queued build")
}

func TestBuildLogs_QueuedBuildThatNeverStartsEndsTheStream(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "2")
	client := fake.NewClientset(queuedCheckoutBuild(time.Now()))
	client.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(batchv1.Resource("jobs"), "checkout-build-queued")
	})

	rec := getCheckoutBuildLogs(t, client)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Waiting for a build slot. This cluster runs up to 2 builds at a time.\n")
	assert.Contains(t, body, "This build left the queue without starting")
}

func TestBuildStatus_QueuedBuildReadsAsWaitingForASlot(t *testing.T) {
	t.Setenv("BUILD_MAX_CONCURRENT", "")
	appCR := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-test"},
		Spec:       kipperv1.AppSpec{Image: "registry.example.com/shop/checkout:current"},
	}
	handler := &Webhooks{Client: fake.NewClientset(queuedCheckoutBuild(time.Now())), CRClient: testCRClient(appCR)}
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{name}/apps/{app}/build/status", handler.BuildStatus)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/shop-test/apps/checkout/build/status", nil))

	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "Pending", got["phase"])
	assert.Equal(t, "Waiting for a build slot. Builds run one at a time on this cluster.", got["message"])
}
