package migration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func activeSession(t *testing.T, h *Handler) *SessionView {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ActiveSessionHandler(rec, httptest.NewRequest(http.MethodGet, "/api/v1/migration/active", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Session *SessionView `json:"session"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Session
}

// A reload of the migration page loses the session it was following, so the
// page asks for the outgoing migration still in progress and picks it up.
func TestActiveSession_FindsTheOutgoingMigrationStillInProgress(t *testing.T) {
	now := time.Now()
	h := &Handler{Sessions: NewSessionStore()}
	h.Sessions.Put(&Session{ID: "old", TargetAPI: "https://target.example.com", Status: SessionVerifying, StartedAt: now.Add(-2 * time.Hour)})
	h.Sessions.Put(&Session{ID: "current", TargetAPI: "https://target.example.com", Status: SessionRunning, StartedAt: now.Add(-time.Minute)})
	h.Sessions.Put(&Session{ID: "done", TargetAPI: "https://target.example.com", Status: SessionCompleted, StartedAt: now})
	h.Sessions.Put(&Session{ID: "incoming", Status: SessionRunning, StartedAt: now})

	got := activeSession(t, h)

	require.NotNil(t, got)
	assert.Equal(t, "current", got.ID, "the newest outgoing session that is running or verifying")
}

func TestActiveSession_IsEmptyWithNoMigrationInProgress(t *testing.T) {
	h := &Handler{Sessions: NewSessionStore()}
	h.Sessions.Put(&Session{ID: "done", TargetAPI: "https://target.example.com", Status: SessionCompleted, StartedAt: time.Now()})
	h.Sessions.Put(&Session{ID: "cancelled", TargetAPI: "https://target.example.com", Status: SessionCancelled, StartedAt: time.Now()})

	assert.Nil(t, activeSession(t, h))
}
