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
	"sync"
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

	// envSMTP and envWebhook replace the env-pinned channels in tests, which is
	// how their independence is exercised without a mail server.
	envSMTP    func(ctx context.Context, id string, e Event)
	envWebhook func(ctx context.Context, id string, e Event)

	// postWebhook replaces the webhook transport in tests.
	postWebhook func(ctx context.Context, url string, e Event) error

	// perSendTimeout is the budget one recipient gets, defaulting to
	// defaultPerSendTimeout.
	perSendTimeout time.Duration

	// deliveryTimeout is the budget each channel gets, defaulting to
	// defaultDeliveryTimeout. A field rather than a global so a test can
	// shorten it for its own notifier without writing to something another
	// test's delivery goroutines are reading.
	deliveryTimeout time.Duration
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
)

// defaultDeliveryTimeout is what each channel gets.
const defaultDeliveryTimeout = 20 * time.Second

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
		// A budget each, and no waiting on each other. These channels exist
		// because the others can be broken, so a relay that accepts the
		// connection and then goes quiet, or an API server stalling on the
		// bell's write, must not be able to spend the time the rest need.
		var wg sync.WaitGroup
		for _, deliver := range n.channels() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Best-effort background work calling hooks wired from another
				// package. One of them panicking must not take the process
				// down, and must not stop the other channels either.
				defer func() {
					if r := recover(); r != nil {
						log.Printf("security: a delivery channel panicked for event %s: %v", id, r)
					}
				}()
				n.withBudget(base, func(ctx context.Context) { deliver(ctx, id, e) })
			}()
		}
		wg.Wait()
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

func (n *Notifier) deliverEnvSMTP(ctx context.Context, id string, e Event) {
	host := os.Getenv(envSMTPHost)
	if host == "" {
		return
	}

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
		n.perSend(func(ctx context.Context) {
			if err := mail.Send(ctx, cfg, to, "[Kipper security] "+e.Summary, emailBody(id, e)); err != nil {
				log.Printf("security: env-pinned email to %s failed for event %s: %v", to, id, err)
			}
		})
	}
}

func (n *Notifier) deliverEnvWebhook(ctx context.Context, id string, e Event) {
	url := os.Getenv(envWebhook)
	if url == "" {
		return
	}
	post := n.postWebhook
	if post == nil {
		post = postWebhook
	}
	if err := post(ctx, url, e); err != nil {
		log.Printf("security: env-pinned webhook failed for event %s: %v", id, err)
	}
}

func (n *Notifier) deliverBell(ctx context.Context, _ string, e Event) {
	if n.Console.Alert != nil {
		n.Console.Alert(ctx, e.Kind, e.Summary)
	}
}

func (n *Notifier) deliverConsoleEmail(ctx context.Context, id string, e Event) {
	if n.Console.Email == nil {
		return
	}
	// The recipients are snapshotted on the event where it carries them, so a
	// change of admins between the event and its delivery cannot decide who
	// hears about it.
	recipients := e.Recipients
	if len(recipients) == 0 && n.Console.Admins != nil {
		recipients = n.Console.Admins()
	}
	for _, to := range recipients {
		// A budget per address. The channel's own bounds the channel; one relay
		// that stalls must not spend the time the next address needs.
		n.perSend(func(ctx context.Context) {
			if err := n.Console.Email(ctx, to, "[Kipper security] "+e.Summary, emailBody(id, e)); err != nil {
				log.Printf("security: console email to %s failed for event %s: %v", to, id, err)
			}
		})
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
func postWebhook(ctx context.Context, url string, e Event) error {
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
	// The channel's budget bounds this, with the client's own limit as the
	// ceiling for a caller that gave no deadline.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload)) //nolint:gosec // G704: the webhook URL is host-operator-set env config, not request input
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req) //nolint:gosec // G704: the webhook URL is host-operator-set env config, not request input
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

// withBudget runs one delivery channel under its own deadline.
func (n *Notifier) withBudget(base context.Context, deliver func(context.Context)) {
	budget := n.deliveryTimeout
	if budget <= 0 {
		budget = defaultDeliveryTimeout
	}
	ctx, cancel := context.WithTimeout(base, budget)
	defer cancel()
	deliver(ctx)
}

// channels lists every delivery this notifier has, each of which runs on its own
// budget and none of which waits for another.
func (n *Notifier) channels() []func(context.Context, string, Event) {
	return []func(context.Context, string, Event){
		or(n.envSMTP, n.deliverEnvSMTP),
		or(n.envWebhook, n.deliverEnvWebhook),
		n.deliverBell,
		n.deliverConsoleEmail,
	}
}

// or picks a test's replacement where there is one.
func or(replacement, real func(context.Context, string, Event)) func(context.Context, string, Event) {
	if replacement != nil {
		return replacement
	}
	return real
}

// perSend runs one recipient's delivery under a deadline of its own, detached
// from the channel's.
//
// A channel budget cannot also be a per-send budget when the sends are serial:
// the first address to stall spends it, and every address after it fails its
// dial against an expired context. So the channel's deadline bounds the channel
// and each address gets its own from a fresh base. A channel with many
// recipients can therefore outlive its own budget, which costs nothing: the
// channels run on separate goroutines and none waits for another.
func (n *Notifier) perSend(send func(context.Context)) {
	budget := n.perSendTimeout
	if budget <= 0 {
		budget = defaultPerSendTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	send(ctx)
}

// defaultPerSendTimeout is what one recipient gets.
const defaultPerSendTimeout = 20 * time.Second
