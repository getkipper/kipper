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
		Data:       map[string][]byte{"webhook-url": []byte(url)},
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
			client := fake.NewClientset(toObjects(tc.objects)...)
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
