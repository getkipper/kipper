package installer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
