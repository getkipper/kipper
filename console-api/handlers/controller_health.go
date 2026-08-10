package handlers

import (
	"net/http"
	"sync"
)

// controllerHealthRegistry records the registration and runtime state of the
// CRD controller manager so it can be surfaced instead of living only in logs.
type controllerHealthRegistry struct {
	mu             sync.RWMutex
	controllers    map[string]bool
	managerStarted bool
	cacheSynced    bool
}

var controllerHealth = &controllerHealthRegistry{controllers: map[string]bool{}}

// SetControllerRegistered records whether the named CRD controller registered
// with the manager. Called during controller-manager startup.
func SetControllerRegistered(name string, registered bool) {
	controllerHealth.mu.Lock()
	defer controllerHealth.mu.Unlock()
	controllerHealth.controllers[name] = registered
}

// SetControllerManagerStarted records whether the controller manager is
// running.
func SetControllerManagerStarted(started bool) {
	controllerHealth.mu.Lock()
	defer controllerHealth.mu.Unlock()
	controllerHealth.managerStarted = started
}

// SetControllerCacheSynced records whether the manager's informer caches have
// synced.
func SetControllerCacheSynced(synced bool) {
	controllerHealth.mu.Lock()
	defer controllerHealth.mu.Unlock()
	controllerHealth.cacheSynced = synced
}

// ControllerHealthHandler reports whether the CRD controllers are healthy.
// GET /health/controllers
//
// This is a reporting endpoint for observability and alerting, not the pod
// readiness probe. console-api keeps serving the API even when a controller is
// down, so the console can surface the degradation rather than the whole UI
// going dark over one broken reconciler. It returns 503 when degraded so
// external monitors can alert on it.
//
// The endpoint is unauthenticated (external monitors need it), so the response
// is a single boolean. The controller inventory, manager, and cache-sync state
// are internal topology an anonymous caller has no business fingerprinting.
func ControllerHealthHandler(w http.ResponseWriter, r *http.Request) {
	controllerHealth.mu.RLock()
	allRegistered := len(controllerHealth.controllers) > 0
	for _, registered := range controllerHealth.controllers {
		if !registered {
			allRegistered = false
		}
	}
	healthy := controllerHealth.managerStarted && controllerHealth.cacheSynced && allRegistered
	controllerHealth.mu.RUnlock()

	status := http.StatusOK
	if !healthy {
		status = http.StatusServiceUnavailable
	}
	respondJSON(w, status, map[string]bool{"healthy": healthy})
}
