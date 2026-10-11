package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
)

func projectFlags() *cobra.Command {
	cmd := &cobra.Command{Use: "x"}
	cmd.Flags().String("project", "", "")
	cmd.Flags().String("environment", "", "")
	return cmd
}

func TestAppCommandsActOnTheProjectInUse(t *testing.T) {
	cluster := &config.Cluster{CurrentProject: "blog", CurrentEnvironment: "prod"}

	assert.Equal(t, "blog-prod", selectedNamespace(projectFlags(), cluster))
}

func TestAnExplicitProjectWinsOverTheOneInUse(t *testing.T) {
	cluster := &config.Cluster{CurrentProject: "blog", CurrentEnvironment: "prod"}
	cmd := projectFlags()
	require.NoError(t, cmd.Flags().Set("project", "shop"))
	require.NoError(t, cmd.Flags().Set("environment", "test"))

	assert.Equal(t, "shop-test", selectedNamespace(cmd, cluster))
}

func TestNoProjectInUseSelectsNoNamespace(t *testing.T) {
	assert.Empty(t, selectedNamespace(projectFlags(), &config.Cluster{}), "the app is then found by name across namespaces")
}
