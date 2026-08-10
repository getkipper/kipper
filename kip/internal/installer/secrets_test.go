package installer

import (
	"strings"
	"testing"
)

func TestApplySecretCmdKeepsValueOffCommandLine(t *testing.T) {
	cmd := applySecretCmd("dex", dexOIDCClientSecretName, "secret")
	// The value is fed via stdin, so it must not appear as a kubectl argument.
	if strings.Contains(cmd, "--from-literal") {
		t.Error("secret value must not be passed as a --from-literal argument")
	}
	for _, want := range []string{
		"-n dex create secret generic dex-oidc-client",
		"--from-file=secret=/dev/stdin",
		"--dry-run=client -o yaml | kubectl apply -f -",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("applySecretCmd missing %q in:\n%s", want, cmd)
		}
	}
}

func TestApplySecretWithLiteralCmdKeepsSecretOffCommandLine(t *testing.T) {
	cmd := applySecretWithLiteralCmd("monitoring", "grafana-admin", "admin-user", "admin", "admin-password")
	// The username is a non-secret literal; the password is fed via stdin, so
	// only the username may appear as a --from-literal argument.
	if strings.Contains(cmd, "--from-literal=admin-password") {
		t.Error("the password must be fed via stdin, never as a --from-literal argument")
	}
	for _, want := range []string{
		"-n monitoring create secret generic grafana-admin",
		"--from-literal=admin-user=admin",
		"--from-file=admin-password=/dev/stdin",
		"--dry-run=client -o yaml | kubectl apply -f -",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("applySecretWithLiteralCmd missing %q in:\n%s", want, cmd)
		}
	}
}

func TestReadSecretCmdDistinguishesAbsentFromError(t *testing.T) {
	cmd := readSecretCmd("kipper-system", dexOIDCClientSecretName, "secret")
	// --ignore-not-found makes an absent Secret succeed with empty output; a real
	// failure must still exit non-zero, so the command must not swallow errors.
	if strings.Contains(cmd, "|| true") || strings.Contains(cmd, "2>/dev/null") {
		t.Errorf("readSecretCmd must not mask read failures:\n%s", cmd)
	}
	for _, want := range []string{
		"get secret dex-oidc-client --ignore-not-found",
		"jsonpath='{.data.secret}'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("readSecretCmd missing %q in:\n%s", want, cmd)
		}
	}
}

func TestEnsureNamespaceCmd(t *testing.T) {
	cmd := ensureNamespaceCmd("dex")
	if !strings.Contains(cmd, "create namespace dex --dry-run=client -o yaml | kubectl apply -f -") {
		t.Errorf("ensureNamespaceCmd unexpected: %s", cmd)
	}
}
