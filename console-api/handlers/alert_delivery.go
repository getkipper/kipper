package handlers

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
)

// DeliveryRoute names where an alert leaves the cluster, if it leaves at all.
type DeliveryRoute string

const (
	// DeliverySlack is the configured webhook.
	DeliverySlack DeliveryRoute = "slack"
	// DeliveryEmail is the console's SMTP settings, used when no webhook is set.
	DeliveryEmail DeliveryRoute = "email"
	// DeliveryNowhere means the alert reaches the console bell and stops there.
	DeliveryNowhere DeliveryRoute = "nowhere"
)

// RouteFor reports which channel carries an alert off the cluster.
//
// Slack wins where both are configured, so one alert is never delivered twice.
// Email is the fallback rather than the equal, because a webhook is what an
// operator who configured one is watching.
func RouteFor(ctx context.Context, client kubernetes.Interface) DeliveryRoute {
	return routeGiven(ctx, client, getSlackWebhookURL(ctx, client))
}

// routeGiven picks the route for a webhook already read, so a caller that has to
// deliver through it decides on the same value it will use.
func routeGiven(ctx context.Context, client kubernetes.Interface, webhook string) DeliveryRoute {
	if webhook != "" {
		return DeliverySlack
	}
	// A configured server with no admin to send to delivers nothing, so it is
	// the nowhere case. Calling it email would put "emailed to the cluster
	// admins" on a screen while every alert stops inside the cluster, which is
	// the exact false comfort this route exists to remove.
	if cfg := loadSMTPConfig(ctx, client); cfg != nil && cfg.Host != "" && len(adminRecipients()) > 0 {
		return DeliveryEmail
	}
	return DeliveryNowhere
}

// AlertsGoNowhere reports that the cluster raises alerts nobody receives.
//
// The console and kip status both ask, because a bell filling in silence is
// indistinguishable from a healthy cluster right up until someone looks. A
// service crash-looping for three and a half days behind a read-only volume is
// what that looks like from the outside.
func AlertsGoNowhere(ctx context.Context, client kubernetes.Interface) bool {
	return RouteFor(ctx, client) == DeliveryNowhere
}

// errSendFailed marks a delivery that did not reach its recipient. Delivery
// never fails the caller, so this exists to be logged and to let a test assert
// that one bad address does not stop the rest.
var errSendFailed = errors.New("alert delivery failed")

// batchDelivery carries a batch of alerts down whichever route is configured.
// The route, the recipients and the transport are injected so the choice can be
// tested without a webhook or an SMTP server.
type batchDelivery struct {
	route DeliveryRoute
	// webhook is the URL the route was chosen on, kept so the choice and the
	// delivery cannot be made against two different reads of the Secret.
	webhook   string
	admins    func() []string
	sendEmail func(ctx context.Context, to, subject, body string) error
	sendSlack func(ctx context.Context, alert Alert) error
}

// deliver sends each alert down exactly one route.
//
// Failures are logged rather than returned: this runs off the controller loop,
// the alerts are already stored, and a broken SMTP server must not stop the
// next tick. One unreachable address does not stop the others, because during
// an incident the admin whose mail bounces is rarely the only one on call.
func (d batchDelivery) deliver(ctx context.Context, alerts []Alert) {
	switch d.route {
	case DeliverySlack:
		if d.sendSlack == nil {
			return
		}
		for _, alert := range alerts {
			if err := d.sendSlack(ctx, alert); err != nil {
				log.Printf("alerts: slack send failed for %s/%s: %v", alert.Namespace, alert.App, err)
			}
		}
	case DeliveryEmail:
		if d.sendEmail == nil {
			return
		}
		recipients := d.admins()
		if len(recipients) == 0 {
			log.Printf("alerts: smtp is configured but no admin has an address, so %d alert(s) reached nobody", len(alerts))
			return
		}
		for _, alert := range alerts {
			subject, body := alertEmail(alert)
			for _, to := range recipients {
				if err := d.sendEmail(ctx, to, subject, body); err != nil {
					log.Printf("alerts: email to %s failed for %s/%s: %v", to, alert.Namespace, alert.App, err)
				}
			}
		}
	case DeliveryNowhere:
		log.Printf("alerts: %d alert(s) stored with no delivery channel configured; they reach the console bell and stop there", len(alerts))
	}
}

// alertEmail renders one alert. The subject carries the identity so a phone
// notification is useful without opening anything, which is the whole point of
// the fallback.
func alertEmail(alert Alert) (subject, body string) {
	subject = fmt.Sprintf("[Kipper %s] %s/%s: %s",
		strings.ToUpper(alert.Severity), alert.Namespace, alert.App, alert.Action)

	// Every field is escaped, including the ones that look safe. An alert's
	// reason can quote a line a workload wrote: the read-only detector puts the
	// log a container produced before it died into this message, and markup in
	// it would render inside a mail the reader trusts as Kipper's.
	body = fmt.Sprintf(
		"<p><strong>%s</strong> in <code>%s/%s</code></p><p>%s</p><p>%s</p><hr><p>Sent by Kipper because no Slack webhook is configured. Configure one in the console under Settings to change where these go.</p>",
		html.EscapeString(alert.Action),
		html.EscapeString(alert.Namespace),
		html.EscapeString(alert.App),
		html.EscapeString(alert.Reason),
		html.EscapeString(alert.Time))
	return subject, body
}

// deliveryFor builds the delivery for a cluster: the route it has configured,
// its admin addresses, and the transports.
//
// AdminRecipients is set at startup by main.go, because the role store lives
// there. Leaving it nil costs the email route rather than breaking it, which
// keeps this package usable from a test with no role store at all.
func deliveryFor(ctx context.Context, client kubernetes.Interface) batchDelivery {
	// One read, not two. The route was chosen on the strength of a webhook
	// existing, so it has to be delivered through that same webhook: read
	// twice, a Secret being created or rotated between them pairs the Slack
	// route with no URL, and the alert fails without falling back.
	webhook := getSlackWebhookURL(ctx, client)
	email := &EmailService{Client: client}
	return batchDelivery{
		route:   routeGiven(ctx, client, webhook),
		webhook: webhook,
		admins:  adminRecipients,
		sendEmail: func(ctx context.Context, to, subject, body string) error {
			return email.Send(ctx, to, subject, body)
		},
		sendSlack: func(ctx context.Context, alert Alert) error {
			return SendSlackAlert(ctx, webhook, alert)
		},
	}
}

// adminRecipients returns the addresses the email route delivers to. main.go
// replaces it at startup with a reader over the role store.
var adminRecipients = func() []string { return nil }

// SetAdminRecipients wires the role store in, so an alert with no Slack webhook
// can reach the people who would otherwise only find it in the bell.
func SetAdminRecipients(f func() []string) { adminRecipients = f }

// AlertDelivery answers where this cluster's alerts go, so the console can say
// when the answer is nowhere.
type AlertDelivery struct {
	Client kubernetes.Interface
}

type alertDeliveryResponse struct {
	Route        string `json:"route"`
	GoingNowhere bool   `json:"going_nowhere"`
	// Reason distinguishes a cluster with no channel configured from one whose
	// SMTP server has nobody to send to, because the two need different advice
	// and only one of them is fixed by configuring SMTP.
	Reason string `json:"reason,omitempty"`
}

// Get reports the configured route. It names the channel and never the
// credential, so an operator who cannot read the webhook secret can still be
// told whether one exists.
func (a *AlertDelivery) Get(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	route := RouteFor(ctx, a.Client)
	respondJSON(w, http.StatusOK, alertDeliveryResponse{
		Route:        string(route),
		GoingNowhere: route == DeliveryNowhere,
		Reason:       nowhereReason(ctx, a.Client, route),
	})
}

// nowhereReason says why alerts are not leaving, or "" when they are.
func nowhereReason(ctx context.Context, client kubernetes.Interface, route DeliveryRoute) string {
	if route != DeliveryNowhere {
		return ""
	}
	if cfg := loadSMTPConfig(ctx, client); cfg != nil && cfg.Host != "" {
		return "no_recipients"
	}
	return "no_channel"
}
