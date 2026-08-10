package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSlack_Get_NoConfig(t *testing.T) {
	client := fake.NewClientset()
	handler := &Slack{Client: client}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/settings/slack", nil)

	handler.Get(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp slackGetResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if resp.WebhookURL != "" {
		t.Errorf("expected empty webhook URL, got %q", resp.WebhookURL)
	}
}

func TestSlack_Update(t *testing.T) {
	client := fake.NewClientset()
	handler := &Slack{Client: client}

	url := "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX" //nolint:gosec // test data
	body, _ := json.Marshal(slackUpdateRequest{WebhookURL: url})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/settings/slack", bytes.NewReader(body))

	handler.Update(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp slackGetResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if !strings.HasPrefix(resp.WebhookURL, "https://hooks.slack.") {
		t.Errorf("expected masked URL to start with the original prefix, got %q", resp.WebhookURL)
	}
	if !strings.Contains(resp.WebhookURL, "*") {
		t.Errorf("expected masked URL to contain asterisks, got %q", resp.WebhookURL)
	}
}

func TestSlack_Get_Masked(t *testing.T) {
	url := "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX" //nolint:gosec // test data
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      slackSecretName,
			Namespace: slackSecretNamespace,
		},
		Data: map[string][]byte{
			slackSecretKey: []byte(url),
		},
	}

	client := fake.NewClientset(secret)
	handler := &Slack{Client: client}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/settings/slack", nil)

	handler.Get(w, r)

	var resp slackGetResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if resp.WebhookURL == url {
		t.Error("expected URL to be masked, but got the full URL")
	}
	if !strings.HasPrefix(resp.WebhookURL, "https://hooks.slack.") {
		t.Errorf("expected masked URL to preserve prefix, got %q", resp.WebhookURL)
	}
}
