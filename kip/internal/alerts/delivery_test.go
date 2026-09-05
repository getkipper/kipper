package alerts

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// The incident ran for three and a half days with an alert firing every hour
// into a console bell nobody had open. No Slack webhook was configured and no
// SMTP either, so every one of them stopped at the cluster edge. The console now
// says so; an operator who lives in a terminal has to be told too.

func slackSecret(url string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-slack", Namespace: "kipper-system"},
		Data:       map[string][]byte{"webhook_url": []byte(url)},
	}
}

func smtpSecret(t *testing.T, host string) *corev1.Secret {
	t.Helper()
	cfg, err := json.Marshal(map[string]any{"host": host, "port": 587, "from": "kipper@example.com"})
	if err != nil {
		t.Fatalf("marshalling smtp config: %v", err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-smtp", Namespace: "kipper-system"},
		Data:       map[string][]byte{"config": cfg},
	}
}

func TestRouteFor(t *testing.T) {
	tests := []struct {
		name    string
		objects []runtime
		want    Route
	}{
		{"slack carries it", []runtime{slackSecret("https://hooks.example.com/abc")}, Slack},
		{"email when there is no webhook", []runtime{smtpSecret(t, "smtp.example.com")}, Email},
		{"slack wins over email", []runtime{slackSecret("https://hooks.example.com/abc"), smtpSecret(t, "smtp.example.com")}, Slack},
		{"neither, which is what the incident looked like", nil, Nowhere},
		{"an empty webhook is not a webhook", []runtime{slackSecret(""), smtpSecret(t, "smtp.example.com")}, Email},
		{"an smtp config with no host is not configured", []runtime{smtpSecret(t, "")}, Nowhere},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The email route needs a recipient as well as a server, so every
			// case here supplies one. Whether the recipient matters is
			// TestRouteForNeedsSomebodyToEmail's subject.
			objects := append(toObjects(tc.objects), adminsConfigMap(map[string]string{"ops@example.com": "admin"}))
			client := fake.NewClientset(objects...)
			if got := RouteFor(context.Background(), client); got != tc.want {
				t.Errorf("RouteFor() = %q, want %q", got, tc.want)
			}
		})
	}
}

type runtime = *corev1.Secret

func toObjects(secrets []runtime) []k8sruntime.Object {
	out := make([]k8sruntime.Object, 0, len(secrets))
	for _, s := range secrets {
		out = append(out, s)
	}
	return out
}

// These names are console-api's, read here because kip and console-api are
// separate modules and neither can import the other. A rename on either side
// has to be made on both, and a mismatch is silent: kip reads an absent key,
// finds nothing, and reports that alerts go nowhere while Slack is delivering
// them.
//
// The names are pinned here against console-api/handlers/slack.go and
// email.go. If one of those changes, this fails and says which.
func TestSecretNamesMatchConsoleAPI(t *testing.T) {
	for _, tc := range []struct{ name, got, want string }{
		{"namespace", secretNamespace, "kipper-system"},
		{"slack secret", slackSecretName, "kipper-slack"},
		{"slack webhook key", slackWebhookKey, "webhook_url"},
		{"smtp secret", smtpSecretName, "kipper-smtp"},
		{"smtp config key", smtpConfigKey, "config"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s is %q here and %q in console-api; kip would read an absent key and report the wrong route",
				tc.name, tc.got, tc.want)
		}
	}
}

// An SMTP server with no admin to email delivers nothing, so kip status must
// call it what it is. console-api reaches the same verdict from its role store;
// this reads the same ConfigMap, because a CLI that disagrees with the console
// about whether alerts are leaving is the false assurance both are here to
// remove.
func adminsConfigMap(roles map[string]string) *corev1.ConfigMap {
	data, _ := json.Marshal(roles)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-users", Namespace: "kipper-system"},
		Data:       map[string]string{"users": string(data)},
	}
}

func TestRouteForNeedsSomebodyToEmail(t *testing.T) {
	tests := []struct {
		name  string
		roles map[string]string
		want  Route
	}{
		{
			name:  "an admin to email",
			roles: map[string]string{"ops@example.com": "admin"},
			want:  Email,
		},
		{
			name:  "no users at all",
			roles: nil,
			want:  Nowhere,
		},
		{
			name:  "users, but none of them an admin",
			roles: map[string]string{"dev@example.com": "deployer", "read@example.com": "viewer"},
			want:  Nowhere,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewClientset(smtpSecret(t, "smtp.example.com"), adminsConfigMap(tc.roles))
			if got := RouteFor(context.Background(), client); got != tc.want {
				t.Errorf("RouteFor() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Slack needs no admin list, so an empty one must not take a working webhook off
// the table.
func TestRouteForSlackDoesNotNeedAdmins(t *testing.T) {
	client := fake.NewClientset(slackSecret("https://hooks.example.com/abc"), adminsConfigMap(nil))
	if got := RouteFor(context.Background(), client); got != Slack {
		t.Errorf("RouteFor() = %q, want %q", got, Slack)
	}
}

// The reason is what kip status turns into advice, and "configure SMTP" is
// wrong advice for a cluster that already has it.
func TestNowhereReasonDistinguishesTheTwoCases(t *testing.T) {
	noChannel := fake.NewClientset(adminsConfigMap(map[string]string{"ops@example.com": "admin"}))
	if got := NowhereReason(context.Background(), noChannel); got != NoChannel {
		t.Errorf("NowhereReason() = %q, want %q", got, NoChannel)
	}

	noAdmins := fake.NewClientset(smtpSecret(t, "smtp.example.com"), adminsConfigMap(nil))
	if got := NowhereReason(context.Background(), noAdmins); got != NoRecipients {
		t.Errorf("NowhereReason() = %q, want %q", got, NoRecipients)
	}
}
