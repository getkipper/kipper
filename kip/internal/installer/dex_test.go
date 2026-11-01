package installer

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func TestGenerateSecret(t *testing.T) {
	secret1, err := generateSecret(32)
	require.NoError(t, err)
	assert.Len(t, secret1, 64) // 32 bytes = 64 hex chars

	secret2, err := generateSecret(32)
	require.NoError(t, err)

	assert.NotEqual(t, secret1, secret2, "consecutive secrets should be unique")
}

func TestGenerateSecretLength(t *testing.T) {
	tests := []struct {
		bytes  int
		hexLen int
	}{
		{16, 32},
		{32, 64},
		{64, 128},
	}

	for _, tt := range tests {
		secret, err := generateSecret(tt.bytes)
		require.NoError(t, err)
		assert.Len(t, secret, tt.hexLen)
	}
}

func TestDexManifestSetsExplicitTokenLifetimes(t *testing.T) {
	// The expiry block is the platform's session policy; a Dex default
	// slipping back in silently would change session and token exposure
	// windows without anyone deciding it.
	assert.Contains(t, dexManifestTemplate, "expiry:")
	assert.Contains(t, dexManifestTemplate, `idTokens: "15m"`,
		"ID tokens are cluster credentials once the authenticator lands; their lifetime is the whole revocation story")
	assert.Contains(t, dexManifestTemplate, `authRequests: "10m"`)
	assert.Contains(t, dexManifestTemplate, `reuseInterval: "30s"`)
	assert.Contains(t, dexManifestTemplate, `validIfNotUsedFor: "168h"`)
	assert.Contains(t, dexManifestTemplate, `absoluteLifetime: "720h"`)
	assert.NotContains(t, dexManifestTemplate, "disableRotation",
		"refresh rotation must stay enabled; the CLI's store lock and reuseInterval exist to make rotation safe, not to replace it")
}

func TestDexImageIsDigestPinned(t *testing.T) {
	// Dex signs the identities the platform trusts; a mutable tag would let
	// a re-pushed image replace the identity provider without any change
	// here.
	assert.Regexp(t, `image: ghcr\.io/dexidp/dex:v[0-9.]+@sha256:[0-9a-f]{64}`, dexManifestTemplate)
}

// failingRunner fails commands containing failOn.
type failingRunner struct{ failOn string }

func (f failingRunner) Run(cmd string) (string, error) {
	if strings.Contains(cmd, f.failOn) {
		return "", errors.New("refused")
	}
	return "", nil
}

func (f failingRunner) RunStdin(cmd string, in io.Reader) (string, error) {
	b, _ := io.ReadAll(in)
	return f.Run(cmd + "\n" + string(b))
}

func TestInstallDexKeepsThePasswordHashOutOfCommandsAndErrors(t *testing.T) {
	const hash = "$2a$10$exampleexampleexampleexampleexampleexampleexample12"
	host := &dexApplyFails{failure: fmt.Errorf("running %q: %w: staticPasswords: %s", "kubectl apply -f -", errors.New("exit status 1"), hash)}

	_, err := InstallDex(host, "dex.example.com", "console.example.com", "example.com", hash)

	require.Error(t, err)
	assert.Contains(t, host.stdin, hash, "the manifest still carries the hash")
	for _, c := range host.commands {
		assert.NotContains(t, c, hash)
	}
	assert.NotContains(t, err.Error(), hash)
}

// dexApplyFails fails only "kubectl apply -f -" with staticPasswords in stdin,
// allowing prerequisite Secret writes to succeed.
type dexApplyFails struct {
	failure  error
	commands []string
	stdin    string
}

func (d *dexApplyFails) Run(cmd string) (string, error) {
	d.commands = append(d.commands, cmd)
	return "", nil
}

func (d *dexApplyFails) RunStdin(cmd string, in io.Reader) (string, error) {
	b, _ := io.ReadAll(in)
	d.commands = append(d.commands, cmd)
	if cmd == "kubectl apply -f -" && strings.Contains(string(b), "staticPasswords") {
		d.stdin = string(b)
		return "", d.failure
	}
	return "", nil
}

// A failed apply may have committed the password, so the caller must receive it.
func TestInstallDexReportsWhetherThePasswordWasSent(t *testing.T) {
	sent, err := InstallDex(failingRunner{failOn: "create namespace"}, "dex.example.com", "console.example.com", "example.com", "hash")
	require.Error(t, err)
	assert.False(t, sent, "a prerequisite failed before the password manifest")

	sent, err = InstallDex(failingRunner{failOn: "staticPasswords"}, "dex.example.com", "console.example.com", "example.com", "hash")
	require.Error(t, err)
	assert.True(t, sent, "an apply that reports failure may still have committed")
}

// The console stores SSO client secrets in dex-connector-secrets, and the
// connector config refers to them by environment variable.
func TestDexLoadsTheConnectorSecretsIntoItsEnvironment(t *testing.T) {
	manifest := renderDexManifest("dex.example.com", "console.example.com", "example.com", "$2a$10$hash")
	var deploy appsv1.Deployment
	for _, doc := range strings.Split(manifest, "\n---\n") {
		if strings.Contains(doc, "kind: Deployment") {
			require.NoError(t, yaml.Unmarshal([]byte(doc), &deploy))
		}
	}
	require.Len(t, deploy.Spec.Template.Spec.Containers, 1)
	envFrom := deploy.Spec.Template.Spec.Containers[0].EnvFrom
	require.Len(t, envFrom, 1)
	require.NotNil(t, envFrom[0].SecretRef)
	assert.Equal(t, "dex-connector-secrets", envFrom[0].SecretRef.Name)
	require.NotNil(t, envFrom[0].SecretRef.Optional)
	assert.True(t, *envFrom[0].SecretRef.Optional, "Dex starts before the first connector exists")
}
