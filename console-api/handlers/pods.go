package handlers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Pods provides handlers for listing running pods of an app.
type Pods struct {
	Client kubernetes.Interface
}

type podsResponse struct {
	Pods []string `json:"pods"`
}

// List returns the names of all running pods for an app.
// GET /api/v1/projects/{name}/apps/{app}/pods
func (p *Pods) List(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	pods, err := p.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", app),
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to list pods: %v", err))
		return
	}

	var names []string
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodRunning {
			names = append(names, pod.Name)
		}
	}

	if names == nil {
		names = []string{}
	}

	respondJSON(w, http.StatusOK, podsResponse{Pods: names})
}

// terminationInfo is what a container recorded when it stopped.
type terminationInfo struct {
	Reason     string `json:"reason,omitempty"`
	ExitCode   int32  `json:"exit_code"`
	Message    string `json:"message,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// containerHealth is one container's state and, when it has failed, the reason
// it did.
type containerHealth struct {
	Name            string           `json:"name"`
	Ready           bool             `json:"ready"`
	Restarts        int32            `json:"restarts"`
	State           string           `json:"state"`
	Reason          string           `json:"reason,omitempty"`
	Message         string           `json:"message,omitempty"`
	ExitCode        *int32           `json:"exit_code,omitempty"`
	LastTermination *terminationInfo `json:"last_termination,omitempty"`
	// Log is the tail of what the container last printed. Kubernetes reports
	// "Error, exit 1" for anything a process exits non-zero on, so the reason
	// is almost always here rather than in the status beside it.
	Log string `json:"log,omitempty"`
}

// podHealth is one pod's phase and the state of every container in it.
type podHealth struct {
	Name           string            `json:"name"`
	Phase          string            `json:"phase"`
	InitContainers []containerHealth `json:"init_containers"`
	Containers     []containerHealth `json:"containers"`
}

// workloadHealth answers "why is this workload not running".
type workloadHealth struct {
	Pods []podHealth `json:"pods"`
}

// Health returns the container-level state of every pod belonging to an app,
// including the pods that are not running — which are the ones an operator
// opens this for.
//
// List above deliberately reports only Running pods, because it feeds the
// shell and log pickers where a dead pod is no use. That filter is why a
// crashing app looked like an app with no pods at all, so this endpoint exists
// beside it rather than changing it.
//
// A container's reason lives in one of two places depending on whether it is
// between restarts: the waiting state carries CrashLoopBackOff or
// ImagePullBackOff, while the exit code and the process's own message are in
// LastTerminationState. Reporting only the first tells an operator that a
// container is restarting without ever saying what killed it.
//
// GET /api/v1/projects/{name}/apps/{app}/health
func (p *Pods) Health(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	pods, err := p.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", app),
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to list pods: %v", err))
		return
	}

	health := workloadHealth{Pods: []podHealth{}}
	for _, pod := range pods.Items {
		entry := podHealth{
			Name:           pod.Name,
			Phase:          string(pod.Status.Phase),
			InitContainers: containerHealthFor(pod.Status.InitContainerStatuses),
			Containers:     containerHealthFor(pod.Status.ContainerStatuses),
		}
		p.attachFailureLogs(ctx, pod.Name, namespace, entry.InitContainers)
		p.attachFailureLogs(ctx, pod.Name, namespace, entry.Containers)
		health.Pods = append(health.Pods, entry)
	}

	respondJSON(w, http.StatusOK, health)
}

// containerHealthFor flattens container statuses into the shape the console
// renders. An empty slice rather than nil, so the console distinguishes "no
// containers" from a field it failed to read.
func containerHealthFor(statuses []corev1.ContainerStatus) []containerHealth {
	out := make([]containerHealth, 0, len(statuses))
	for _, cs := range statuses {
		c := containerHealth{
			Name:     cs.Name,
			Ready:    cs.Ready,
			Restarts: cs.RestartCount,
		}
		switch {
		case cs.State.Running != nil:
			c.State = "running"
		case cs.State.Terminated != nil:
			t := cs.State.Terminated
			c.State = "terminated"
			c.Reason = t.Reason
			c.Message = t.Message
			exit := t.ExitCode
			c.ExitCode = &exit
		case cs.State.Waiting != nil:
			c.State = "waiting"
			c.Reason = cs.State.Waiting.Reason
			c.Message = cs.State.Waiting.Message
		default:
			c.State = "unknown"
		}
		if t := cs.LastTerminationState.Terminated; t != nil {
			c.LastTermination = &terminationInfo{
				Reason:     t.Reason,
				ExitCode:   t.ExitCode,
				Message:    t.Message,
				FinishedAt: t.FinishedAt.UTC().Format(time.RFC3339),
			}
		}
		out = append(out, c)
	}
	return out
}

// failureLogLines is how much of a dying container's output is carried. Enough
// for a stack trace's cause, short enough that a page of them is still a page.
const failureLogLines = 20

// attachFailureLogs fetches the tail of what each failing container printed.
//
// A status of "Error, exit 1" is what Kubernetes reports for every non-zero
// exit, so on its own it names no cause. The cause was in the log the whole
// time: a duplicate database index, a missing environment variable, a port
// already bound. Reporting the status without it tells an operator that
// something is wrong and leaves them exactly where they started.
//
// Only failing containers are read, and a log that cannot be fetched is left
// empty rather than failing the response: the states beside it are still worth
// having, and a pod too young to have logs is the common case.
func (p *Pods) attachFailureLogs(ctx context.Context, pod, namespace string, containers []containerHealth) {
	for i := range containers {
		if containers[i].State == "running" && containers[i].Restarts == 0 {
			continue
		}
		// A container between restarts has its useful output in the previous
		// instance; the current one has only just started.
		previous := containers[i].LastTermination != nil
		containers[i].Log = p.tailContainerLog(ctx, pod, namespace, containers[i].Name, previous)
	}
}

func (p *Pods) tailContainerLog(ctx context.Context, pod, namespace, container string, previous bool) string {
	lines := int64(failureLogLines)
	stream, err := p.Client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		TailLines: &lines,
		Previous:  previous,
	}).Stream(ctx)
	if err != nil {
		return ""
	}
	defer func() { _ = stream.Close() }()

	out, err := io.ReadAll(io.LimitReader(stream, 64<<10))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
