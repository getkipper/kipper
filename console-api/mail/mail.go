// Package mail sends multipart plain+HTML email over SMTP. It is the shared
// transport for the console-configured email service and the env-pinned
// security channel, which differ only in where their configuration comes from.
package mail

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// Config holds the SMTP connection settings for one delivery.
type Config struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	TLS      bool
}

// Send delivers an email to a single recipient using the given SMTP settings.
func Send(cfg Config, to, subject, htmlBody string) error {
	if cfg.Host == "" {
		return fmt.Errorf("SMTP not configured")
	}

	plainBody := StripHTML(htmlBody)
	boundary := "kipper-boundary-0123456789"

	var msg strings.Builder
	fmt.Fprintf(&msg, "From: %s\r\n", cfg.From)
	fmt.Fprintf(&msg, "To: %s\r\n", to)
	fmt.Fprintf(&msg, "Subject: %s\r\n", subject)
	msg.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&msg, "Content-Type: multipart/alternative; boundary=%q\r\n", boundary)
	msg.WriteString("\r\n")

	fmt.Fprintf(&msg, "--%s\r\n", boundary)
	msg.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
	msg.WriteString(plainBody)
	msg.WriteString("\r\n")

	fmt.Fprintf(&msg, "--%s\r\n", boundary)
	msg.WriteString("Content-Type: text/html; charset=UTF-8\r\n\r\n")
	msg.WriteString(htmlBody)
	msg.WriteString("\r\n")

	fmt.Fprintf(&msg, "--%s--\r\n", boundary)

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)

	if cfg.TLS {
		return sendWithTLS(addr, auth, cfg, to, msg.String())
	}
	return smtp.SendMail(addr, auth, cfg.From, []string{to}, []byte(msg.String()))
}

func sendWithTLS(addr string, auth smtp.Auth, cfg Config, to, msg string) error {
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 10 * time.Second},
		"tcp", addr,
		&tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12},
	)
	if err != nil {
		return fmt.Errorf("TLS dial: %w", err)
	}

	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fmt.Errorf("SMTP client: %w", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Auth(auth); err != nil {
		return fmt.Errorf("SMTP auth: %w", err)
	}
	if err := c.Mail(extractEmail(cfg.From)); err != nil {
		return fmt.Errorf("SMTP MAIL: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("SMTP RCPT: %w", err)
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	if _, err := fmt.Fprint(w, msg); err != nil {
		return fmt.Errorf("writing email: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("closing email: %w", err)
	}

	return c.Quit()
}

// extractEmail pulls the bare email from "Name <email>" format.
func extractEmail(from string) string {
	if idx := strings.Index(from, "<"); idx != -1 {
		end := strings.Index(from, ">")
		if end > idx {
			return from[idx+1 : end]
		}
	}
	return from
}

// StripHTML provides a basic plain text fallback by removing HTML tags.
func StripHTML(html string) string {
	var out strings.Builder
	inTag := false
	for _, r := range html {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			out.WriteRune(r)
		}
	}
	return strings.TrimSpace(out.String())
}
