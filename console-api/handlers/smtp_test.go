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

func TestSMTP_Get_NoConfig(t *testing.T) {
	client := fake.NewClientset()
	handler := &SMTP{Client: client}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/settings/smtp", nil)

	handler.Get(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp smtpConfig
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if resp.Host != "" {
		t.Errorf("expected empty host, got %q", resp.Host)
	}
	if resp.Port != 587 {
		t.Errorf("expected default port 587, got %d", resp.Port)
	}
	if !resp.TLS {
		t.Error("expected TLS true by default")
	}
}

func TestSMTP_Update_Creates(t *testing.T) {
	client := fake.NewClientset()
	handler := &SMTP{Client: client}

	cfg := smtpConfig{
		Host:     "smtp.example.com",
		Port:     465,
		Username: "user@example.com",
		Password: "secret123",
		From:     "Kipper <noreply@example.com>",
		TLS:      true,
	}
	body, _ := json.Marshal(cfg) //nolint:gosec // test fixture: serialising the SMTP config shape is the whole point of the test

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/settings/smtp", bytes.NewReader(body))

	handler.Update(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", w.Code, w.Body.String())
	}

	var resp smtpConfig
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if resp.Host != "smtp.example.com" {
		t.Errorf("expected host smtp.example.com, got %q", resp.Host)
	}
	if !strings.Contains(resp.Password, "*") {
		t.Errorf("expected password to be masked, got %q", resp.Password)
	}
}

func TestSMTP_Get_Masked(t *testing.T) {
	cfg := smtpConfig{
		Host:     "smtp.example.com",
		Port:     587,
		Username: "user@example.com",
		Password: "mysecretpassword",
		From:     "Test <test@example.com>",
		TLS:      true,
	}
	data, _ := json.Marshal(cfg) //nolint:gosec // test fixture: serialising the SMTP config shape is the whole point of the test

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      smtpSecretName,
			Namespace: smtpSecretNamespace,
		},
		Data: map[string][]byte{"config": data},
	}

	client := fake.NewClientset(secret)
	handler := &SMTP{Client: client}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/settings/smtp", nil)

	handler.Get(w, r)

	var resp smtpConfig
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if resp.Password == "mysecretpassword" {
		t.Error("expected password to be masked")
	}
	if !strings.HasPrefix(resp.Password, "my") {
		t.Errorf("expected masked password to preserve first 2 chars, got %q", resp.Password)
	}
	if resp.Host != "smtp.example.com" {
		t.Errorf("expected host preserved, got %q", resp.Host)
	}
}

func TestMaskPassword(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"ab", "**"},
		{"abcd", "****"},
		{"abcde", "ab***"},
		{"mysecretpassword", "my**************"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := maskPassword(tt.input)
			if got != tt.want {
				t.Errorf("maskPassword(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
