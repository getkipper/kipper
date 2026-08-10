package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

const (
	recommendationType = "ResourceRecommendation"
	dismissAnnotation  = "kipper.run/recommendation-dismissed-at"
)

// Recommendations provides handlers for resource recommendation management.
type Recommendations struct {
	CRClient crclient.Client
}

type recommendationResponse struct {
	Active             bool   `json:"active"`
	Message            string `json:"message,omitempty"`
	RecommendedProfile string `json:"recommended_profile,omitempty"`
	Since              string `json:"since,omitempty"`
}

// Get returns the current resource recommendation for an app.
// GET /projects/{name}/apps/{app}/recommendation
func (h *Recommendations) Get(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	var app kipperv1.App
	if err := h.CRClient.Get(r.Context(), crclient.ObjectKey{Namespace: project, Name: appName}, &app); err != nil {
		respondJSON(w, http.StatusOK, recommendationResponse{Active: false})
		return
	}

	for _, c := range app.Status.Conditions {
		if c.Type == recommendationType && c.Status == metav1.ConditionTrue {
			profile := extractProfile(c.Reason)
			respondJSON(w, http.StatusOK, recommendationResponse{
				Active:             true,
				Message:            c.Message,
				RecommendedProfile: profile,
				Since:              c.LastTransitionTime.Format(time.RFC3339),
			})
			return
		}
	}

	respondJSON(w, http.StatusOK, recommendationResponse{Active: false})
}

// Dismiss marks the recommendation as dismissed so it won't reappear immediately.
// POST /projects/{name}/apps/{app}/recommendation/dismiss
func (h *Recommendations) Dismiss(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	var app kipperv1.App
	if err := h.CRClient.Get(r.Context(), crclient.ObjectKey{Namespace: project, Name: appName}, &app); err != nil {
		respondError(w, http.StatusNotFound, "app not found")
		return
	}

	if app.Annotations == nil {
		app.Annotations = make(map[string]string)
	}
	app.Annotations[dismissAnnotation] = time.Now().UTC().Format(time.RFC3339)
	if err := h.CRClient.Update(r.Context(), &app); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to dismiss recommendation")
		return
	}

	// Clear the condition from status
	var filtered []metav1.Condition
	for _, c := range app.Status.Conditions {
		if c.Type != recommendationType {
			filtered = append(filtered, c)
		}
	}
	app.Status.Conditions = filtered
	_ = h.CRClient.Status().Update(r.Context(), &app)

	respondJSON(w, http.StatusOK, map[string]string{"status": "dismissed"})
}

// Apply applies the recommended profile to the app.
// POST /projects/{name}/apps/{app}/recommendation/apply
func (h *Recommendations) Apply(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	var app kipperv1.App
	if err := h.CRClient.Get(r.Context(), crclient.ObjectKey{Namespace: project, Name: appName}, &app); err != nil {
		respondError(w, http.StatusNotFound, "app not found")
		return
	}

	// Find the recommendation condition
	var profile string
	for _, c := range app.Status.Conditions {
		if c.Type == recommendationType && c.Status == metav1.ConditionTrue {
			profile = extractProfile(c.Reason)
			break
		}
	}

	if profile == "" {
		respondError(w, http.StatusBadRequest, "no active recommendation")
		return
	}

	// Apply the profile and clear custom overrides
	app.Spec.Resources.Profile = profile
	app.Spec.Resources.CPURequest = ""
	app.Spec.Resources.CPULimit = ""
	app.Spec.Resources.MemoryRequest = ""
	app.Spec.Resources.MemoryLimit = ""
	if err := h.CRClient.Update(r.Context(), &app); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to apply recommendation")
		return
	}

	// Clear the condition
	var filtered []metav1.Condition
	for _, c := range app.Status.Conditions {
		if c.Type != recommendationType {
			filtered = append(filtered, c)
		}
	}
	app.Status.Conditions = filtered
	_ = h.CRClient.Status().Update(r.Context(), &app)

	respondJSON(w, http.StatusOK, map[string]string{"status": "applied", "profile": profile})
}

// extractProfile derives the profile name from a condition Reason like "RecommendJvm".
func extractProfile(reason string) string {
	prefix := "Recommend"
	if strings.HasPrefix(reason, prefix) {
		return strings.ToLower(reason[len(prefix):])
	}
	return ""
}
