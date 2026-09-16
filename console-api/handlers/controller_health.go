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

// ControllerHealthHandler reports manager startup, cache sync, and controller
// registration health at GET /health/controllers, returning 503 when degraded.
// It is separate from pod readiness so the API can still report controller issues.
// The unauthenticated response exposes only a boolean.
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
