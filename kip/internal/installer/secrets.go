package installer

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// gatewayCredentialsSecretName holds the kipper.run gateway registration token
// cluster-side, so label changes (register/deregister) no longer depend on the
// operator's ~/.kip/config.yaml, which keeps a read-only mirror for recovery.
const gatewayCredentialsSecretName = "gateway-credentials"

// ReadGatewayCredentials returns the gateway management token the cluster
// holds, or empty when it holds none.
//
// The token is read from the cluster rather than from ~/.kip/config.yaml
// because the local mirror is not reliably present: a cluster installed before
// the mirror existed, or by another operator, has none, and releasing a label
// without it is impossible — the gateway keys deregistration entirely on the
// token. Reading it here is what lets an uninstall hand the label back.
func ReadGatewayCredentials(client commandRunner) (string, error) {
	out, err := client.Run(fmt.Sprintf(
		"kubectl -n kipper-system get secret %s -o jsonpath='{.data.token}' --ignore-not-found",
		gatewayCredentialsSecretName))
	if err != nil {
		return "", fmt.Errorf("reading gateway credentials: %w", err)
	}
	// Whitespace comes off first: kubectl's jsonpath output is quoted and
	// newline-terminated ("'abc'\n"), so trimming quotes first leaves the
	// trailing quote stranded behind the newline and the decode fails.
	encoded := strings.Trim(strings.TrimSpace(out), "'")
	if encoded == "" {
		return "", nil
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decoding gateway credentials: %w", err)
	}
	return strings.TrimSpace(string(decoded)), nil
}

// StoreGatewayCredentials writes the gateway registration token into the
// kipper-system/gateway-credentials Secret. A custom-domain cluster has no
// registration, so an empty token is a no-op.
func StoreGatewayCredentials(client *ssh.Client, token string) error {
	if token == "" {
		return nil
	}
	if _, err := client.Run(ensureNamespaceCmd("kipper-system")); err != nil {
		return fmt.Errorf("ensuring kipper-system namespace: %w", err)
	}
	if _, err := client.RunStdin(applySecretCmd("kipper-system", gatewayCredentialsSecretName, "token"), strings.NewReader(token)); err != nil {
		return fmt.Errorf("storing gateway credentials: %w", err)
	}
	return nil
}

// ensureNamespaceCmd applies a namespace idempotently.
func ensureNamespaceCmd(namespace string) string {
	return fmt.Sprintf("kubectl create namespace %s --dry-run=client -o yaml | kubectl apply -f -", namespace)
}

// applySecretCmd upserts a single-key generic Secret. The value is read from the
// remote command's stdin (fed by RunStdin), so it never appears in the command
// string, the process table, or an error message. Server-side apply makes a
// re-run with the same value a no-op.
func applySecretCmd(namespace, name, key string) string {
	return fmt.Sprintf(
		"kubectl -n %s create secret generic %s --from-file=%s=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -",
		namespace, name, key)
}

// applySecretWithLiteralCmd upserts a two-key generic Secret: literalKey is set
// to a non-secret literal value passed on the command line, while secretKey is
// read from stdin (fed by RunStdin) so the secret value never appears in the
// command string or the process table. Server-side apply makes a re-run with
// the same values a no-op.
func applySecretWithLiteralCmd(namespace, name, literalKey, literalValue, secretKey string) string {
	return fmt.Sprintf(
		"kubectl -n %s create secret generic %s --from-literal=%s=%s --from-file=%s=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -",
		namespace, name, literalKey, literalValue, secretKey)
}

// readSecretCmd reads one key of a Secret as its raw base64 value.
// --ignore-not-found makes a genuinely absent Secret yield empty output with a
// zero exit, while any other failure (API unavailable, RBAC) exits non-zero so
// the caller can tell "absent" from "could not read" and never rotates a live
// secret on an unreliable read.
func readSecretCmd(namespace, name, key string) string {
	return fmt.Sprintf(
		"kubectl -n %s get secret %s --ignore-not-found -o jsonpath='{.data.%s}'",
		namespace, name, key)
}

// readSecretValue returns the decoded value of one Secret key, or "" when the
// Secret is genuinely absent. Any other failure is an error, so callers can
// tell "absent" from "could not read" and never mint a fresh secret over a
// live one.
func readSecretValue(client *ssh.Client, namespace, name, key string) (string, error) {
	out, err := client.Run(readSecretCmd(namespace, name, key))
	if err != nil {
		return "", fmt.Errorf("reading secret %s/%s: %w", namespace, name, err)
	}
	encoded := strings.TrimSpace(out)
	if encoded == "" {
		return "", nil
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decoding secret %s/%s: %w", namespace, name, err)
	}
	return string(decoded), nil
}
