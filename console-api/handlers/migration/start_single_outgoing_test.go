package migration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func outgoingSession(id string, status SessionStatus) *Session {
	return &Session{
		ID:            id,
		TargetCluster: "earlier.example.org",
		TargetAPI:     "https://api.earlier.example.org",
		Status:        status,
		StartedAt:     time.Now().Add(-time.Hour),
	}
}

// The console follows only one outgoing migration, so a second one started
// alongside it would leave the first running unseen.
func TestStartRefusedWhileAnotherOutgoingMigrationIsInProgress(t *testing.T) {
	for _, status := range []SessionStatus{SessionRunning, SessionVerifying} {
		t.Run(string(status), func(t *testing.T) {
			h := &Handler{Sessions: NewSessionStore(), StepUp: acceptAnyStepUp}
			h.Sessions.Put(outgoingSession("earlier", status))
			_, tokenStr := testToken(t, "https://api.target.example.com")

			rec := postStart(t, h, map[string]interface{}{
				"token":     tokenStr,
				"projects":  []string{"shop"},
				"totp_code": "123456",
			})

			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
			}
			msg := rec.Body.String()
			if !strings.Contains(msg, "earlier.example.org") {
				t.Fatalf("refusal must name the running migration's target, got: %s", msg)
			}
			if !strings.Contains(msg, "cancel") {
				t.Fatalf("refusal must say how to clear the way, got: %s", msg)
			}
		})
	}
}

// Reaching the plan check proves that finished outgoing sessions and incoming
// sessions allow a new start. The missing receipt then causes the refusal.
func TestStartNotBlockedByFinishedOrIncomingMigrations(t *testing.T) {
	cases := map[string]*Session{
		"completed": outgoingSession("done", SessionCompleted),
		"failed":    outgoingSession("broken", SessionFailed),
		"cancelled": outgoingSession("stopped", SessionCancelled),
		"incoming":  {ID: "incoming", Status: SessionRunning, StartedAt: time.Now()},
	}
	for name, session := range cases {
		t.Run(name, func(t *testing.T) {
			h := &Handler{Sessions: NewSessionStore(), StepUp: acceptAnyStepUp}
			h.Sessions.Put(session)
			_, tokenStr := testToken(t, "https://api.target.example.com")

			rec := postStart(t, h, map[string]interface{}{
				"token":     tokenStr,
				"projects":  []string{"shop"},
				"totp_code": "123456",
			})

			if rec.Code != http.StatusForbidden || !strings.Contains(strings.ToLower(rec.Body.String()), "review the plan") {
				t.Fatalf("want the plan refusal, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// Two starts sent at once must not both pass the check before either has
// created its session: exactly one reaches the target.
func TestConcurrentStartsCreateOnlyOneMigration(t *testing.T) {
	acceptReached := make(chan struct{})
	releaseAccept := make(chan struct{})
	hold := make(chan struct{})
	var accepts atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/accept"):
			if accepts.Add(1) == 1 {
				close(acceptReached)
			}
			<-releaseAccept
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"session_id": "t1", "target_base_domain": "target.example.com", "target_version": "dev",
			})
		case strings.Contains(r.URL.Path, "/t1/"):
			// Keeps the started migration running for the rest of the test.
			<-hold
			w.WriteHeader(http.StatusServiceUnavailable)
		case strings.HasSuffix(r.URL.Path, "/projects"):
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"allocatable_cpu_millis": 8000.0, "allocatable_memory_bytes": 1 << 34,
				"allocatable_storage_bytes": 1 << 40, "requested_cpu_millis": 0.0,
				"requested_memory_bytes": 0.0, "requested_storage_bytes": 0.0,
				"target_version": "dev",
			})
		}
	}))
	h := &Handler{
		Client:   fakeClientWithProject(t),
		CRClient: crFakeWithWorkloads(t),
		Sessions: NewSessionStore(),
		StepUp:   acceptAnyStepUp,
	}
	t.Cleanup(func() {
		if s, ok := h.Sessions.Get("t1"); ok {
			s.Finish(SessionCancelled, "")
			s.Cancel()
		}
		close(hold)
		target.Close()
	})
	token, tokenStr := testToken(t, target.URL)
	claims := planClaims("admin@example.com")
	start := func(receipt string) chan *httptest.ResponseRecorder {
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			done <- postStart(t, h, map[string]interface{}{
				"token":        tokenStr,
				"projects":     []string{"shop"},
				"plan_receipt": receipt,
				"totp_code":    "123456",
			})
		}()
		return done
	}

	first := start(issueMatchingReceipt(t, h, claims, token, []string{"shop"}))
	second := issueMatchingReceipt(t, h, claims, token, []string{"shop"})
	<-acceptReached
	secondDone := start(second)
	// Gives the second start time to get past any check that is not held
	// across the first start's accept call.
	time.Sleep(200 * time.Millisecond)
	close(releaseAccept)

	if rec := <-first; rec.Code != http.StatusOK {
		t.Fatalf("first start: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec := <-secondDone; rec.Code != http.StatusConflict {
		t.Fatalf("second start: status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if n := accepts.Load(); n != 1 {
		t.Fatalf("target accepted %d migrations, want 1", n)
	}
}

// TestStartRefusedAtOnceWhileAnotherStartIsUnderway verifies that a second
// start is rejected immediately while the first holds the lock during target checks.
func TestStartRefusedAtOnceWhileAnotherStartIsUnderway(t *testing.T) {
	h := &Handler{Sessions: NewSessionStore(), StepUp: acceptAnyStepUp}
	h.startMu.Lock()
	defer h.startMu.Unlock()
	_, tokenStr := testToken(t, "https://api.target.example.com")

	done := make(chan int, 1)
	go func() {
		rec := postStart(t, h, map[string]interface{}{
			"token":     tokenStr,
			"projects":  []string{"shop"},
			"totp_code": "123456",
		})
		done <- rec.Code
	}()
	select {
	case code := <-done:
		if code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second start waited for the first one's lock")
	}
}
