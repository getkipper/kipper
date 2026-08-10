package migration

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/console-api/security"
)

// CutoverHandler applies the original custom domain routes to migrated apps
// on the target cluster. Called after the user verifies everything works on
// temporary kipper.run URLs.
// POST /api/v1/migration/{session}/cutover
func (h *Handler) CutoverHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "session")
	session, ok := h.Sessions.Get(sessionID)
	if !ok {
		respondError(w, http.StatusNotFound, "session not found")
		return
	}

	if status := session.CurrentStatus(); status != SessionVerifying {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("session is %s, not verifying", status))
		return
	}

	var req struct {
		Force    bool   `json:"force,omitempty"`
		TOTPCode string `json:"totp_code"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	// Cutover repoints production domains, so it carries the same step-up
	// requirement as start. Fails closed when no verifier is wired. The
	// verifier runs after every non-mutating check so a build-gate refusal
	// or a target hiccup never burns a code.
	if h.StepUp == nil {
		respondError(w, http.StatusServiceUnavailable, "2FA verification is unavailable — cutover cannot proceed")
		return
	}

	// A cutover while git rebuilds are unfinished points the production
	// domains at the "building" placeholder (or a failed build). Refuse
	// unless the operator explicitly forces it.
	if !req.Force {
		pending, err := h.unfinishedBuilds(session)
		if err != nil {
			respondError(w, http.StatusBadGateway, fmt.Sprintf("checking build state on target: %v", err))
			return
		}
		if len(pending) > 0 {
			respondJSON(w, http.StatusConflict, map[string]interface{}{
				"error":          fmt.Sprintf("git rebuilds are not finished on the target: %s; wait for the builds, or confirm cutover anyway", strings.Join(pending, ", ")),
				"builds_pending": pending,
			})
			return
		}
	}

	// The atomic code claim sits directly before the first route mutation.
	if err := h.StepUp(r.Context(), middleware.UserFromContext(r.Context()), req.TOTPCode, "migration cutover"); err != nil {
		respondError(w, http.StatusForbidden, err.Error())
		return
	}

	stepName := "Applying custom domains"
	session.AddStep(Step{
		Name:   stepName,
		Phase:  "cutover",
		Status: StepRunning,
	})

	// The custom domains must move onto the target's address. Resolve it
	// once from the target API endpoint — it rides the same ingress as
	// every app on that box — so each hostname's DNS state is judged
	// against the records it actually has to reach.
	expected := h.targetAddrs(session)

	// Apply saved routes to App CRs on the target via the resource endpoint
	var domains []domainStatus
	var failed []string
	total := 0
	for key, routeMap := range session.RoutesSnapshot() {
		namespace, appName := splitKey(key)
		host, _ := routeMap["host"].(string)
		if host == "" {
			continue
		}
		total++

		// Send the route as an AppRouteUpdate to the target
		err := h.sendToTargetDirect(session, fmt.Sprintf("/api/v1/migrate-target/%s/resource", session.ID), map[string]interface{}{
			"kind":      "AppRouteUpdate",
			"name":      appName,
			"namespace": namespace,
			"spec":      map[string]interface{}{"route": routeMap},
		})
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s (%s/%s): %v", host, namespace, appName, err))
			continue
		}

		domains = append(domains, domainStatus{
			Domain:      host,
			App:         appName,
			Namespace:   namespace,
			ExpectedIPs: expected,
		})
	}

	// Fail closed on any unapplied route: the session stays in verifying so
	// the cutover can be retried once the cause is fixed. A partial cutover
	// must never read as a completed migration.
	if len(failed) > 0 {
		session.UpdateStep(stepName, func(s *Step) {
			s.Status = StepFailed
			s.Error = fmt.Sprintf("%d of %d custom domains failed to apply: %s", len(failed), total, strings.Join(failed, "; "))
		})
		// The session stays in verifying but the recorded failure must
		// survive a restart, so the retry starts from a truthful state.
		h.Sessions.Save(session)
		respondError(w, http.StatusBadGateway, fmt.Sprintf("cutover incomplete, %d of %d routes failed: %s; fix the cause and run cutover again", len(failed), total, strings.Join(failed, "; ")))
		return
	}

	session.UpdateStep(stepName, func(s *Step) {
		s.Status = StepCompleted
		s.Detail = fmt.Sprintf("%d custom domains applied", len(domains))
		now := time.Now()
		s.CompletedAt = &now
	})

	// Report each hostname's DNS state against the target's records.
	var stale []string
	for i := range domains {
		domains[i].check(expected)
		if !domains[i].Resolved {
			stale = append(stale, domains[i].Domain)
		}
	}

	// Tell the target the migration landed, so it drops the backups of the
	// Secrets this run overwrote. Best-effort: the rollback copies are inert
	// once cutover has happened, and a failure here must not turn a completed
	// cutover into a reported failure.
	commitToken := &Token{Endpoint: session.TargetAPI, Secret: session.Secret}
	commitResp, err := h.callTarget(commitToken, "POST", fmt.Sprintf("/api/v1/migrate-target/%s/commit", session.ID), nil)
	switch {
	case err != nil:
		session.AddStep(Step{
			Name:   "Clearing rollback copies on target",
			Phase:  "cutover",
			Status: StepSkipped,
			Detail: fmt.Sprintf("could not reach the target to drop the pre-overwrite secret copies: %v", err),
		})
	default:
		// The copies hold a second plaintext of the credentials this migration
		// replaced, so any that survive are named here rather than left to look
		// like a clean finish.
		if remaining, ok := commitResp["rollback_copies_remaining"].([]interface{}); ok && len(remaining) > 0 {
			names := make([]string, 0, len(remaining))
			for _, r := range remaining {
				names = append(names, fmt.Sprintf("%v", r))
			}
			session.AddStep(Step{
				Name:   "Clearing rollback copies on target",
				Phase:  "cutover",
				Status: StepSkipped,
				Detail: fmt.Sprintf("these pre-overwrite secret copies are still on the target and hold the credentials it had before; delete them once you are satisfied with the migration: %s", strings.Join(names, ", ")),
			})
		}
	}

	// The completion step goes in before the persist so a restarted
	// console-api restores the same history the live process showed.
	session.AddStep(Step{
		Name:   "Migration complete",
		Phase:  "cutover",
		Status: StepCompleted,
	})
	session.Finish(SessionCompleted, "")
	h.Sessions.Save(session)

	// Cutover repoints production domains — the moment the migration takes
	// effect for end users — so it alerts like the start did.
	cutoverBy := ""
	if claims := middleware.UserFromContext(r.Context()); claims != nil {
		cutoverBy = claims.Email
	}
	view := session.View()
	h.emitSecurityEvent(r.Context(), security.Event{
		Kind:    "migration_cutover",
		User:    cutoverBy,
		Summary: fmt.Sprintf("migration cutover to %s applied by %s", view.TargetCluster, cutoverBy),
		Fields: []security.Field{
			{Key: "target_cluster", Value: view.TargetCluster},
			{Key: "domains", Value: strconv.Itoa(len(domains))},
			{Key: "forced", Value: strconv.FormatBool(req.Force)},
			{Key: "session", Value: sessionID},
		},
	})

	resp := map[string]interface{}{
		"status":  "completed",
		"domains": domains,
	}
	// The warning must survive a failed target lookup: with no expected
	// addresses every domain reads as stale, and swallowing the warning
	// there would present a completed cutover with silently dead domains.
	if len(stale) > 0 {
		targetHint := "the target server's IP address"
		if len(expected) > 0 {
			targetHint = strings.Join(expected, ", ")
		}
		resp["dns_warning"] = fmt.Sprintf(
			"These hostnames do not point at the target yet: %s. Update their A/AAAA records at your DNS provider to %s. Certificates are issued once DNS lands.",
			strings.Join(stale, ", "), targetHint)
	}
	respondJSON(w, http.StatusOK, resp)
}

// unfinishedBuilds lists the target's git apps whose rebuild has not
// succeeded yet, as "namespace/name (phase)" strings.
func (h *Handler) unfinishedBuilds(session *Session) ([]string, error) {
	token := &Token{Endpoint: session.TargetAPI, Secret: session.Secret}
	resp, err := h.callTarget(token, "GET", fmt.Sprintf("/api/v1/migrate-target/%s/apps", session.ID), nil)
	if err != nil {
		return nil, err
	}

	var pending []string
	apps, _ := resp["apps"].([]interface{})
	for _, a := range apps {
		m, ok := a.(map[string]interface{})
		if !ok {
			continue
		}
		if hasGit, _ := m["has_git"].(bool); !hasGit {
			continue
		}
		phase, _ := m["build_phase"].(string)
		if phase == "Succeeded" {
			continue
		}
		if phase == "" {
			phase = "not started"
		}
		name, _ := m["name"].(string)
		ns, _ := m["namespace"].(string)
		pending = append(pending, fmt.Sprintf("%s/%s (%s)", ns, name, phase))
	}
	return pending, nil
}

// targetAddrs resolves the target cluster's public addresses from the
// session's target API endpoint.
func (h *Handler) targetAddrs(session *Session) []string {
	u, err := url.Parse(session.TargetAPI)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	addrs, err := net.LookupHost(u.Hostname())
	if err != nil {
		return nil
	}
	return addrs
}

// check resolves the domain and records whether it points at the target.
// Resolving to some address is not enough — a hostname still parked on the
// old box resolves fine and used to read as "Resolved" in the console.
func (d *domainStatus) check(expected []string) {
	addrs, err := net.LookupHost(d.Domain)
	if err != nil || len(addrs) == 0 {
		return
	}
	d.ResolvedTo = addrs[0]
	d.Resolved = pointsAt(addrs, expected)
}

// pointsAt reports whether any resolved address matches an expected one.
func pointsAt(addrs, expected []string) bool {
	for _, a := range addrs {
		for _, e := range expected {
			if a == e {
				return true
			}
		}
	}
	return false
}

// DNSStatusHandler checks DNS propagation for migrated domains.
// GET /api/v1/migration/{session}/dns
func (h *Handler) DNSStatusHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "session")
	session, ok := h.Sessions.Get(sessionID)
	if !ok {
		respondError(w, http.StatusNotFound, "session not found")
		return
	}

	expected := h.targetAddrs(session)

	var domains []domainStatus
	for key, routeMap := range session.RoutesSnapshot() {
		namespace, appName := splitKey(key)
		host, _ := routeMap["host"].(string)
		if host == "" {
			continue
		}

		ds := domainStatus{
			Domain:      host,
			App:         appName,
			Namespace:   namespace,
			ExpectedIPs: expected,
		}
		ds.check(expected)

		domains = append(domains, ds)
	}

	respondJSON(w, http.StatusOK, domains)
}

// VerificationHandler returns the list of migrated apps with the temporary
// URLs they are reachable on, for the user to test before domain cutover.
// The app states come from the target cluster itself — its Ingresses carry
// the real temporary hosts, and its build phases show rebuild progress.
// GET /api/v1/migration/{session}/verify
func (h *Handler) VerificationHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "session")
	session, ok := h.Sessions.Get(sessionID)
	if !ok {
		respondError(w, http.StatusNotFound, "session not found")
		return
	}

	token := &Token{Endpoint: session.TargetAPI, Secret: session.Secret}
	resp, err := h.callTarget(token, "GET", fmt.Sprintf("/api/v1/migrate-target/%s/apps", session.ID), nil)
	if err != nil {
		respondError(w, http.StatusBadGateway, fmt.Sprintf("querying target cluster: %v", err))
		return
	}

	// Attach the custom domain each app returns to at cutover.
	routes := session.RoutesSnapshot()
	apps, _ := resp["apps"].([]interface{})
	for _, a := range apps {
		m, ok := a.(map[string]interface{})
		if !ok {
			continue
		}
		ns, _ := m["namespace"].(string)
		name, _ := m["name"].(string)
		if route, ok := routes[ns+"/"+name]; ok {
			if host, _ := route["host"].(string); host != "" {
				m["custom_domain"] = host
			}
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"session_id": sessionID,
		"status":     session.CurrentStatus(),
		"apps":       apps,
	})
}

type domainStatus struct {
	Domain    string `json:"domain"`
	App       string `json:"app"`
	Namespace string `json:"namespace"`
	// Resolved means the hostname points at the target cluster, not merely
	// that it resolves to something.
	Resolved    bool     `json:"resolved"`
	ResolvedTo  string   `json:"resolved_to,omitempty"`
	ExpectedIPs []string `json:"expected_ips,omitempty"`
}

// sendToTargetDirect sends a request to the target using the session's stored
// target API endpoint and migration secret (for use after the token has been
// consumed, when the source session is the only remaining holder of the secret).
func (h *Handler) sendToTargetDirect(session *Session, path string, body interface{}) error {
	token := &Token{Endpoint: session.TargetAPI, Secret: session.Secret}
	return h.sendToTarget(token, path, body)
}

func splitKey(key string) (namespace, name string) {
	idx := strings.Index(key, "/")
	if idx < 0 {
		return key, ""
	}
	return key[:idx], key[idx+1:]
}
