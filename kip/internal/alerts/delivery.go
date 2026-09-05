// Package alerts reads where a cluster's alerts go.
//
// console-api owns delivery; this reads the same two secrets so `kip status` can
// say when an alert has nowhere to leave by. The alternative is asking the
// console-api over HTTP, which needs a session kip does not always have and
// fails in exactly the situation worth reporting.
package alerts

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Route is where an alert leaves the cluster by.
type Route string

const (
	Slack   Route = "slack"
	Email   Route = "email"
	Nowhere Route = "nowhere"
	// Unknown is what a refused lookup gives. Not every operator can read
	// kipper-system, and telling one their alerts go nowhere when the question
	// was never answered would be a false alarm about the one thing this
	// reports.
	Unknown Route = "unknown"
)

// These are console-api's, read here rather than imported: kip and console-api
// are separate modules. A rename on either side has to be made on both, which
// is why the names sit together.
//
//nolint:gosec // G101: these name a Secret and the keys inside it. None is a credential.
const (
	secretNamespace = "kipper-system"
	slackSecretName = "kipper-slack"
	slackWebhookKey = "webhook_url"
	smtpSecretName  = "kipper-smtp"
	smtpConfigKey   = "config"

	rolesConfigMapName = "kipper-users"
	rolesConfigMapKey  = "users"
	adminRole          = "admin"
)

// Reason says why alerts are not leaving, so the advice can match. Telling an
// operator to configure SMTP is wrong when SMTP is already configured and the
// cluster simply has nobody to send to.
type Reason string

const (
	NoChannel    Reason = "no_channel"
	NoRecipients Reason = "no_recipients"
)

// RouteFor reports how alerts leave this cluster.
//
// Slack first, then email, then nowhere, matching what console-api does when it
// delivers one. A cluster that cannot be read reports Nowhere, because an
// unanswerable question is not evidence a channel exists.
func RouteFor(ctx context.Context, client kubernetes.Interface) Route {
	route, err := Lookup(ctx, client)
	if err != nil {
		return Unknown
	}
	return route
}

// Lookup reports the route, and says when it could not find out.
//
// An absent secret is an answer: nothing is configured. A refused read is not,
// and the two must not collapse into one.
func Lookup(ctx context.Context, client kubernetes.Interface) (Route, error) {
	slack, err := slackConfigured(ctx, client)
	if err != nil {
		return Unknown, err
	}
	if slack {
		return Slack, nil
	}

	smtp, err := smtpConfigured(ctx, client)
	if err != nil {
		return Unknown, err
	}
	admins, err := adminAddresses(ctx, client)
	if err != nil {
		return Unknown, err
	}
	// A configured server with no admin to send to delivers nothing, so it is
	// the nowhere case. console-api decides this the same way from its role
	// store; a CLI that disagreed with the console about whether alerts leave
	// would be the false assurance both of them exist to remove.
	if smtp && len(admins) > 0 {
		return Email, nil
	}
	return Nowhere, nil
}

// NowhereReason says why nothing is leaving. It is only meaningful when
// RouteFor returned Nowhere.
func NowhereReason(ctx context.Context, client kubernetes.Interface) Reason {
	if smtp, err := smtpConfigured(ctx, client); err == nil && smtp {
		return NoRecipients
	}
	return NoChannel
}

// adminAddresses reads the cluster admins from the role store console-api
// writes. An unreadable store returns none, which reports nowhere: an
// unanswerable question is not evidence that somebody is being told.
func adminAddresses(ctx context.Context, client kubernetes.Interface) ([]string, error) {
	cm, err := client.CoreV1().ConfigMaps(secretNamespace).Get(ctx, rolesConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the cluster admins: %w", err)
	}

	var roles map[string]string
	if err := json.Unmarshal([]byte(cm.Data[rolesConfigMapKey]), &roles); err != nil {
		// Same: written by console-api, so a broken store is corruption, and a
		// store with no readable admins has nobody to email.
		return nil, nil //nolint:nilerr // an unreadable store is "no admins", not "cannot tell"
	}

	var admins []string
	for email, role := range roles {
		if role == adminRole {
			admins = append(admins, email)
		}
	}
	return admins, nil
}

func slackConfigured(ctx context.Context, client kubernetes.Interface) (bool, error) {
	secret, err := client.CoreV1().Secrets(secretNamespace).Get(ctx, slackSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading the Slack settings: %w", err)
	}
	return len(secret.Data[slackWebhookKey]) > 0, nil
}

func smtpConfigured(ctx context.Context, client kubernetes.Interface) (bool, error) {
	secret, err := client.CoreV1().Secrets(secretNamespace).Get(ctx, smtpSecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading the email settings: %w", err)
	}
	var cfg struct {
		Host string `json:"host"`
	}
	if err := json.Unmarshal(secret.Data[smtpConfigKey], &cfg); err != nil {
		// Stored by console-api, so this is corruption rather than a
		// permissions problem, and an unusable config is not a channel.
		return false, nil //nolint:nilerr // an unreadable config is "not configured", not "cannot tell"
	}
	// A server with no host sends nothing, so a half-filled config is not a
	// channel.
	return cfg.Host != "", nil
}
