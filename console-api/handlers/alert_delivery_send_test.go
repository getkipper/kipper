package handlers

import (
	"context"
	"strings"
	"testing"
)

// deliverBatch is what AddAlerts calls once the alerts are stored. These tests
// cover the choice it makes, not the transports themselves: Slack and SMTP are
// exercised by their own tests, and neither should be reached twice for one
// alert.

type recordedSend struct {
	to      string
	subject string
	body    string
}

func TestDeliverBatch(t *testing.T) {
	alerts := []Alert{
		{Time: "2026-09-01T06:58:05Z", Namespace: "payroll-test", App: "db",
			Action: "CrashLoopBackOff", Severity: "critical",
			Reason: "could not remove old lock file: Read-only file system"},
	}

	t.Run("emails the admins when no webhook is configured", func(t *testing.T) {
		var sent []recordedSend
		d := batchDelivery{
			route: DeliveryEmail,
			admins: func() []string {
				return []string{"ops@example.com", "second@example.com"}
			},
			sendEmail: func(_ context.Context, to, subject, body string) error {
				sent = append(sent, recordedSend{to, subject, body})
				return nil
			},
		}

		d.deliver(context.Background(), alerts)

		if len(sent) != 2 {
			t.Fatalf("sent %d emails, want one per admin (2)", len(sent))
		}
		if !strings.Contains(sent[0].subject, "payroll-test") || !strings.Contains(sent[0].subject, "db") {
			t.Errorf("subject %q names neither the namespace nor the service", sent[0].subject)
		}
		if !strings.Contains(sent[0].body, "Read-only file system") {
			t.Errorf("body drops the reason, which is the part that identifies the failure: %q", sent[0].body)
		}
	})

	t.Run("sends no email when a webhook is configured", func(t *testing.T) {
		called := false
		d := batchDelivery{
			route:     DeliverySlack,
			admins:    func() []string { return []string{"ops@example.com"} },
			sendEmail: func(context.Context, string, string, string) error { called = true; return nil },
		}

		d.deliver(context.Background(), alerts)

		if called {
			t.Error("emailed as well as posting to Slack; one alert should travel one route")
		}
	})

	t.Run("carries on when one admin's address fails", func(t *testing.T) {
		var reached []string
		d := batchDelivery{
			route:  DeliveryEmail,
			admins: func() []string { return []string{"broken@example.com", "ops@example.com"} },
			sendEmail: func(_ context.Context, to, _, _ string) error {
				reached = append(reached, to)
				if to == "broken@example.com" {
					return errSendFailed
				}
				return nil
			},
		}

		d.deliver(context.Background(), alerts)

		if len(reached) != 2 {
			t.Errorf("stopped after a failed address; reached %v, want both tried", reached)
		}
	})

	t.Run("does nothing when there is nowhere to send", func(t *testing.T) {
		called := false
		d := batchDelivery{
			route:     DeliveryNowhere,
			admins:    func() []string { return []string{"ops@example.com"} },
			sendEmail: func(context.Context, string, string, string) error { called = true; return nil },
		}

		d.deliver(context.Background(), alerts)

		if called {
			t.Error("sent an email with no channel configured")
		}
	})

	t.Run("sends nothing when the route is email but no admin exists", func(t *testing.T) {
		called := false
		d := batchDelivery{
			route:     DeliveryEmail,
			admins:    func() []string { return nil },
			sendEmail: func(context.Context, string, string, string) error { called = true; return nil },
		}

		d.deliver(context.Background(), alerts)

		if called {
			t.Error("tried to send to nobody")
		}
	})
}
