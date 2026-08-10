package handlers

import (
	"context"
	"net/http"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/ai"
)

// AIBundleStatus serves the AI bundle drift report so the Platform
// page (and any future alerting) can surface a missing LibreChat or
// AnythingLLM Deployment without an operator having to `kubectl get`
// the namespace by hand.
type AIBundleStatus struct {
	Client kubernetes.Interface
}

// Get returns the drift report for both AI bundles.
// GET /api/v1/settings/ai/bundle-status
func (h *AIBundleStatus) Get(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	report, err := ai.CheckBundleDrift(ctx, h.Client)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to check AI bundle drift")
		return
	}
	respondJSON(w, http.StatusOK, report)
}
