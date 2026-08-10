package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/console-api/ai"
)

const diagnosePrompt = `You are the Kipper AI error diagnostician. A Kubernetes application is failing and the user needs help.

Analyse the pod status, events, and logs below. Provide:
1. A clear one-line summary of the problem
2. The root cause
3. Specific fix instructions (exact commands or config changes)

Be concise and direct. The user may not know Kubernetes — explain in plain language.
Do not suggest "check the logs" — you already have them. Give the answer.`

// AIDiagnose provides AI-powered error diagnosis for failing apps.
type AIDiagnose struct {
	Client   kubernetes.Interface
	Settings *AISettings
}

// Diagnose gathers pod status, events, and logs for a failing app and streams an AI diagnosis.
// POST /api/v1/projects/{name}/apps/{app}/diagnose
func (a *AIDiagnose) Diagnose(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")
	a.streamDiagnosis(w, r, project, app, "application")
}

// DiagnoseFunction gathers pod status, events, and logs for a failing
// function (cron pod, test run, scaled-out HTTP pod) and streams an
// AI diagnosis. Functions use the same `app=<name>` pod label as apps,
// so the gather logic is identical — only the routing and the
// human-facing label in the prompt differ.
// POST /api/v1/projects/{name}/functions/{fn}/diagnose
func (a *AIDiagnose) DiagnoseFunction(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	fn := chi.URLParam(r, "fn")
	a.streamDiagnosis(w, r, project, fn, "function")
}

// streamDiagnosis is the shared workhorse: gather context, ask the AI
// provider, stream the answer back as SSE. The kind ("application" or
// "function") only changes the human-facing prefix in the prompt
// message — the diagnostic prompt and gather logic are unchanged.
func (a *AIDiagnose) streamDiagnosis(w http.ResponseWriter, r *http.Request, namespace, name, kind string) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	cfg, err := a.Settings.GetRaw(ctx)
	if err != nil || cfg.Provider == "" {
		respondError(w, http.StatusBadRequest, "AI not configured — go to Settings to add a provider")
		return
	}

	diagnostic := a.gatherDiagnostics(ctx, namespace, name)

	provider, err := ai.NewProvider(ai.Config{
		Provider:  cfg.Provider,
		APIKey:    cfg.APIKey,
		Model:     cfg.Model,
		OllamaURL: cfg.OllamaURL,
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create AI provider: %v", err))
		return
	}

	messages := []ai.Message{
		{Role: "user", Content: fmt.Sprintf("Diagnose this failing %s:\n\n%s", kind, diagnostic)},
	}

	stream, err := provider.Chat(ctx, diagnosePrompt, messages)
	if err != nil {
		respondError(w, http.StatusBadGateway, fmt.Sprintf("AI provider error: %v", err))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		respondError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	for chunk := range stream {
		if chunk.Err != nil {
			_, _ = fmt.Fprintf(w, "data: {\"error\": %q}\n\n", chunk.Err.Error())
			flusher.Flush()
			return
		}

		if chunk.Done {
			_, _ = fmt.Fprintf(w, "data: {\"done\": true}\n\n")
			flusher.Flush()
			return
		}

		data, _ := json.Marshal(map[string]string{"content": chunk.Content})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
}

func (a *AIDiagnose) gatherDiagnostics(ctx context.Context, namespace, app string) string {
	var b strings.Builder

	// Pod statuses
	pods, err := a.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", app),
	})
	if err == nil && len(pods.Items) > 0 {
		b.WriteString("## Pod Status\n\n")
		for _, pod := range pods.Items {
			_, _ = fmt.Fprintf(&b, "Pod: %s  Phase: %s\n", pod.Name, pod.Status.Phase)

			for _, cs := range pod.Status.ContainerStatuses {
				_, _ = fmt.Fprintf(&b, "  Container: %s  Ready: %v  Restarts: %d\n", cs.Name, cs.Ready, cs.RestartCount)

				if cs.State.Waiting != nil {
					_, _ = fmt.Fprintf(&b, "  State: Waiting — %s: %s\n", cs.State.Waiting.Reason, cs.State.Waiting.Message)
				}
				if cs.State.Terminated != nil {
					_, _ = fmt.Fprintf(&b, "  State: Terminated — %s (exit %d): %s\n", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode, cs.State.Terminated.Message)
				}
				if cs.LastTerminationState.Terminated != nil {
					t := cs.LastTerminationState.Terminated
					_, _ = fmt.Fprintf(&b, "  Last termination: %s (exit %d): %s\n", t.Reason, t.ExitCode, t.Message)
				}
			}
			b.WriteString("\n")
		}
	} else {
		b.WriteString("## Pod Status\n\nNo pods found for this application.\n\n")
	}

	// Events
	events, err := a.Client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s", app),
	})
	if err == nil && len(events.Items) > 0 {
		b.WriteString("## Recent Events\n\n")
		for _, e := range events.Items {
			_, _ = fmt.Fprintf(&b, "[%s] %s: %s\n", e.Reason, e.InvolvedObject.Kind, e.Message)
		}
		b.WriteString("\n")
	}

	// Also get pod-level events
	if pods != nil {
		for _, pod := range pods.Items {
			podEvents, err := a.Client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
				FieldSelector: fmt.Sprintf("involvedObject.name=%s", pod.Name),
			})
			if err == nil && len(podEvents.Items) > 0 {
				_, _ = fmt.Fprintf(&b, "## Events for pod %s\n\n", pod.Name)
				for _, e := range podEvents.Items {
					_, _ = fmt.Fprintf(&b, "[%s] %s\n", e.Reason, e.Message)
				}
				b.WriteString("\n")
			}
		}
	}

	// Recent logs from the first pod (last 50 lines)
	if pods != nil && len(pods.Items) > 0 {
		pod := pods.Items[0]
		container := appContainerFromPod(&pod)
		logOpts := &corev1.PodLogOptions{Container: container, TailLines: int64Ptr(50)}

		// Try current container logs
		req := a.Client.CoreV1().Pods(namespace).GetLogs(pod.Name, logOpts)
		logBytes, err := req.DoRaw(ctx)
		if err == nil && len(logBytes) > 0 {
			b.WriteString("## Container Logs (last 50 lines)\n\n```\n")
			b.Write(logBytes)
			b.WriteString("```\n\n")
		}

		// Try previous container logs if the container restarted
		prevOpts := &corev1.PodLogOptions{Container: container, TailLines: int64Ptr(50), Previous: true}
		prevReq := a.Client.CoreV1().Pods(namespace).GetLogs(pod.Name, prevOpts)
		prevBytes, err := prevReq.DoRaw(ctx)
		if err == nil && len(prevBytes) > 0 {
			b.WriteString("## Previous Container Logs (before crash)\n\n```\n")
			b.Write(prevBytes)
			b.WriteString("```\n\n")
		}
	}

	return b.String()
}

func int64Ptr(i int64) *int64 {
	return &i
}

// appContainerFromPod returns the app container name for log streaming.
// For single-container pods it returns empty (Kubernetes uses the only
// container). For multi-container pods it returns the first non-sidecar
// container name.
func appContainerFromPod(pod *corev1.Pod) string {
	if len(pod.Spec.Containers) <= 1 {
		return ""
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != "kipper-instance-proxy" {
			return c.Name
		}
	}
	return pod.Spec.Containers[0].Name
}
