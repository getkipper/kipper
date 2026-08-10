package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/console-api/mail"
	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/console-api/security"
)

const (
	smtpSecretName      = "kipper-smtp" //nolint:gosec // k8s Secret object name, not a credential value
	smtpSecretNamespace = "kipper-system"
)

// SMTP provides handlers for SMTP email configuration.
type SMTP struct {
	Client kubernetes.Interface
	// Security records configuration changes: repointing SMTP is the first
	// move of an attacker silencing migration alerts, so the change itself
	// alerts — including to the previous destination.
	Security *security.Notifier
}

// notifyConfigChange emits the security event for a notification-channel
// change and warns the previous destination directly, so the party losing
// alerts hears that it lost them.
func (s *SMTP) notifyConfigChange(ctx context.Context, r *http.Request, old *smtpConfig, updated smtpConfig) {
	if s.Security == nil {
		return
	}
	user := ""
	if claims := middleware.UserFromContext(r.Context()); claims != nil {
		user = claims.Email
	}
	oldHost := ""
	if old != nil {
		oldHost = old.Host
	}
	s.Security.Emit(ctx, security.Event{
		Kind:    "smtp_settings_changed",
		User:    user,
		Summary: fmt.Sprintf("SMTP settings changed by %s", user),
		Fields: []security.Field{
			{Key: "previous_host", Value: oldHost},
			{Key: "new_host", Value: updated.Host},
		},
	})

	// The previous server/recipients only hear about the change through the
	// old configuration, so this delivery cannot use the new one.
	if old == nil || old.Host == "" || s.Security.Console.Admins == nil {
		return
	}
	oldCfg := mailConfig(old)
	body := fmt.Sprintf(
		"<h2>Kipper security event</h2><p>The SMTP settings of this cluster were changed by %s. "+
			"Alert emails now go through %s. If this change is unexpected, treat the cluster as compromised.</p>",
		html.EscapeString(user), html.EscapeString(updated.Host))
	go func() {
		for _, admin := range s.Security.Console.Admins() {
			if err := mail.Send(oldCfg, admin, "[Kipper security] SMTP settings changed", body); err != nil {
				log.Printf("security: previous-destination email to %s failed: %v", admin, err)
			}
		}
	}()
}

type smtpConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
	TLS      bool   `json:"tls"`
}

// Get returns the current SMTP configuration with the password masked.
// GET /api/v1/settings/smtp
func (s *SMTP) Get(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	cfg := loadSMTPConfig(ctx, s.Client)
	if cfg == nil {
		respondJSON(w, http.StatusOK, smtpConfig{Port: 587, TLS: true})
		return
	}

	cfg.Password = maskPassword(cfg.Password)
	respondJSON(w, http.StatusOK, cfg)
}

// Update saves the SMTP configuration.
// PUT /api/v1/settings/smtp
func (s *SMTP) Update(w http.ResponseWriter, r *http.Request) {
	var req smtpConfig
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// The pre-change configuration is the only channel the previous
	// destination can still be reached on, so capture it before the write.
	previous := loadSMTPConfig(ctx, s.Client)

	// If the password is masked, preserve the existing one
	if strings.Contains(req.Password, "***") && previous != nil {
		req.Password = previous.Password
	}

	data, _ := json.Marshal(req) //nolint:gosec // serialising SMTP config to a k8s Secret is the intended sink for the password

	secret, err := s.Client.CoreV1().Secrets(smtpSecretNamespace).Get(ctx, smtpSecretName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      smtpSecretName,
				Namespace: smtpSecretNamespace,
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper"},
			},
			Data: map[string][]byte{"config": data},
		}
		if _, err := s.Client.CoreV1().Secrets(smtpSecretNamespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to create smtp secret")
			return
		}
		s.notifyConfigChange(ctx, r, previous, req)
		req.Password = maskPassword(req.Password)
		respondJSON(w, http.StatusOK, req)
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read smtp secret")
		return
	}

	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	secret.Data["config"] = data

	if _, err := s.Client.CoreV1().Secrets(smtpSecretNamespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update smtp secret")
		return
	}

	s.notifyConfigChange(ctx, r, previous, req)
	req.Password = maskPassword(req.Password)
	respondJSON(w, http.StatusOK, req)
}

// Test sends a test email to the current admin's email address.
// POST /api/v1/settings/smtp/test
func (s *SMTP) Test(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	cfg := loadSMTPConfig(ctx, s.Client)
	if cfg == nil || cfg.Host == "" {
		respondError(w, http.StatusBadRequest, "SMTP not configured")
		return
	}

	var req struct {
		To string `json:"to"`
	}
	_ = decodeJSON(r, &req)

	to := req.To
	if to == "" {
		claims := middleware.UserFromContext(r.Context())
		if claims == nil || claims.Email == "" {
			respondError(w, http.StatusBadRequest, "provide a recipient email or log in with an account that has one")
			return
		}
		to = claims.Email
	}

	emailSvc := &EmailService{Client: s.Client}
	err := emailSvc.Send(ctx, to, "Kipper SMTP test", "<h2>SMTP configuration is working</h2><p>This test email was sent from your Kipper cluster.</p>")
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "sent", "to": to})
}

func loadSMTPConfig(ctx context.Context, client kubernetes.Interface) *smtpConfig {
	secret, err := client.CoreV1().Secrets(smtpSecretNamespace).Get(ctx, smtpSecretName, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	var cfg smtpConfig
	if err := json.Unmarshal(secret.Data["config"], &cfg); err != nil {
		return nil
	}
	return &cfg
}

func maskPassword(pw string) string {
	if pw == "" {
		return ""
	}
	if len(pw) <= 4 {
		return strings.Repeat("*", len(pw))
	}
	return pw[:2] + strings.Repeat("*", len(pw)-2)
}
