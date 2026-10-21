package logicalns

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// Create provisions a PostgreSQL or MySQL database or a RabbitMQ vhost through
// pod exec, using the service's shared credentials. Existing namespaces are reused,
// and MySQL and RabbitMQ permissions are reapplied. For MongoDB, it only validates
// the name, and the database is created on first write.
func Create(ctx context.Context, client kubernetes.Interface, restConfig *rest.Config, serviceName, namespace, svcType, value string, creds *corev1.Secret) error {
	if svcType == "mongodb" {
		if !ValidName(value) {
			return fmt.Errorf("invalid namespace name %q", value)
		}
		return nil
	}
	if restConfig == nil {
		return fmt.Errorf("REST config not available for pod exec")
	}

	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", serviceName),
	})
	if err != nil || len(pods.Items) == 0 {
		return fmt.Errorf("no running pod found for service %q", serviceName)
	}
	podName := pods.Items[0].Name

	cmd, err := Cmd(svcType, value, creds)
	if err != nil {
		return err
	}

	req := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: serviceName,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("provisioning namespace: %s: %w", stderr.String(), err)
	}

	return nil
}

// Cmd builds a provisioning command for PostgreSQL, MySQL, or RabbitMQ. Values
// are passed as shell positional arguments. Name validation also protects the
// SQL literals and identifiers that use them. creds must be non-nil and contain
// a valid USERNAME. Invalid names, usernames and unsupported service types return
// an error. PostgreSQL uses creds' NAME or defaults to app. MySQL reads the root
// password from the pod's MYSQL_ROOT_PASSWORD environment variable.
func Cmd(svcType, value string, creds *corev1.Secret) ([]string, error) {
	if !ValidName(value) {
		return nil, fmt.Errorf("invalid namespace name %q", value)
	}
	username := string(creds.Data["USERNAME"])
	if !validUsername(username) {
		return nil, fmt.Errorf("invalid service username %q: use 1–32 ASCII letters, digits, underscores, dashes, or dots", username)
	}
	switch svcType {
	case "postgres":
		// Connect to the service's existing database to create the binding's database.
		defaultDB := string(creds.Data["NAME"])
		if defaultDB == "" {
			defaultDB = "app"
		}
		// $1 = namespace value, $2 = username, $3 = default DB.
		return []string{
			"sh", "-c",
			`psql -U "$2" -d "$3" -tc "SELECT 1 FROM pg_database WHERE datname='$1'" | grep -q 1 || psql -U "$2" -d "$3" -c "CREATE DATABASE \"$1\""`,
			"--", value, username, defaultDB,
		}, nil
	case "mysql":
		// Use root to create the database and grant the service user access.
		// mysql reads the root password from MYSQL_PWD, which keeps it out
		// of the process's arguments.
		return []string{
			"sh", "-c",
			// Backticks inside double quotes are command substitution to the
			// shell, so the MySQL identifier quotes are escaped.
			`MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot -e "CREATE DATABASE IF NOT EXISTS \` + "`" + `$1\` + "`" + `; GRANT ALL PRIVILEGES ON \` + "`" + `$1\` + "`" + `.* TO \` + "`" + `$2\` + "`" + `@'%'"`,
			"--", value, username,
		}, nil
	case "rabbitmq":
		// Reuse an existing vhost and reapply the service user's permissions.
		return []string{
			"sh", "-c",
			`rabbitmqctl list_vhosts -q | grep -Fxq -- "$1" || rabbitmqctl add_vhost -- "$1" && rabbitmqctl set_permissions -p "$1" "$2" ".*" ".*" ".*"`,
			"--", value, username,
		}, nil
	}
	return nil, fmt.Errorf("logical-namespace provisioning not supported for service type %q", svcType)
}

// ValidName accepts 1–63 ASCII letters, digits, underscores, dashes, dots, or
// slashes. This restricts names used in SQL while allowing vhost paths such as /orders.
func ValidName(value string) bool {
	if value == "" || len(value) > 63 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == '/':
		default:
			return false
		}
	}
	return true
}

// validUsername restricts usernames embedded in MySQL account identifiers.
func validUsername(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return false
		}
	}
	return true
}
