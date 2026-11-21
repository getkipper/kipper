package migration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
)

func cancelSession(h *Handler, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/migration/"+id+"/cancel", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("session", id)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.CancelHandler(rec, req)
	return rec
}

// TestCancelLeavesAFinishedMigrationAsItEnded covers late cancel requests,
// including clicks that race with cutover. Finished sessions keep their status.
func TestCancelLeavesAFinishedMigrationAsItEnded(t *testing.T) {
	for _, status := range []SessionStatus{SessionCompleted, SessionFailed, SessionCancelled} {
		h := &Handler{Sessions: NewSessionStore()}
		h.Sessions.Put(&Session{ID: "done", TargetAPI: "http://127.0.0.1:1", Status: status, Error: "kept"})

		rec := cancelSession(h, "done")

		assert.Equal(t, http.StatusConflict, rec.Code, "cancel of a %s migration", status)
		s, _ := h.Sessions.Get("done")
		assert.Equal(t, status, s.CurrentStatus())
	}
}

func TestCancelStopsARunningOrVerifyingMigration(t *testing.T) {
	for _, status := range []SessionStatus{SessionRunning, SessionVerifying} {
		h := &Handler{Sessions: NewSessionStore()}
		h.Sessions.Put(&Session{ID: "live", TargetAPI: "http://127.0.0.1:1", Status: status})

		rec := cancelSession(h, "live")

		assert.Equal(t, http.StatusOK, rec.Code, "cancel of a %s migration", status)
		s, _ := h.Sessions.Get("live")
		assert.Equal(t, SessionCancelled, s.CurrentStatus())
	}
}

func postCutover(h *Handler, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/migration/"+id+"/cutover", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("session", id)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.CutoverHandler(rec, req)
	return rec
}

// TestCancelWaitsForARunningCutover checks that cancellation is blocked while
// production domains move, since some may already have moved when cancel arrives.
func TestCancelWaitsForARunningCutover(t *testing.T) {
	h := &Handler{Sessions: NewSessionStore()}
	s := &Session{ID: "live", TargetAPI: "http://127.0.0.1:1", Status: SessionVerifying}
	h.Sessions.Put(s)
	if _, claimed := s.BeginCutover(); !claimed {
		t.Fatal("a verifying session must be able to begin its cutover")
	}

	rec := cancelSession(h, "live")

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "cutover")
	assert.Contains(t, rec.Body.String(), `"cutover_running":true`, "the console tells this refusal apart from an ended migration")
	assert.Equal(t, SessionVerifying, s.CurrentStatus())

	s.EndCutover()
	assert.Equal(t, http.StatusOK, cancelSession(h, "live").Code, "cancel works again once the cutover returned")
}

func TestSecondCutoverIsRefusedWhileOneRuns(t *testing.T) {
	h := &Handler{Sessions: NewSessionStore()}
	s := &Session{ID: "live", TargetAPI: "http://127.0.0.1:1", Status: SessionVerifying}
	h.Sessions.Put(s)
	_, _ = s.BeginCutover()

	rec := postCutover(h, "live")

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "cutover")
}

// TestCancelDuringVerificationAbortsTheTarget checks cleanup after verification
// begins. The run loop exits at that point, so the handler requests cleanup.
func TestCancelDuringVerificationAbortsTheTarget(t *testing.T) {
	aborted := make(chan string, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aborted <- r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	h := &Handler{Sessions: NewSessionStore()}
	h.Sessions.Put(&Session{ID: "live", TargetAPI: target.URL, Secret: "s", Status: SessionVerifying})

	assert.Equal(t, http.StatusOK, cancelSession(h, "live").Code)

	select {
	case got := <-aborted:
		assert.Equal(t, "POST /api/v1/migrate-target/live/abort", got)
	case <-time.After(5 * time.Second):
		t.Fatal("the target was never asked to abort")
	}
}

// TestRefusedCancelReportsTheStatusItSaw checks the status captured under the
// lock. The handler uses it to identify a cutover refusal even if the cutover
// releases its claim before the response.
func TestRefusedCancelReportsTheStatusItSaw(t *testing.T) {
	s := &Session{ID: "live", Status: SessionVerifying}
	_, _ = s.BeginCutover()

	from, cancelled := s.MarkCancelled()
	s.EndCutover()

	assert.False(t, cancelled)
	assert.Equal(t, SessionVerifying, from, "a refusal while verifying means a cutover held the session")
}
