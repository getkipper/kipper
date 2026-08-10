package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/console-api/security"
)

const (
	slackSecretName      = "kipper-slack"
	slackSecretNamespace = "kipper-system"
	slackSecretKey       = "webhook_url" //nolint:gosec // not a credential, just a ConfigMap key name
)

// Slack provides handlers for Slack webhook configuration.
type Slack struct {
	Client kubernetes.Interface
	// Security records configuration changes: repointing the webhook is the
	// first move of an attacker silencing migration alerts, so the change
	// itself alerts — including to the previous webhook.
	Security *security.Notifier
}

// notifyConfigChange emits the security event for a webhook change and warns
// the previous webhook directly, so the channel losing alerts hears that it
// lost them.
func (s *Slack) notifyConfigChange(ctx context.Context, r *http.Request, previousURL, newURL string) {
	if s.Security == nil {
		return
	}
	user := ""
	if claims := middleware.UserFromContext(r.Context()); claims != nil {
		user = claims.Email
	}
	s.Security.Emit(ctx, security.Event{
		Kind:    "slack_settings_changed",
		User:    user,
		Summary: fmt.Sprintf("Slack alert webhook changed by %s", user),
	})

	if previousURL == "" || previousURL == newURL {
		return
	}
	go func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		alert := Alert{
			Time:     time.Now().UTC().Format(time.RFC3339),
			Action:   "security",
			Severity: "critical",
			Reason:   fmt.Sprintf("The Slack alert webhook of this cluster was changed by %s. Alerts stop arriving here. If this change is unexpected, treat the cluster as compromised.", user),
		}
		if err := SendSlackAlert(sctx, previousURL, alert); err != nil {
			log.Printf("security: previous-webhook notice failed: %v", err)
		}
	}()
}

type slackGetResponse struct {
	WebhookURL string `json:"webhook_url"`
}

type slackUpdateRequest struct {
	WebhookURL string `json:"webhook_url"`
}

// Get returns the current Slack webhook URL (masked).
// GET /api/v1/settings/slack
func (s *Slack) Get(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	url := getSlackWebhookURL(ctx, s.Client)
	respondJSON(w, http.StatusOK, slackGetResponse{WebhookURL: maskWebhookURL(url)})
}

// Update saves a Slack webhook URL to the kipper-slack secret.
// PUT /api/v1/settings/slack
func (s *Slack) Update(w http.ResponseWriter, r *http.Request) {
	var req slackUpdateRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	secret, err := s.Client.CoreV1().Secrets(slackSecretNamespace).Get(ctx, slackSecretName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      slackSecretName,
				Namespace: slackSecretNamespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "kipper",
				},
			},
			Data: map[string][]byte{
				slackSecretKey: []byte(req.WebhookURL),
			},
		}
		if _, err := s.Client.CoreV1().Secrets(slackSecretNamespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to create slack secret")
			return
		}
		s.notifyConfigChange(ctx, r, "", req.WebhookURL)
		respondJSON(w, http.StatusOK, slackGetResponse{WebhookURL: maskWebhookURL(req.WebhookURL)})
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read slack secret")
		return
	}

	previousURL := string(secret.Data[slackSecretKey])
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	secret.Data[slackSecretKey] = []byte(req.WebhookURL)

	if _, err := s.Client.CoreV1().Secrets(slackSecretNamespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update slack secret")
		return
	}

	s.notifyConfigChange(ctx, r, previousURL, req.WebhookURL)
	respondJSON(w, http.StatusOK, slackGetResponse{WebhookURL: maskWebhookURL(req.WebhookURL)})
}

// SlackConfigured reports whether a Slack webhook URL has been saved.
func SlackConfigured(ctx context.Context, client kubernetes.Interface) bool {
	return getSlackWebhookURL(ctx, client) != ""
}

// getSlackWebhookURL reads the webhook URL from the Kubernetes secret.
func getSlackWebhookURL(ctx context.Context, client kubernetes.Interface) string {
	secret, err := client.CoreV1().Secrets(slackSecretNamespace).Get(ctx, slackSecretName, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return string(secret.Data[slackSecretKey])
}

func maskWebhookURL(url string) string {
	if url == "" {
		return ""
	}
	if len(url) <= 20 {
		return url[:4] + strings.Repeat("*", len(url)-4)
	}
	return url[:20] + strings.Repeat("*", len(url)-20)
}

// SendSlackAlert posts an alert to a Slack webhook.
func SendSlackAlert(ctx context.Context, webhookURL string, alert Alert) error {
	emoji := severityEmoji(alert.Severity)
	text := fmt.Sprintf("%s *%s* | %s/%s\n%s\n_%s_",
		emoji, strings.ToUpper(alert.Severity), alert.Namespace, alert.App, alert.Action, alert.Reason)

	payload := map[string]string{"text": text}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling slack payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body)) //nolint:gosec // webhook URL from admin-configured secret
	if err != nil {
		return fmt.Errorf("building slack request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("posting to slack: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("slack returned status %d", resp.StatusCode)
	}
	return nil
}

func severityEmoji(severity string) string {
	switch severity {
	case "warning":
		return "\U0001f7e1" // yellow circle
	case "critical":
		return "\U0001f534" // red circle
	default:
		return "\U0001f7e2" // green circle
	}
}
