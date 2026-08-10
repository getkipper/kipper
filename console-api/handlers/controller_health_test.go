package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// resetControllerHealth returns the registry to a clean state so tests don't
// leak into one another through the package-level singleton.
func resetControllerHealth() {
	controllerHealth.mu.Lock()
	defer controllerHealth.mu.Unlock()
	controllerHealth.controllers = map[string]bool{}
	controllerHealth.managerStarted = false
	controllerHealth.cacheSynced = false
}

// healthResponse mirrors the endpoint's minimal public body: a single boolean,
// with no controller inventory or manager/cache internals exposed to anonymous
// callers.
type healthResponse struct {
	Healthy bool `json:"healthy"`
}

func getControllerHealth(t *testing.T) (int, healthResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/health/controllers", nil)
	w := httptest.NewRecorder()
	ControllerHealthHandler(w, req)

	var resp healthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	// The response must carry only the boolean — no internal topology.
	if body := w.Body.String(); strings.Contains(body, "managerStarted") ||
		strings.Contains(body, "controllers") || strings.Contains(body, "cacheSynced") {
		t.Fatalf("controller-health response leaks internal topology: %s", body)
	}
	return w.Code, resp
}

func TestControllerHealthAllUpReports200(t *testing.T) {
	resetControllerHealth()
	SetControllerRegistered("App", true)
	SetControllerRegistered("Service", true)
	SetControllerManagerStarted(true)
	SetControllerCacheSynced(true)

	code, resp := getControllerHealth(t)

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if !resp.Healthy {
		t.Fatalf("expected healthy=true, got false")
	}
}

func TestControllerHealthOneDownReports503(t *testing.T) {
	resetControllerHealth()
	SetControllerRegistered("App", true)
	SetControllerRegistered("Build", false)
	SetControllerManagerStarted(true)
	SetControllerCacheSynced(true)

	code, resp := getControllerHealth(t)

	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when a controller is down, got %d", code)
	}
	if resp.Healthy {
		t.Fatalf("expected healthy=false when a controller failed to register")
	}
}

func TestControllerHealthStartingReports503(t *testing.T) {
	// Manager not yet started and caches not synced: healthy must be false
	// even though every registered controller succeeded.
	resetControllerHealth()
	SetControllerRegistered("App", true)

	code, resp := getControllerHealth(t)

	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while still starting, got %d", code)
	}
	if resp.Healthy {
		t.Fatalf("expected healthy=false while starting, got true")
	}
}

func TestControllerHealthNoControllersReports503(t *testing.T) {
	// An empty registry (e.g. the manager failed to construct) is unhealthy.
	resetControllerHealth()
	SetControllerManagerStarted(true)
	SetControllerCacheSynced(true)

	code, resp := getControllerHealth(t)

	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 with no controllers, got %d", code)
	}
	if resp.Healthy {
		t.Fatalf("expected healthy=false with no controllers registered")
	}
}
