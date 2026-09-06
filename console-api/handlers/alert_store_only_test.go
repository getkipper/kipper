package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// A security event already has its own email: a richer body, a recipient
// snapshot taken before the change that caused it, and an env-pinned channel.
// It is fed into the bell as well so it shows in the console, and once the bell
// learned to email admins that meant two emails per admin for every security
// action. The bell stores; the security notifier delivers.

func TestStoringAnAlertDeliversNothing(t *testing.T) {
	client := fake.NewClientset(smtpSecret(t, "smtp.example.com"))

	sent := make(chan string, 4)
	restore := adminRecipients
	SetAdminRecipients(func() []string { sent <- "admins read"; return []string{"ops@example.com"} })
	defer func() { SetAdminRecipients(restore) }()

	_ = storeAlerts(context.Background(), client, []Alert{{
		Time:     time.Now().UTC().Format(time.RFC3339),
		Action:   "security",
		Severity: "critical",
		Reason:   "a git credential was revoked",
	}})

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
func TestAddAlertsStillDelivers(t *testing.T) {
	client := fake.NewClientset(smtpSecret(t, "smtp.example.com"))

	reached := make(chan struct{}, 1)
	restore := adminRecipients
	SetAdminRecipients(func() []string { reached <- struct{}{}; return nil })
	defer func() { SetAdminRecipients(restore) }()

	_ = AddAlerts(context.Background(), client, []Alert{{
		Time:   time.Now().UTC().Format(time.RFC3339),
		Action: "CrashLoopBackOff",
		Reason: "container is crash-looping",
	}})

	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("an ordinary alert was stored but never delivered")
	}
}

// A security event is delivered by the security notifier, which owns its email:
// a fuller body, a recipient list snapshotted before the change that caused the
// event, and an env-pinned channel. It has no Slack sender of its own, so the
// bell hook has always been how a security event reached the console-configured
// webhook.
//
// Storing without delivering fixed the duplicate email and silently took Slack
// away with it. On a cluster with a webhook and no SMTP, which is the ordinary
// setup, every security event then went nowhere but the bell.
func TestSecurityAlertReachesSlackButNotEmail(t *testing.T) {
	client := fake.NewClientset(
		slackSecret("https://hooks.example.com/abc"),
		smtpSecret(t, "smtp.example.com"),
	)

	posted := make(chan Alert, 2)
	emailed := make(chan string, 2)
	restore := deliveryBuilder
	setDeliveryBuilder(func(_ context.Context, _ kubernetes.Interface) batchDelivery {
		return batchDelivery{
			route:     DeliverySlack,
			admins:    func() []string { return []string{"ops@example.com"} },
			sendSlack: func(_ context.Context, a Alert) error { posted <- a; return nil },
			sendEmail: func(_ context.Context, to, _, _ string) error { emailed <- to; return nil },
		}
	})
	defer func() { setDeliveryBuilder(restore) }()

	StoreSecurityAlert(context.Background(), client, Alert{
		Time:     time.Now().UTC().Format(time.RFC3339),
		Action:   "security",
		Severity: "critical",
		Reason:   "a git credential was revoked",
	})

	select {
	case a := <-posted:
		if a.Reason != "a git credential was revoked" {
			t.Errorf("posted the wrong alert: %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a security event never reached the configured Slack webhook")
	}

	select {
	case to := <-emailed:
		t.Errorf("also emailed %s; the security notifier sends that email itself, so this is the second copy", to)
	case <-time.After(150 * time.Millisecond):
	}
}

// With no webhook, the bell hook sends nothing at all: the notifier's own email
// is the delivery, and a second thinner copy is what this avoids.
func TestSecurityAlertSendsNothingWithoutAWebhook(t *testing.T) {
	client := fake.NewClientset(smtpSecret(t, "smtp.example.com"))

	touched := make(chan struct{}, 2)
	restore := deliveryBuilder
	setDeliveryBuilder(func(_ context.Context, _ kubernetes.Interface) batchDelivery {
		return batchDelivery{
			route:     DeliveryEmail,
			admins:    func() []string { touched <- struct{}{}; return []string{"ops@example.com"} },
			sendEmail: func(context.Context, string, string, string) error { touched <- struct{}{}; return nil },
		}
	})
	defer func() { setDeliveryBuilder(restore) }()

	StoreSecurityAlert(context.Background(), client, Alert{
		Time: time.Now().UTC().Format(time.RFC3339), Action: "security", Reason: "2FA was reset",
	})

	select {
	case <-touched:
		t.Error("the bell hook delivered a security event the notifier also delivers")
	case <-time.After(150 * time.Millisecond):
	}
}

// The bell and Slack are two channels, and the bell failing is not a reason for
// Slack to hear nothing. A ConfigMap write that fails takes the console record
// with it; the webhook is what reaches somebody who is not looking at the
// console, which for a security event is the point.
func TestSecurityAlertReachesSlackEvenWhenTheBellWriteFails(t *testing.T) {
	client := fake.NewClientset(slackSecret("https://hooks.example.com/abc"))
	client.PrependReactor("*", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, assert.AnError
	})

	posted := make(chan Alert, 1)
	restore := deliveryBuilder
	setDeliveryBuilder(func(_ context.Context, _ kubernetes.Interface) batchDelivery {
		return batchDelivery{
			route:     DeliverySlack,
			admins:    func() []string { return nil },
			sendSlack: func(_ context.Context, a Alert) error { posted <- a; return nil },
		}
	})
	defer func() { setDeliveryBuilder(restore) }()

	StoreSecurityAlert(context.Background(), client, Alert{
		Time: time.Now().UTC().Format(time.RFC3339), Action: "security", Reason: "2FA was reset",
	})

	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("the bell write failed and took Slack down with it")
	}
}

// A stalled API server must not delay the webhook either. Removing the success
// dependency was not enough: Slack started only after the write returned, so an
// API server hanging on the ConfigMap held the notification for as long as it
// hung, and a restart in that window lost it.
func TestSecurityAlertReachesSlackWhileTheBellWriteIsStillHanging(t *testing.T) {
	client := fake.NewClientset(slackSecret("https://hooks.example.com/abc"))
	release := make(chan struct{})
	client.PrependReactor("*", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		<-release
		return true, nil, assert.AnError
	})
	defer close(release)

	posted := make(chan Alert, 1)
	restore := deliveryBuilder
	setDeliveryBuilder(func(_ context.Context, _ kubernetes.Interface) batchDelivery {
		return batchDelivery{
			route:     DeliverySlack,
			admins:    func() []string { return nil },
			sendSlack: func(_ context.Context, a Alert) error { posted <- a; return nil },
		}
	})
	defer func() { setDeliveryBuilder(restore) }()

	go StoreSecurityAlert(context.Background(), client, Alert{
		Time: time.Now().UTC().Format(time.RFC3339), Action: "security", Reason: "2FA was reset",
	})

	select {
	case <-posted:
	case <-time.After(3 * time.Second):
		t.Fatal("Slack waited for a write that had not returned")
	}
}
