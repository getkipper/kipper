package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/console-api/ai"
)

// AIDiagnoseService provides AI-powered diagnosis for services (StatefulSets).
type AIDiagnoseService struct {
	Client   kubernetes.Interface
	Settings *AISettings
}

// Diagnose gathers pod status, events, and logs for a service and streams an AI diagnosis.
// POST /api/v1/services/{name}/diagnose?namespace={ns}
func (a *AIDiagnoseService) Diagnose(w http.ResponseWriter, r *http.Request) {
	name, namespace, ok := requireService(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	cfg, err := a.Settings.GetRaw(ctx)
	if err != nil || cfg.Provider == "" {
		respondError(w, http.StatusBadRequest, "AI not configured — go to Settings to add a provider")
		return
	}

	diagnostic := gatherServiceDiagnostics(ctx, a.Client, name, namespace)

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
		{Role: "user", Content: "Diagnose this failing service:\n\n" + diagnostic},
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

func gatherServiceDiagnostics(ctx context.Context, client kubernetes.Interface, name, namespace string) string {
	var b strings.Builder

	ss, err := client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		b.WriteString("Service not found.\n")
		return b.String()
	}

	b.WriteString("## StatefulSet Status\n\n")
	desired := int32(1)
	if ss.Spec.Replicas != nil {
		desired = *ss.Spec.Replicas
	}
	_, _ = fmt.Fprintf(&b, "Name: %s\nNamespace: %s\nDesired: %d, Ready: %d, Updated: %d\n",
		ss.Name, ss.Namespace, desired, ss.Status.ReadyReplicas, ss.Status.UpdatedReplicas)

	if ss.Spec.Template.Spec.Containers[0].Resources.Requests != nil {
		_, _ = fmt.Fprintf(&b, "Resources: CPU=%s, Memory=%s\n",
			ss.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().String(),
			ss.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().String())
	}
	b.WriteString("\n")

	// Pod statuses
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", name),
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
		b.WriteString("## Pod Status\n\nNo pods found.\n\n")
	}

	// Events
	if pods != nil {
		for _, pod := range pods.Items {
			podEvents, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
				FieldSelector: fmt.Sprintf("involvedObject.name=%s", pod.Name),
			})
			if err == nil && len(podEvents.Items) > 0 {
				_, _ = fmt.Fprintf(&b, "## Events for %s\n\n", pod.Name)
				for _, e := range podEvents.Items {
					_, _ = fmt.Fprintf(&b, "[%s] %s\n", e.Reason, e.Message)
				}
				b.WriteString("\n")
			}
		}
	}

	// PVC status
	pvcs, err := client.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, pvc := range pvcs.Items {
			if strings.HasPrefix(pvc.Name, "data-"+name) {
				_, _ = fmt.Fprintf(&b, "## PVC: %s\n\nPhase: %s, Storage: %s\n\n",
					pvc.Name, pvc.Status.Phase, pvc.Spec.Resources.Requests.Storage().String())
			}
		}
	}

	// Recent logs
	if pods != nil && len(pods.Items) > 0 {
		pod := pods.Items[0]
		container := appContainerFromPod(&pod)
		logOpts := &corev1.PodLogOptions{Container: container, TailLines: int64Ptr(50)}
		req := client.CoreV1().Pods(namespace).GetLogs(pod.Name, logOpts)
		logBytes, err := req.DoRaw(ctx)
		if err == nil && len(logBytes) > 0 {
			b.WriteString("## Container Logs (last 50 lines)\n\n```\n")
			b.Write(logBytes)
			b.WriteString("```\n\n")
		}

		prevOpts := &corev1.PodLogOptions{Container: container, TailLines: int64Ptr(50), Previous: true}
		prevReq := client.CoreV1().Pods(namespace).GetLogs(pod.Name, prevOpts)
		prevBytes, err := prevReq.DoRaw(ctx)
		if err == nil && len(prevBytes) > 0 {
			b.WriteString("## Previous Container Logs (before crash)\n\n```\n")
			b.Write(prevBytes)
			b.WriteString("```\n\n")
		}
	}

	return b.String()
}
