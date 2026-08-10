package handlers

import "net/http"

// BuildVersion is the console-api build version, set from main at startup. It
// is reported on /health so the kip CLI can detect a version skew between
// itself and the cluster instead of misreading a rejected token as an expired
// session.
var BuildVersion = "dev"

// Health responds to liveness/readiness probes and reports the build version.
// GET /health
func Health(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": BuildVersion,
	})
}
