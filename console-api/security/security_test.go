package security

import (
	"context"
	"sync"
	"testing"
	"time"
)

// waitFor polls until the condition holds or the deadline passes. The
// notifier delivers on its own goroutine, so tests observe asynchronously.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

func TestEmitDeliversToConsoleHooks(t *testing.T) {
	var mu sync.Mutex
	var alerts []string
	var emails []string

	n := &Notifier{Console: ConsoleHooks{
		Alert: func(ctx context.Context, kind, reason string) {
			mu.Lock()
			alerts = append(alerts, kind+": "+reason)
			mu.Unlock()
		},
		Email: func(ctx context.Context, to, subject, htmlBody string) error {
			mu.Lock()
			emails = append(emails, to)
			mu.Unlock()
			return nil
		},
		Admins: func() []string { return []string{"one@example.com", "two@example.com"} },
	}}

	n.Emit(context.Background(), Event{
		Kind:    "migration_started",
		User:    "admin@example.com",
		Summary: "cluster migration started",
		Fields:  []Field{{Key: "target_endpoint", Value: "https://api.target.example.com"}},
	})

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(alerts) == 1 && len(emails) == 2
	})
}

func TestEmitSurvivesCancelledRequestContext(t *testing.T) {
	var mu sync.Mutex
	delivered := false

	n := &Notifier{Console: ConsoleHooks{
		Alert: func(ctx context.Context, kind, reason string) {
			mu.Lock()
			delivered = true
			mu.Unlock()
		},
	}}

	// The request context is cancelled immediately, the way an admin
	// aborting the HTTP call would; delivery must still happen.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n.Emit(ctx, Event{Kind: "twofa_enrolled", Summary: "factor enrolled"})

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return delivered
	})
}

func TestOutOfBandConfigured(t *testing.T) {
	// The zero notifier with no env channel reports nothing out-of-band.
	n := &Notifier{}
	if n.OutOfBandConfigured(context.Background()) {
		t.Fatal("no channel must report false")
	}

	// A console channel counts.
	n.Console.SlackConfigured = func(ctx context.Context) bool { return true }
	if !n.OutOfBandConfigured(context.Background()) {
		t.Fatal("a configured Slack webhook must count as out-of-band")
	}

	// The env-pinned webhook counts and is reported separately.
	n.Console.SlackConfigured = nil
	t.Setenv("KIPPER_SECURITY_WEBHOOK", "https://hooks.example.com/T000/B000")
	if !n.OutOfBandConfigured(context.Background()) || !n.EnvPinnedConfigured() {
		t.Fatal("the env-pinned webhook must count as out-of-band and env-pinned")
	}
}

func TestEnvPinnedSMTPNeedsRecipients(t *testing.T) {
	t.Setenv("KIPPER_SECURITY_SMTP_HOST", "mail.example.com")
	n := &Notifier{}
	if n.EnvPinnedConfigured() {
		t.Fatal("SMTP host without recipients must not count as configured")
	}
	t.Setenv("KIPPER_SECURITY_SMTP_TO", "security@example.com")
	if !n.EnvPinnedConfigured() {
		t.Fatal("SMTP host with recipients must count as configured")
	}
}
