package migration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/kubernetes/fake"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func postTarget(handler http.HandlerFunc, id, action string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/migrate-target/"+id+"/"+action, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("session", id)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func targetEndHandler(status SessionStatus) (*Handler, *Session) {
	h := &Handler{
		Sessions: NewSessionStore(),
		Client:   fake.NewSimpleClientset(),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).Build(),
	}
	s := &Session{ID: "t1", Projects: []string{"shop"}, Secret: "test-session-secret", Status: status}
	h.Sessions.Put(s)
	return h, s
}

// TestTargetCommitAndAbortDoNotUndoEachOther checks that commit and abort are
// mutually exclusive, since commit deletes the rollback copies that abort uses
// to restore Secrets. The same operation can run again to retry failed cleanup.
func TestTargetCommitAndAbortDoNotUndoEachOther(t *testing.T) {
	tests := []struct {
		name   string
		status SessionStatus
		action string
		want   int
	}{
		{"abort after commit", SessionCompleted, "abort", http.StatusConflict},
		{"commit after abort", SessionCancelled, "commit", http.StatusConflict},
		{"abort repeated", SessionCancelled, "abort", http.StatusOK},
		{"commit repeated", SessionCompleted, "commit", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, s := targetEndHandler(tt.status)
			handler := h.AbortHandler
			if tt.action == "commit" {
				handler = h.CommitHandler
			}

			rec := postTarget(handler, "t1", tt.action)

			assert.Equal(t, tt.want, rec.Code, rec.Body.String())
			assert.Equal(t, tt.status, s.CurrentStatus())
			assert.True(t, s.BeginEnding(tt.status), "the claim must be free once the request returned")
		})
	}
}

func TestTargetAbortWaitsForACommitInProgress(t *testing.T) {
	h, s := targetEndHandler(SessionRunning)
	if !s.BeginEnding(SessionCompleted) {
		t.Fatal("a running target session must be claimable")
	}

	assert.Equal(t, http.StatusConflict, postTarget(h.AbortHandler, "t1", "abort").Code)
	assert.Equal(t, http.StatusConflict, postTarget(h.CommitHandler, "t1", "commit").Code)
	assert.Equal(t, SessionRunning, s.CurrentStatus())
}
