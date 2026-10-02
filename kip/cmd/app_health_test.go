package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func healthFlagsFrom(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
	addHealthFlags(cmd)
	require.NoError(t, cmd.ParseFlags(args))
	return cmd
}

func TestReadHealthFlags(t *testing.T) {
	t.Run("none given", func(t *testing.T) {
		edit, err := readHealthFlags(healthFlagsFrom(t))
		require.NoError(t, err)
		assert.False(t, edit.Any())
	})
	t.Run("every flag", func(t *testing.T) {
		edit, err := readHealthFlags(healthFlagsFrom(t, "--health", "http", "--health-path", "/ready", "--health-port", "8081",
			"--health-startup-timeout", "600", "--health-timeout", "3"))
		require.NoError(t, err)
		assert.Equal(t, "http", edit.Type)
		assert.Equal(t, "/ready", *edit.Path)
		assert.Equal(t, int32(8081), *edit.Port)
		assert.Equal(t, int32(600), *edit.StartupTimeoutSeconds)
		assert.Equal(t, int32(3), *edit.TimeoutSeconds)
	})
	t.Run("only what was given is set", func(t *testing.T) {
		edit, err := readHealthFlags(healthFlagsFrom(t, "--health-startup-timeout", "900"))
		require.NoError(t, err)
		assert.Empty(t, edit.Type)
		assert.Nil(t, edit.Path)
		assert.Nil(t, edit.Port)
		assert.Equal(t, int32(900), *edit.StartupTimeoutSeconds)
	})
	t.Run("an unknown type", func(t *testing.T) {
		_, err := readHealthFlags(healthFlagsFrom(t, "--health", "grpc"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "http, tcp, none or auto")
	})
	t.Run("auto with settings", func(t *testing.T) {
		_, err := readHealthFlags(healthFlagsFrom(t, "--health", "auto", "--health-port", "8081"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--health auto")
	})
}

func TestAppUpdateAndDeployTakeTheHealthFlags(t *testing.T) {
	for _, cmd := range []*cobra.Command{appUpdateCmd, appDeployCmd} {
		for _, name := range []string{"health", "health-path", "health-port", "health-startup-timeout", "health-timeout"} {
			assert.NotNil(t, cmd.Flags().Lookup(name), "%s --%s", cmd.Name(), name)
		}
	}
}
