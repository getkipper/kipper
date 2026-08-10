// Package security delivers security events for destructive or
// abuse-relevant operations: migration lifecycle, 2FA factor lifecycle,
// and notification-configuration changes.
//
// Every event always lands in the host log as one structured line with an
// event ID — that is the durable audit record. Network deliveries are
// best-effort on top, in two trust classes:
//
//   - The env-pinned channel (KIPPER_SECURITY_SMTP_* / KIPPER_SECURITY_WEBHOOK)
//     is set at install time on the host. A console admin cannot read or
//     change it, so it survives a stolen admin JWT.
//   - The console-configured channels (alert bell + Slack, SMTP email to all
//     admins) are admin-editable and therefore reach admins only while the
//     configuration is untampered. Changes to that configuration are
//     themselves security events, delivered to the previous destination.
package security

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/getkipper/kipper/console-api/mail"
)

// Event is one security-relevant occurrence.
type Event struct {
	// Kind names the occurrence: "migration_started", "twofa_enrolled", ...
	Kind string
	// User is the initiating identity (email), or "" when no JWT was
	// involved (e.g. an inbound migration accept authenticated by token).
	User string
	// Summary is the one-line human description used as the alert reason
	// and email subject.
	Summary string
	// Fields carry structured detail (target endpoint, project list, ...)
	// in a stable order for the log line and the email body.
	Fields []Field
	// Recipients pins the console-email delivery list at emit time. Events
	// about admin-list mutations must set it from the pre-mutation list —
	// resolving recipients at delivery time would let the mutation cut its
	// own victim out of the alert. Empty means resolve Admins() at delivery.
	Recipients []string
}

// Field is one ordered key/value detail on an event.
type Field struct {
	Key   string
	Value string
}

// ConsoleHooks are the admin-editable delivery paths, wired from the
// handlers package. Any of them may be nil, and each is best-effort.
type ConsoleHooks struct {
	// Alert posts to the console alert bell (which also forwards to the
	// console-configured Slack webhook).
	Alert func(ctx context.Context, kind, reason string)
	// Email sends via the console-configured SMTP to one recipient.
	Email func(ctx context.Context, to, subject, htmlBody string) error
	// EmailConfigured reports whether console SMTP settings exist.
	EmailConfigured func(ctx context.Context) bool
	// SlackConfigured reports whether a console Slack webhook exists.
	SlackConfigured func(ctx context.Context) bool
	// Admins lists the email addresses of all cluster admins.
	Admins func() []string
}

// Notifier delivers security events. The zero value logs to the host log
// only; hooks and the environment add the network channels.
type Notifier struct {
	Console ConsoleHooks
}

// Env-pinned channel configuration. Read per event rather than cached, so a
// host-level change needs no restart.
const (
	envSMTPHost     = "KIPPER_SECURITY_SMTP_HOST"
	envSMTPPort     = "KIPPER_SECURITY_SMTP_PORT"
	envSMTPUsername = "KIPPER_SECURITY_SMTP_USERNAME"
	envSMTPPassword = "KIPPER_SECURITY_SMTP_PASSWORD" //nolint:gosec // env var name, not a credential
	envSMTPFrom     = "KIPPER_SECURITY_SMTP_FROM"
	envSMTPTo       = "KIPPER_SECURITY_SMTP_TO"
	envWebhook      = "KIPPER_SECURITY_WEBHOOK"

	deliveryTimeout = 20 * time.Second
)

// Emit records the event in the host log and fans it out to every configured
// channel. The log write is synchronous — it is the audit record — while
// network deliveries run on their own goroutine so no request handler blocks
// on SMTP or a webhook. Delivery failures are themselves logged loudly.
func (n *Notifier) Emit(ctx context.Context, e Event) {
	id := eventID()
	logLine(id, e)

	// Detach from the request context: an admin cancelling the HTTP request
	// must not be able to cancel the notification fan-out.
	base := context.WithoutCancel(ctx)
	go func() {
		dctx, cancel := context.WithTimeout(base, deliveryTimeout)
		defer cancel()
		n.deliverEnvPinned(id, e)
		n.deliverConsole(dctx, id, e)
	}()
}

// OutOfBandConfigured reports whether any delivery channel exists that leaves
// the box: the env-pinned channel, console SMTP, or console Slack. When this
// is false, detection degrades to the console bell — which an attacker
// holding an admin session can watch — and the UI warns before a migration.
func (n *Notifier) OutOfBandConfigured(ctx context.Context) bool {
	if envPinnedConfigured() {
		return true
	}
	if n.Console.EmailConfigured != nil && n.Console.EmailConfigured(ctx) {
		return true
	}
	if n.Console.SlackConfigured != nil && n.Console.SlackConfigured(ctx) {
		return true
	}
	return false
}

// EnvPinnedConfigured reports whether the host-level channel exists. Only
// this channel survives a compromised admin account; the UI distinguishes it
// from the admin-editable ones.
func (n *Notifier) EnvPinnedConfigured() bool {
	return envPinnedConfigured()
}

func envPinnedConfigured() bool {
	return (os.Getenv(envSMTPHost) != "" && os.Getenv(envSMTPTo) != "") ||
		os.Getenv(envWebhook) != ""
}

func (n *Notifier) deliverEnvPinned(id string, e Event) {
	if host := os.Getenv(envSMTPHost); host != "" {
		port := 587
		if p, err := strconv.Atoi(os.Getenv(envSMTPPort)); err == nil && p > 0 {
			port = p
		}
		cfg := mail.Config{
			Host:     host,
			Port:     port,
			Username: os.Getenv(envSMTPUsername),
			Password: os.Getenv(envSMTPPassword),
			From:     os.Getenv(envSMTPFrom),
			TLS:      true,
		}
		if cfg.From == "" {
			cfg.From = "kipper-security@" + host
		}
		for _, to := range splitRecipients(os.Getenv(envSMTPTo)) {
			if err := mail.Send(cfg, to, "[Kipper security] "+e.Summary, emailBody(id, e)); err != nil {
				log.Printf("security: env-pinned email to %s failed for event %s: %v", to, id, err)
			}
		}
	}

	if url := os.Getenv(envWebhook); url != "" {
		if err := postWebhook(url, e); err != nil {
			log.Printf("security: env-pinned webhook failed for event %s: %v", id, err)
		}
	}
}

func (n *Notifier) deliverConsole(ctx context.Context, id string, e Event) {
	if n.Console.Alert != nil {
		n.Console.Alert(ctx, e.Kind, e.Summary)
	}
	if n.Console.Email == nil {
		return
	}
	recipients := e.Recipients
	if len(recipients) == 0 && n.Console.Admins != nil {
		recipients = n.Console.Admins()
	}
	for _, to := range recipients {
		if err := n.Console.Email(ctx, to, "[Kipper security] "+e.Summary, emailBody(id, e)); err != nil {
			log.Printf("security: console email to %s failed for event %s: %v", to, id, err)
		}
	}
}

// logLine writes the append-only audit record. One line, stable key=value
// order, unmissable prefix.
func logLine(id string, e Event) {
	var b strings.Builder
	fmt.Fprintf(&b, "SECURITY event=%s id=%s time=%s", e.Kind, id, time.Now().UTC().Format(time.RFC3339))
	if e.User != "" {
		fmt.Fprintf(&b, " user=%q", e.User)
	}
	for _, f := range e.Fields {
		fmt.Fprintf(&b, " %s=%q", f.Key, f.Value)
	}
	fmt.Fprintf(&b, " summary=%q", e.Summary)
	log.Print(b.String())
}

func emailBody(id string, e Event) string {
	var b strings.Builder
	b.WriteString("<h2>Kipper security event</h2>")
	fmt.Fprintf(&b, "<p>%s</p><table>", html.EscapeString(e.Summary))
	fmt.Fprintf(&b, "<tr><td><b>Event</b></td><td>%s</td></tr>", html.EscapeString(e.Kind))
	fmt.Fprintf(&b, "<tr><td><b>ID</b></td><td>%s</td></tr>", html.EscapeString(id))
	fmt.Fprintf(&b, "<tr><td><b>Time</b></td><td>%s</td></tr>", time.Now().UTC().Format(time.RFC3339))
	if e.User != "" {
		fmt.Fprintf(&b, "<tr><td><b>User</b></td><td>%s</td></tr>", html.EscapeString(e.User))
	}
	for _, f := range e.Fields {
		fmt.Fprintf(&b, "<tr><td><b>%s</b></td><td>%s</td></tr>", html.EscapeString(f.Key), html.EscapeString(f.Value))
	}
	b.WriteString("</table>")
	b.WriteString("<p>If this was not you or an admin you trust, treat the cluster as compromised: reset the account's credentials and 2FA factor now.</p>")
	return b.String()
}

// postWebhook delivers a Slack-compatible payload to the pinned webhook.
func postWebhook(url string, e Event) error {
	var text strings.Builder
	fmt.Fprintf(&text, ":rotating_light: *Kipper security*: %s", e.Summary)
	if e.User != "" {
		fmt.Fprintf(&text, "\nUser: %s", e.User)
	}
	for _, f := range e.Fields {
		fmt.Fprintf(&text, "\n%s: %s", f.Key, f.Value)
	}
	payload, err := json.Marshal(map[string]string{"text": text.String()})
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(payload)) //nolint:gosec // G704: the webhook URL is host-operator-set env config, not request input
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}

func splitRecipients(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// eventID returns a short random identifier tying the log line to the
// deliveries that reference it.
func eventID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// The ID only correlates log lines; the event still records.
		return "unknown"
	}
	return hex.EncodeToString(b)
}
