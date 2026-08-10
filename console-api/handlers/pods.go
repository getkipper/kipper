package handlers

import (
	"context"
	"fmt"
	"net/http"
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
