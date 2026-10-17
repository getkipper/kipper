package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInstallProfileFlagRefusesUnknownNames(t *testing.T) {
	assert.NoError(t, checkInstallProfile(""))
	for _, p := range []string{"nano", "small", "medium", "large", "xlarge"} {
		assert.NoError(t, checkInstallProfile(p), p)
	}
	err := checkInstallProfile("huge")
	assert.ErrorContains(t, err, `unknown profile "huge"`)
	assert.ErrorContains(t, err, "nano, small, medium, large, xlarge")
}

func TestDrivenRequestCarriesTheProfile(t *testing.T) {
	cmd := installCmd
	require := assert.New(t)
	require.NoError(cmd.Flags().Set("profile", "small"))
	t.Cleanup(func() { _ = cmd.Flags().Set("profile", "") })
	req, err := drivenRequest(cmd, "203.0.113.10")
	require.NoError(err)
	require.Equal("small", req.Profile)
}
