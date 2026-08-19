package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// healthRouter wires the real route so the test drives what production serves,
// rather than calling the handler with a hand-built chi context.
func healthRouter(p *Pods) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{name}/apps/{app}/health", p.Health)
	return r
}

func getHealth(t *testing.T, objects ...*corev1.Pod) workloadHealth {
	t.Helper()
	client := fake.NewSimpleClientset()
	for _, pod := range objects {
		if _, err := client.CoreV1().Pods(pod.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("staging pod: %v", err)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/shop-test/apps/checkout/health", nil)
	healthRouter(&Pods{Client: client}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got workloadHealth
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return got
}

func appPod(name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "shop-test",
			Labels:    map[string]string{"app": "checkout"},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// The failure this endpoint exists for: a container that starts, dies, and is
// restarted carries its reason only in LastTerminationState, and that is the
// one an operator needs to read.
func TestHealthReportsWhyACrashingContainerDied(t *testing.T) {
	pod := appPod("checkout-abc", corev1.PodRunning)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "checkout",
		Ready:        false,
		RestartCount: 3,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason:  "CrashLoopBackOff",
				Message: "back-off 40s restarting failed container",
			},
		},
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				Reason:     "Error",
				ExitCode:   1,
				Message:    "migration failed: relation already exists",
				FinishedAt: metav1.NewTime(time.Unix(1700000000, 0).UTC()),
			},
		},
	}}

	got := getHealth(t, pod)

	if len(got.Pods) != 1 {
		t.Fatalf("pods = %d, want 1", len(got.Pods))
	}
	containers := got.Pods[0].Containers
	if len(containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(containers))
	}
	c := containers[0]
	if c.State != "waiting" {
		t.Errorf("state = %q, want waiting", c.State)
	}
	if c.Reason != "CrashLoopBackOff" {
		t.Errorf("reason = %q, want CrashLoopBackOff", c.Reason)
	}
	if c.Restarts != 3 {
		t.Errorf("restarts = %d, want 3", c.Restarts)
	}
	if c.LastTermination == nil {
		t.Fatal("last termination is missing, which is the only place the cause is recorded")
	}
	if c.LastTermination.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", c.LastTermination.ExitCode)
	}
	if c.LastTermination.Message != "migration failed: relation already exists" {
		t.Errorf("message = %q, want the container's own message", c.LastTermination.Message)
	}
}

// A pod that never starts reports the waiting reason on the container, with no
// termination to read: an image that cannot be pulled is the common case.
func TestHealthReportsAContainerThatNeverStarted(t *testing.T) {
	pod := appPod("checkout-def", corev1.PodPending)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "checkout",
		Ready: false,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason:  "ImagePullBackOff",
				Message: `Back-off pulling image "registry.example.com/shop/checkout:9f2c1a"`,
			},
		},
	}}

	got := getHealth(t, pod)

	c := got.Pods[0].Containers[0]
	if c.Reason != "ImagePullBackOff" {
		t.Errorf("reason = %q, want ImagePullBackOff", c.Reason)
	}
	if c.LastTermination != nil {
		t.Errorf("last termination = %+v, want none for a container that never ran", c.LastTermination)
	}
}

// Init containers are where a build's clone step fails, so leaving them out
// would hide the reason on exactly the pods that need one.
func TestHealthIncludesInitContainers(t *testing.T) {
	pod := appPod("checkout-ghi", corev1.PodPending)
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "clone",
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				Reason:   "Error",
				ExitCode: 128,
				Message:  "could not read Username for 'https://git.example.com'",
			},
		},
	}}

	got := getHealth(t, pod)

	if len(got.Pods[0].InitContainers) != 1 {
		t.Fatalf("init containers = %d, want 1", len(got.Pods[0].InitContainers))
	}
	c := got.Pods[0].InitContainers[0]
	if c.State != "terminated" {
		t.Errorf("state = %q, want terminated", c.State)
	}
	if c.ExitCode == nil || *c.ExitCode != 128 {
		t.Errorf("exit code = %v, want 128", c.ExitCode)
	}
}

// A working app reports running with nothing alarming attached, so the console
// can tell "healthy" from "no information".
func TestHealthReportsARunningContainerPlainly(t *testing.T) {
	pod := appPod("checkout-jkl", corev1.PodRunning)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "checkout",
		Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}

	got := getHealth(t, pod)

	c := got.Pods[0].Containers[0]
	if c.State != "running" {
		t.Errorf("state = %q, want running", c.State)
	}
	if !c.Ready {
		t.Error("ready = false, want true")
	}
	if c.Reason != "" || c.LastTermination != nil {
		t.Errorf("running container carries failure detail: %+v", c)
	}
}

// The pods endpoint beside this one filters to Running, which hides precisely
// the pods an operator is looking for. This one must not.
func TestHealthIncludesPodsThatAreNotRunning(t *testing.T) {
	got := getHealth(t, appPod("checkout-mno", corev1.PodPending), appPod("checkout-pqr", corev1.PodFailed))

	if len(got.Pods) != 2 {
		t.Fatalf("pods = %d, want 2: a pending and a failed pod are the ones worth reporting", len(got.Pods))
	}
}

// An app with no pods at all is an answer, not an error.
func TestHealthReportsNoPodsAsAnEmptyList(t *testing.T) {
	got := getHealth(t)

	if got.Pods == nil {
		t.Fatal("pods = nil, want an empty list so the console renders an empty state rather than a failure")
	}
	if len(got.Pods) != 0 {
		t.Fatalf("pods = %d, want 0", len(got.Pods))
	}
}

// "Error, exit 1" is what Kubernetes reports for every non-zero exit, so on its
// own it names no cause. In the live incident the cause was one line of the
// container's log, and reading it needed kubectl.
func TestHealthCarriesTheLogOfAFailingContainer(t *testing.T) {
	pod := appPod("checkout-abc", corev1.PodRunning)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "checkout",
		RestartCount: 3,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
		},
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1},
		},
	}}

	got := getHealth(t, pod)

	// The fake clientset serves a fixed body for any log request, so what this
	// pins is that a failing container is asked about at all.
	if got.Pods[0].Containers[0].Log == "" {
		t.Error("no log was carried for a container that is crashing, which is where the cause is")
	}
}

// A healthy container's log is not fetched: it is not why anyone opened this,
// and a request per container per poll is a cost with nothing to show for it.
func TestHealthDoesNotFetchLogsForAHealthyContainer(t *testing.T) {
	pod := appPod("checkout-jkl", corev1.PodRunning)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "checkout",
		Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}

	got := getHealth(t, pod)

	if got.Pods[0].Containers[0].Log != "" {
		t.Error("a running container's log was fetched, which nothing asked for")
	}
}
