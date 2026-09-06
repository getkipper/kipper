// Package mail sends multipart plain+HTML email over SMTP. It is the shared
// transport for the console-configured email service and the env-pinned
// security channel, which differ only in where their configuration comes from.
package mail

import (
	"context"
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

	// insecureSkipVerify is for the in-process server the deadline tests run
	// against, which serves a throwaway certificate. Unexported so no caller
	// outside this package can reach it, which is the point: there is no
	// configuration path to it and no way to turn it on by accident.
	insecureSkipVerify bool
}

// Send delivers an email to a single recipient using the given SMTP settings.
//
// The context bounds the whole exchange. An alert batch gives its delivery
// twenty seconds for every recipient it has, and without that reaching the
// socket a silent relay holds each send for the transport's own minute: a batch
// of twenty-five alerts to three admins would run for over an hour while the
// caller believed it had given up long before.
func Send(ctx context.Context, cfg Config, to, subject, htmlBody string) error {
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
	return deliver(ctx, addr, cfg, to, msg.String())
}

// dialTimeout bounds reaching the server.
const dialTimeout = 10 * time.Second

// transactionTimeout bounds everything after that. A server that accepts the
// connection and then stops answering is an ordinary failure, and without this
// the send waits for good: the alerts behind it are never delivered and the
// goroutine never returns. Generous, because a large message to a slow relay is
// not a fault.
//
// A variable so a test can prove the bound without waiting a minute for it.
var transactionTimeout = 60 * time.Second

// deliver runs one SMTP transaction, bounded end to end.
func deliver(ctx context.Context, addr string, cfg Config, to, msg string) error {
	conn, err := dial(ctx, addr, cfg)
	if err != nil {
		return err
	}
	// One deadline over the whole conversation rather than per command, so a
	// server that answers each step slowly enough still cannot hold the
	// goroutine indefinitely. The caller's is used where it is shorter.
	if err := conn.SetDeadline(deadlineFor(ctx)); err != nil {
		_ = conn.Close()
		return fmt.Errorf("setting the SMTP deadline: %w", err)
	}

	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("SMTP client: %w", err)
	}
	defer func() { _ = c.Close() }()

	if !cfg.TLS {
		// STARTTLS where the server offers it, which is what smtp.SendMail
		// does and what keeps PlainAuth willing to send credentials.
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsConfig(cfg)); err != nil {
				return fmt.Errorf("SMTP STARTTLS: %w", err)
			}
		}
	}

	// A relay that accepts mail without authenticating is a normal
	// configuration, so no credentials means no AUTH. But credentials that were
	// configured are an instruction: a server that offers no way to use them is
	// an error rather than a licence to send the mail anonymously, which is
	// also what an AUTH-stripping attacker on a cleartext link would arrange.
	if cfg.Username != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return fmt.Errorf("SMTP server offers no AUTH, but credentials are configured")
		}
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
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

// dial opens the connection, with TLS from the first byte where the settings ask
// for it and plain otherwise.
func dial(ctx context.Context, addr string, cfg Config) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: dialTimeout}
	if !cfg.TLS {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("SMTP dial: %w", err)
		}
		return conn, nil
	}

	conn, err := (&tls.Dialer{Config: tlsConfig(cfg), NetDialer: dialer}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("TLS dial: %w", err)
	}
	return conn, nil
}

func tlsConfig(cfg Config) *tls.Config {
	return &tls.Config{
		ServerName:         cfg.Host,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.insecureSkipVerify, //nolint:gosec // unexported, set only by this package's deadline tests
	}
}

// deadlineFor is the transport's own limit, or the caller's where that is
// sooner.
func deadlineFor(ctx context.Context) time.Time {
	ours := time.Now().Add(transactionTimeout)
	if theirs, ok := ctx.Deadline(); ok && theirs.Before(ours) {
		return theirs
	}
	return ours
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
