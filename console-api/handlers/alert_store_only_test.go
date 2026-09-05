package handlers

import (
	"context"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

// A security event already has its own email: a richer body, a recipient
// snapshot taken before the change that caused it, and an env-pinned channel.
// It is fed into the bell as well so it shows in the console, and once the bell
// learned to email admins that meant two emails per admin for every security
// action. The bell stores; the security notifier delivers.

func TestStoreAlertPersistsWithoutDelivering(t *testing.T) {
	client := fake.NewClientset(smtpSecret(t, "smtp.example.com"))

	sent := make(chan string, 4)
	restore := adminRecipients
	adminRecipients = func() []string { sent <- "admins read"; return []string{"ops@example.com"} }
	defer func() { adminRecipients = restore }()

	StoreAlert(context.Background(), client, Alert{
		Time:     time.Now().UTC().Format(time.RFC3339),
		Action:   "security",
		Severity: "critical",
		Reason:   "a git credential was revoked",
	})

	select {
	case <-sent:
		t.Fatal("the bell delivered a security event the notifier also delivers; every admin gets it twice")
	case <-time.After(150 * time.Millisecond):
	}

	alerts := (&Alerts{Client: client}).readAlerts(context.Background())
	if len(alerts) != 1 {
		t.Fatalf("stored %d alerts, want the event kept in the bell", len(alerts))
	}
}

// The ordinary path still delivers. Storing without sending is the exception a
// caller asks for, never the default.
func TestAddAlertStillDelivers(t *testing.T) {
	client := fake.NewClientset(smtpSecret(t, "smtp.example.com"))

	reached := make(chan struct{}, 1)
	restore := adminRecipients
	adminRecipients = func() []string { reached <- struct{}{}; return nil }
	defer func() { adminRecipients = restore }()

	AddAlert(context.Background(), client, Alert{
		Time:   time.Now().UTC().Format(time.RFC3339),
		Action: "CrashLoopBackOff",
		Reason: "container is crash-looping",
	})

	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("an ordinary alert was stored but never delivered")
	}
}
