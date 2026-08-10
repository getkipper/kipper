package handlers

import (
	"context"
	"fmt"

	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/console-api/mail"
)

// EmailService sends emails via SMTP using configuration stored in a K8s Secret.
type EmailService struct {
	Client kubernetes.Interface
}

// Send delivers an email using the cluster's SMTP configuration.
func (e *EmailService) Send(ctx context.Context, to, subject, htmlBody string) error {
	cfg := loadSMTPConfig(ctx, e.Client)
	if cfg == nil || cfg.Host == "" {
		return fmt.Errorf("SMTP not configured")
	}
	return mail.Send(mailConfig(cfg), to, subject, htmlBody)
}

// Configured returns true if SMTP settings have been saved.
func (e *EmailService) Configured(ctx context.Context) bool {
	cfg := loadSMTPConfig(ctx, e.Client)
	return cfg != nil && cfg.Host != ""
}

// mailConfig converts the stored console SMTP settings to the transport shape.
func mailConfig(cfg *smtpConfig) mail.Config {
	return mail.Config{
		Host:     cfg.Host,
		Port:     cfg.Port,
		Username: cfg.Username,
		Password: cfg.Password,
		From:     cfg.From,
		TLS:      cfg.TLS,
	}
}
