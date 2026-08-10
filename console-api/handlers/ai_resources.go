package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/console-api/ai"
)

const resourceOptPrompt = `You are the Kipper resource optimiser. Analyse the resource usage data below and provide specific recommendations to reduce waste or prevent issues.

Rules:
- Be concise — bullet points, not paragraphs
- Give specific values: "reduce memory limit from 512Mi to 192Mi"
- Flag over-provisioned resources (using <30%% of request)
- Flag under-provisioned resources (using >80%% of request, risk of OOM/throttling)
- If resources look well-tuned, say so
- Reference Kipper console actions, not kubectl commands`

// AIResources provides AI-powered resource optimisation recommendations.
type AIResources struct {
	Client   kubernetes.Interface
	Settings *AISettings
}

// Optimise gathers resource usage and streams AI recommendations.
// POST /api/v1/projects/{name}/apps/{app}/optimise
func (a *AIResources) Optimise(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	cfg, err := a.Settings.GetRaw(ctx)
	if err != nil || cfg.Provider == "" {
		respondError(w, http.StatusBadRequest, "AI not configured: go to Settings to add a provider")
		return
	}

	summary := a.gatherResourceData(ctx, project, app)

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
		{Role: "user", Content: "Analyse these resource allocations and suggest optimisations:\n\n" + summary},
	}

	stream, err := provider.Chat(ctx, resourceOptPrompt, messages)
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

func (a *AIResources) gatherResourceData(ctx context.Context, namespace, app string) string {
	var b strings.Builder

	// Get deployment spec (requests/limits)
	deploy, err := a.Client.AppsV1().Deployments(namespace).Get(ctx, app, metav1.GetOptions{})
	if err != nil {
		b.WriteString("Deployment not found.\n")
		return b.String()
	}

	b.WriteString("## Resource Configuration\n\n")
	for _, c := range deploy.Spec.Template.Spec.Containers {
		_, _ = fmt.Fprintf(&b, "Container: %s\n", c.Name)
		if c.Resources.Requests != nil {
			_, _ = fmt.Fprintf(&b, "  Requests: CPU=%s, Memory=%s\n",
				c.Resources.Requests.Cpu().String(),
				c.Resources.Requests.Memory().String())
		} else {
			b.WriteString("  Requests: not set\n")
		}
		if c.Resources.Limits != nil {
			_, _ = fmt.Fprintf(&b, "  Limits: CPU=%s, Memory=%s\n",
				c.Resources.Limits.Cpu().String(),
				c.Resources.Limits.Memory().String())
		} else {
			b.WriteString("  Limits: not set\n")
		}
	}

	// Get replicas
	var replicas int32
	if deploy.Spec.Replicas != nil {
		replicas = *deploy.Spec.Replicas
	}
	_, _ = fmt.Fprintf(&b, "\nReplicas: %d (ready: %d)\n", replicas, deploy.Status.ReadyReplicas)

	// Get pod metrics from metrics-server
	b.WriteString("\n## Current Usage (from metrics-server)\n\n")

	pods, err := a.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", app),
	})
	if err != nil || len(pods.Items) == 0 {
		b.WriteString("No running pods found.\n")
		return b.String()
	}

	// Use the metrics API via discovery REST client
	for _, pod := range pods.Items {
		metricsPath := fmt.Sprintf("/apis/metrics.k8s.io/v1beta1/namespaces/%s/pods/%s", namespace, pod.Name)
		result := a.Client.Discovery().RESTClient().Get().AbsPath(metricsPath).Do(ctx)
		raw, err := result.Raw()
		if err != nil {
			_, _ = fmt.Fprintf(&b, "Pod %s: metrics unavailable\n", pod.Name)
			continue
		}

		var podMetrics struct {
			Containers []struct {
				Name  string `json:"name"`
				Usage struct {
					CPU    string `json:"cpu"`
					Memory string `json:"memory"`
				} `json:"usage"`
			} `json:"containers"`
		}
		if err := json.Unmarshal(raw, &podMetrics); err != nil {
			_, _ = fmt.Fprintf(&b, "Pod %s: failed to parse metrics\n", pod.Name)
			continue
		}

		for _, c := range podMetrics.Containers {
			_, _ = fmt.Fprintf(&b, "Pod %s / %s: CPU=%s, Memory=%s\n", pod.Name, c.Name, c.Usage.CPU, c.Usage.Memory)
		}
	}

	// HPA status if present
	hpa, err := a.Client.AutoscalingV2().HorizontalPodAutoscalers(namespace).Get(ctx, app, metav1.GetOptions{})
	if err == nil {
		b.WriteString("\n## Autoscaling\n\n")
		_, _ = fmt.Fprintf(&b, "HPA: min=%d, max=%d, current=%d replicas\n", *hpa.Spec.MinReplicas, hpa.Spec.MaxReplicas, hpa.Status.CurrentReplicas)
	}

	return b.String()
}
