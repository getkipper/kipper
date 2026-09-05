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
		{Time: "2026-09-01T06:58:05Z", Namespace: "shop-test", App: "db",
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
		if !strings.Contains(sent[0].subject, "shop-test") || !strings.Contains(sent[0].subject, "db") {
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

// An alert's reason can carry a line a workload wrote. The read-only detector
// quotes the log a container produced before it died, so anything that container
// chose to print reaches an admin's inbox inside a message from Kipper. Rendered
// as markup, that is a link or an image the workload controls in a mail the
// reader trusts.
func TestAlertEmailEscapesWhatTheWorkloadWrote(t *testing.T) {
	_, body := alertEmail(Alert{
		Time:      "2026-09-05T09:00:00Z",
		Namespace: "shop-test",
		App:       "db",
		Action:    "VolumeReadOnly",
		Severity:  "critical",
		Reason:    `disk error: <a href="https://evil.example.com">click here</a> & <img src=x>`,
	})

	if strings.Contains(body, "<a href=") || strings.Contains(body, "<img") {
		t.Errorf("the workload's markup reached the mail body unescaped:\n%s", body)
	}
	if !strings.Contains(body, "&lt;a href=") {
		t.Errorf("the reason should still be readable, escaped:\n%s", body)
	}
}

// The identity fields come from Kubernetes object names, which are constrained,
// but the action is set by the controller and the whole message is HTML. Escape
// every interpolated field rather than reasoning about which ones are safe.
func TestAlertEmailEscapesEveryField(t *testing.T) {
	_, body := alertEmail(Alert{
		Time:      "<time>",
		Namespace: "<ns>",
		App:       "<app>",
		Action:    "<action>",
		Reason:    "<reason>",
	})

	for _, raw := range []string{"<time>", "<ns>", "<app>", "<action>", "<reason>"} {
		if strings.Contains(body, raw) {
			t.Errorf("%s reached the body unescaped:\n%s", raw, body)
		}
	}
}

// Slack renders <https://example.com|text> as a clickable link, so a log line a
// container wrote becomes a link in a message the reader trusts as Kipper's.
// Slack asks for exactly three characters to be escaped in message text.
func TestSlackTextEscapesWhatTheWorkloadWrote(t *testing.T) {
	text := slackAlertText(Alert{
		Namespace: "shop-test",
		App:       "db",
		Action:    "VolumeReadOnly",
		Severity:  "critical",
		Reason:    `disk error <https://evil.example.com|click here> & more`,
	})

	if strings.Contains(text, "<https://") {
		t.Errorf("the workload's link markup reached the Slack message:\n%s", text)
	}
	for _, want := range []string{"&lt;https://", "&gt;", "&amp;"} {
		if !strings.Contains(text, want) {
			t.Errorf("expected %s in the escaped message:\n%s", want, text)
		}
	}
}

// Kipper's own formatting still has to work. Escaping the three characters
// Slack names must not touch the asterisks and underscores that make the
// message readable.
func TestSlackTextKeepsItsOwnFormatting(t *testing.T) {
	text := slackAlertText(Alert{
		Namespace: "shop-test", App: "db",
		Action: "CrashLoopBackOff", Severity: "warning",
		Reason: "container is crash-looping",
	})

	if !strings.Contains(text, "*WARNING*") {
		t.Errorf("the severity lost its emphasis:\n%s", text)
	}
	if !strings.Contains(text, "shop-test/db") {
		t.Errorf("the identity is missing:\n%s", text)
	}
}
