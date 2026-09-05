package handlers

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// The incident this covers: a Postgres service crash-looped for three and a half
// days behind a read-only volume. The alert fired hourly into the console bell,
// no Slack webhook was configured, and so nothing ever left the cluster. An
// alert store with no outbound channel is the failure, not the storage.

func slackSecret(url string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: slackSecretName, Namespace: slackSecretNamespace},
		Data:       map[string][]byte{slackSecretKey: []byte(url)},
	}
}

func smtpSecret(t *testing.T, host string) *corev1.Secret {
	t.Helper()
	//nolint:gosec // G117: an SMTP config fixture with no password set; the field is the type's, not a secret.
	cfg, err := json.Marshal(smtpConfig{Host: host, Port: 587, From: "kipper@example.com"})
	if err != nil {
		t.Fatalf("marshalling smtp config: %v", err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: smtpSecretName, Namespace: smtpSecretNamespace},
		Data:       map[string][]byte{"config": cfg},
	}
}

// withAdmin gives the cluster somebody to email for the duration of a test.
// The email route needs a recipient as much as it needs a server, so a test
// asserting that route has to supply one.
func withAdmin(t *testing.T) {
	t.Helper()
	restore := adminRecipients
	adminRecipients = func() []string { return []string{"ops@example.com"} }
	t.Cleanup(func() { adminRecipients = restore })
}

func TestDeliveryRoute(t *testing.T) {
	withAdmin(t)

	tests := []struct {
		name    string
		objects []runtimeObject
		want    DeliveryRoute
	}{
		{
			name: "slack configured, so slack carries it",
			objects: []runtimeObject{
				slackSecret("https://hooks.example.com/abc"),
			},
			want: DeliverySlack,
		},
		{
			name: "no slack, but smtp is set, so email carries it",
			objects: []runtimeObject{
				smtpSecret(t, "smtp.example.com"),
			},
			want: DeliveryEmail,
		},
		{
			name: "slack wins when both are configured, so one alert is not sent twice",
			objects: []runtimeObject{
				slackSecret("https://hooks.example.com/abc"),
				smtpSecret(t, "smtp.example.com"),
			},
			want: DeliverySlack,
		},
		{
			name:    "neither configured, which is what the incident looked like",
			objects: nil,
			want:    DeliveryNowhere,
		},
		{
			name: "a slack secret holding an empty webhook counts as unconfigured",
			objects: []runtimeObject{
				slackSecret(""),
				smtpSecret(t, "smtp.example.com"),
			},
			want: DeliveryEmail,
		},
		{
			name: "an smtp secret with no host counts as unconfigured",
			objects: []runtimeObject{
				smtpSecret(t, ""),
			},
			want: DeliveryNowhere,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeClient(tc.objects...)
			if got := RouteFor(context.Background(), client); got != tc.want {
				t.Errorf("RouteFor() = %q, want %q", got, tc.want)
			}
		})
	}
}

// AlertsGoNowhere is what the console and kip status ask, so an operator is told
// the bell is filling in silence rather than discovering it during an incident.
func TestAlertsGoNowhere(t *testing.T) {
	t.Run("true when no channel is configured", func(t *testing.T) {
		if !AlertsGoNowhere(context.Background(), newFakeClient()) {
			t.Error("AlertsGoNowhere() = false with no channels, want true")
		}
	})

	t.Run("false once email is configured", func(t *testing.T) {
		withAdmin(t)
		client := newFakeClient(smtpSecret(t, "smtp.example.com"))
		if AlertsGoNowhere(context.Background(), client) {
			t.Error("AlertsGoNowhere() = true with smtp configured, want false")
		}
	})
}

type runtimeObject = *corev1.Secret

func newFakeClient(objs ...runtimeObject) kubernetes.Interface {
	if len(objs) == 0 {
		return fake.NewClientset()
	}
	items := make([]interface{}, 0, len(objs))
	for _, o := range objs {
		items = append(items, o)
	}
	switch len(items) {
	case 1:
		return fake.NewClientset(objs[0])
	default:
		return fake.NewClientset(objs[0], objs[1])
	}
}

// An SMTP server with nobody to send to is the "nowhere" case, not the email
// case. Reporting email would recreate the property that made the incident last
// three and a half days: the console and kip status showing outbound coverage
// while every alert stops inside the cluster.
func TestRouteForNeedsSomebodyToEmail(t *testing.T) {
	restore := adminRecipients
	defer func() { adminRecipients = restore }()

	client := newFakeClient(smtpSecret(t, "smtp.example.com"))

	adminRecipients = func() []string { return nil }
	if got := RouteFor(context.Background(), client); got != DeliveryNowhere {
		t.Errorf("RouteFor() = %q with SMTP configured and no admins, want %q", got, DeliveryNowhere)
	}

	adminRecipients = func() []string { return []string{"ops@example.com"} }
	if got := RouteFor(context.Background(), client); got != DeliveryEmail {
		t.Errorf("RouteFor() = %q with an admin to email, want %q", got, DeliveryEmail)
	}
}

// Slack does not need an admin list, so the absence of admins must not take a
// working webhook off the table.
func TestRouteForSlackDoesNotNeedAdmins(t *testing.T) {
	restore := adminRecipients
	defer func() { adminRecipients = restore }()
	adminRecipients = func() []string { return nil }

	client := newFakeClient(slackSecret("https://hooks.example.com/abc"))
	if got := RouteFor(context.Background(), client); got != DeliverySlack {
		t.Errorf("RouteFor() = %q, want %q", got, DeliverySlack)
	}
}
