package migration

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// A session in verifying carries the saved routes and target secret the
// cutover still needs, so it must come back intact after a restart.
func TestSessionStore_VerifyingSessionSurvivesRestart(t *testing.T) {
	client := fake.NewSimpleClientset()

	store := NewPersistentSessionStore(client, "kipper-system")
	store.Put(&Session{
		ID:        "abc123",
		Status:    SessionVerifying,
		TargetAPI: "https://console-api.new.example.com",
		Projects:  []string{"shop"},
		StartedAt: time.Now(),
		Secret:    "target-secret",
		Steps: []Step{
			{Name: "Transferring secrets (shop-prod)", Status: StepCompleted},
			{Name: "Data transfer for minio (files)", Status: StepSkipped, ManualSteps: []string{"mc mirror ..."}},
		},
		SavedRoutes: map[string]map[string]interface{}{
			"shop-prod/web": {"host": "shop.example.com"},
		},
	})

	restarted := NewPersistentSessionStore(client, "kipper-system")
	session, ok := restarted.Get("abc123")
	if !ok {
		t.Fatal("session not restored after restart")
	}
	if session.Status != SessionVerifying {
		t.Fatalf("status = %s, want verifying", session.Status)
	}
	if session.Secret != "target-secret" {
		t.Fatal("target secret must survive the restart, cutover needs it")
	}
	if session.SavedRoutes["shop-prod/web"]["host"] != "shop.example.com" {
		t.Fatal("saved routes must survive the restart")
	}
	if len(session.Steps) != 2 || len(session.Steps[1].ManualSteps) == 0 {
		t.Fatal("steps, including manual instructions, must survive the restart")
	}
}

// A source run that was mid-transfer when the process died has lost its
// transfer loop. It must come back as failed with a reason, never as a
// zombie run.
func TestSessionStore_RunningSessionFailsOnRestart(t *testing.T) {
	client := fake.NewSimpleClientset()

	store := NewPersistentSessionStore(client, "kipper-system")
	store.Put(&Session{
		ID:        "def456",
		Status:    SessionRunning,
		TargetAPI: "https://target.example.com",
		StartedAt: time.Now(),
	})

	restarted := NewPersistentSessionStore(client, "kipper-system")
	session, ok := restarted.Get("def456")
	if !ok {
		t.Fatal("session not restored after restart")
	}
	if session.Status != SessionFailed {
		t.Fatalf("status = %s, want failed", session.Status)
	}
	if !strings.Contains(session.Error, "restart") {
		t.Fatalf("error should explain the restart, got: %q", session.Error)
	}
}

// A target-side session (no TargetAPI) runs no transfer loop: it only
// authenticates the source's writes. It must survive a restart in running
// state, or the source's in-flight transfer gets refused mid-migration.
func TestSessionStore_TargetSessionKeepsRunningOnRestart(t *testing.T) {
	client := fake.NewSimpleClientset()

	store := NewPersistentSessionStore(client, "kipper-system")
	store.Put(&Session{
		ID:        "tgt789",
		Status:    SessionRunning,
		Secret:    "target-secret",
		StartedAt: time.Now(),
	})

	restarted := NewPersistentSessionStore(client, "kipper-system")
	session, ok := restarted.Get("tgt789")
	if !ok {
		t.Fatal("session not restored after restart")
	}
	if session.Status != SessionRunning {
		t.Fatalf("status = %s, want running", session.Status)
	}
	if session.Secret != "target-secret" {
		t.Fatal("target secret must survive the restart, receives authenticate against it")
	}
}

// Finished sessions are not kept forever: past retention their Secrets are
// removed instead of restored.
func TestSessionStore_OldSessionsAreDropped(t *testing.T) {
	client := fake.NewSimpleClientset()

	store := NewPersistentSessionStore(client, "kipper-system")
	store.Put(&Session{
		ID:        "old789",
		Status:    SessionCompleted,
		StartedAt: time.Now().Add(-8 * 24 * time.Hour),
	})

	restarted := NewPersistentSessionStore(client, "kipper-system")
	if _, ok := restarted.Get("old789"); ok {
		t.Fatal("session past retention must not be restored")
	}
	if _, err := client.CoreV1().Secrets("kipper-system").Get(context.Background(), "migration-session-old789", metav1.GetOptions{}); err == nil {
		t.Fatal("session Secret past retention must be deleted")
	}
}

// Deleting a session removes its Secret so an evicted (e.g. expired) session
// cannot be resurrected by the next restart.
func TestSessionStore_DeleteRemovesSecret(t *testing.T) {
	client := fake.NewSimpleClientset()

	store := NewPersistentSessionStore(client, "kipper-system")
	store.Put(&Session{ID: "gone01", Status: SessionVerifying, StartedAt: time.Now()})
	store.Delete("gone01")

	if _, err := client.CoreV1().Secrets("kipper-system").Get(context.Background(), "migration-session-gone01", metav1.GetOptions{}); err == nil {
		t.Fatal("deleted session's Secret must be removed")
	}
	restarted := NewPersistentSessionStore(client, "kipper-system")
	if _, ok := restarted.Get("gone01"); ok {
		t.Fatal("deleted session must not be restored")
	}
}
