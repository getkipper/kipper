package migration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/getkipper/kipper/console-api/middleware"
)

func TestPointsAt(t *testing.T) {
	tests := []struct {
		name     string
		addrs    []string
		expected []string
		want     bool
	}{
		{"points at target", []string{"203.0.113.20"}, []string{"203.0.113.20"}, true},
		{"still on the old box", []string{"203.0.113.10"}, []string{"203.0.113.20"}, false},
		{"one of several records matches", []string{"203.0.113.10", "203.0.113.20"}, []string{"203.0.113.20"}, true},
		{"no expected records known", []string{"203.0.113.10"}, nil, false},
		{"nothing resolved", nil, []string{"203.0.113.20"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pointsAt(tt.addrs, tt.expected); got != tt.want {
				t.Fatalf("pointsAt(%v, %v) = %v, want %v", tt.addrs, tt.expected, got, tt.want)
			}
		})
	}
}

func cutoverSession(targetAPI string) *Session {
	return &Session{
		ID:        "s1",
		Status:    SessionVerifying,
		TargetAPI: targetAPI,
		Secret:    "secret",
		SavedRoutes: map[string]map[string]interface{}{
			"shop-prod/web": {"host": "shop.example.com"},
			"shop-prod/api": {"host": "api.example.com"},
		},
	}
}

// acceptAnyStepUp stands in for the twofa verifier on tests that exercise
// the cutover mechanics rather than the step-up gate.
func acceptAnyStepUp(ctx context.Context, claims *middleware.Claims, code, operation string) error {
	return nil
}

func runCutover(t *testing.T, session *Session) *httptest.ResponseRecorder {
	return runCutoverWithBody(t, session, "")
}

func runCutoverWithBody(t *testing.T, session *Session, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{Sessions: NewSessionStore(), StepUp: acceptAnyStepUp}
	h.Sessions.Put(session)
	return serveCutover(t, h, body)
}

func serveCutover(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Post("/{session}/cutover", h.CutoverHandler)

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, "/s1/cutover", reader)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// Without a wired verifier the cutover refuses outright — fail closed, never
// fail open.
func TestCutoverHandler_FailsClosedWithoutStepUp(t *testing.T) {
	h := &Handler{Sessions: NewSessionStore()}
	h.Sessions.Put(cutoverSession("http://unused.invalid"))
	rec := serveCutover(t, h, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// A refused code stops the cutover after the build gate, before any route
// moves.
func TestCutoverHandler_RefusesBadCode(t *testing.T) {
	routesTouched := false
	target := targetWithApps(`{"apps":[]}`, func(w http.ResponseWriter, r *http.Request) {
		routesTouched = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"created"}`))
	})
	defer target.Close()

	h := &Handler{Sessions: NewSessionStore(), StepUp: func(ctx context.Context, claims *middleware.Claims, code, operation string) error {
		return fmt.Errorf("invalid 2FA code")
	}}
	session := cutoverSession(target.URL)
	h.Sessions.Put(session)
	rec := serveCutover(t, h, `{"totp_code":"000000"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if session.CurrentStatus() != SessionVerifying {
		t.Fatalf("session status = %s, want untouched verifying", session.CurrentStatus())
	}
	if routesTouched {
		t.Fatal("a refused code must never reach a route update")
	}
}

// targetWithApps wraps a route handler with a target that reports the given
// apps state on the build-gate endpoint, which the cutover checks first.
func targetWithApps(appsJSON string, routes http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/apps") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(appsJSON))
			return
		}
		routes(w, r)
	}))
}

// A route the target refuses must fail the cutover and keep the session in
// verifying, so it can be retried. Partial route application used to be
// reported as a completed migration.
func TestCutoverHandler_FailsClosedOnRouteError(t *testing.T) {
	target := targetWithApps(`{"apps":[]}`, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no such app"}`, http.StatusInternalServerError)
	})
	defer target.Close()

	session := cutoverSession(target.URL)
	rec := runCutover(t, session)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if session.Status != SessionVerifying {
		t.Fatalf("session status = %s, want it kept at verifying for retry", session.Status)
	}
	if !strings.Contains(rec.Body.String(), "run cutover again") {
		t.Fatalf("response should tell the user the cutover is retryable, got: %s", rec.Body.String())
	}
}

// When every route applies, the cutover completes the session.
func TestCutoverHandler_CompletesWhenAllRoutesApply(t *testing.T) {
	target := targetWithApps(`{"apps":[]}`, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"created"}`))
	})
	defer target.Close()

	session := cutoverSession(target.URL)
	rec := runCutover(t, session)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if session.CurrentStatus() != SessionCompleted {
		t.Fatalf("session status = %s, want completed", session.CurrentStatus())
	}
}

// An unfinished git rebuild blocks the cutover: the production domain would
// otherwise serve the "building" placeholder. Force overrides the gate.
func TestCutoverHandler_BlocksOnUnfinishedBuilds(t *testing.T) {
	target := targetWithApps(
		`{"apps":[{"name":"web","namespace":"shop-prod","has_git":true,"build_phase":"Building"},{"name":"docs","namespace":"shop-prod","has_git":false}]}`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"created"}`))
		})
	defer target.Close()

	session := cutoverSession(target.URL)
	rec := runCutover(t, session)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "shop-prod/web (Building)") {
		t.Fatalf("response should name the unfinished build, got: %s", rec.Body.String())
	}
	if session.CurrentStatus() != SessionVerifying {
		t.Fatalf("session status = %s, want it kept at verifying", session.CurrentStatus())
	}

	// The image-only app must not block, and force must bypass the gate.
	rec = runCutoverWithBody(t, session, `{"force":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("forced cutover status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if session.CurrentStatus() != SessionCompleted {
		t.Fatalf("session status = %s, want completed after forced cutover", session.CurrentStatus())
	}
}

// Cutover is only valid from the verifying state.
func TestCutoverHandler_RejectsWrongState(t *testing.T) {
	session := cutoverSession("http://unused.invalid")
	session.Status = SessionFailed
	rec := runCutover(t, session)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
