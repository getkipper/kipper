package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/installer"
)

func stubEncryptSecrets(t *testing.T, already bool, err error) {
	t.Helper()
	saved := encryptSecrets
	t.Cleanup(func() { encryptSecrets = saved })
	encryptSecrets = func(_ installer.CommandRunner, notify func(string)) (bool, error) {
		notify("Restarting k3s on the new key.")
		return already, err
	}
}

func TestEncryptSecretsSaysWhatStaysUnencrypted(t *testing.T) {
	stubEncryptSecrets(t, false, nil)

	out := captureStdout(t, func() { require.NoError(t, encryptSecretsOn(nil)) })

	assert.Contains(t, out, "...  Restarting k3s on the new key.")
	assert.Contains(t, out, "every existing Secret was rewritten")
	assert.Contains(t, out, "earlier backups keep them unencrypted")
}

func TestEncryptSecretsOnAnEncryptedClusterSaysSo(t *testing.T) {
	stubEncryptSecrets(t, true, nil)

	out := captureStdout(t, func() { require.NoError(t, encryptSecretsOn(nil)) })

	assert.Contains(t, out, "already encrypts every Secret")
	assert.NotContains(t, out, "rewritten")
}

func TestEncryptSecretsReturnsTheFailure(t *testing.T) {
	stubEncryptSecrets(t, false, assert.AnError)

	out := captureStdout(t, func() { assert.ErrorIs(t, encryptSecretsOn(nil), assert.AnError) })

	assert.NotContains(t, out, "✔")
}

func stubSecretsEncrypted(t *testing.T, encrypted bool, err error) {
	t.Helper()
	saved := secretsEncrypted
	t.Cleanup(func() { secretsEncrypted = saved })
	secretsEncrypted = func(installer.CommandRunner) (bool, error) { return encrypted, err }
}

func TestUpgradeNamesTheCommandWhenSecretsAreUnencrypted(t *testing.T) {
	stubSecretsEncrypted(t, false, nil)

	out := captureStdout(t, func() { noteUnencryptedSecrets(nil) })

	assert.Contains(t, out, "kip cluster encrypt-secrets")
}

func TestUpgradeSaysNothingAboutEncryptedSecrets(t *testing.T) {
	stubSecretsEncrypted(t, true, nil)
	assert.Empty(t, captureStdout(t, func() { noteUnencryptedSecrets(nil) }))

	stubSecretsEncrypted(t, false, assert.AnError)
	assert.Empty(t, captureStdout(t, func() { noteUnencryptedSecrets(nil) }), "a k3s that cannot answer is not reported as unencrypted")
}
