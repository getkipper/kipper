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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Route is where an alert leaves the cluster by.
type Route string

const (
	Slack   Route = "slack"
	Email   Route = "email"
	Nowhere Route = "nowhere"
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
	if slackConfigured(ctx, client) {
		return Slack
	}
	// A configured server with no admin to send to delivers nothing, so it is
	// the nowhere case. console-api decides this the same way from its role
	// store; a CLI that disagreed with the console about whether alerts leave
	// would be the false assurance both of them exist to remove.
	if smtpConfigured(ctx, client) && len(adminAddresses(ctx, client)) > 0 {
		return Email
	}
	return Nowhere
}

// NowhereReason says why nothing is leaving. It is only meaningful when
// RouteFor returned Nowhere.
func NowhereReason(ctx context.Context, client kubernetes.Interface) Reason {
	if smtpConfigured(ctx, client) {
		return NoRecipients
	}
	return NoChannel
}

// adminAddresses reads the cluster admins from the role store console-api
// writes. An unreadable store returns none, which reports nowhere: an
// unanswerable question is not evidence that somebody is being told.
func adminAddresses(ctx context.Context, client kubernetes.Interface) []string {
	cm, err := client.CoreV1().ConfigMaps(secretNamespace).Get(ctx, rolesConfigMapName, metav1.GetOptions{})
	if err != nil {
		return nil
	}

	var roles map[string]string
	if err := json.Unmarshal([]byte(cm.Data[rolesConfigMapKey]), &roles); err != nil {
		return nil
	}

	var admins []string
	for email, role := range roles {
		if role == adminRole {
			admins = append(admins, email)
		}
	}
	return admins
}

func slackConfigured(ctx context.Context, client kubernetes.Interface) bool {
	secret, err := client.CoreV1().Secrets(secretNamespace).Get(ctx, slackSecretName, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return len(secret.Data[slackWebhookKey]) > 0
}

func smtpConfigured(ctx context.Context, client kubernetes.Interface) bool {
	secret, err := client.CoreV1().Secrets(secretNamespace).Get(ctx, smtpSecretName, metav1.GetOptions{})
	if err != nil {
		return false
	}
	var cfg struct {
		Host string `json:"host"`
	}
	if err := json.Unmarshal(secret.Data[smtpConfigKey], &cfg); err != nil {
		return false
	}
	// A server with no host sends nothing, so a half-filled config is not a
	// channel.
	return cfg.Host != ""
}
