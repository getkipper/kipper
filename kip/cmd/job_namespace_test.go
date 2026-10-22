package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
)

func TestJobCommandsUseTheDefaultProjectWhenNoneIsInUse(t *testing.T) {
	assert.Equal(t, "default", jobNamespace(projectFlags(), &config.Cluster{}))
}

func TestJobCommandsUseTheProjectInUse(t *testing.T) {
	assert.Equal(t, "blog-prod", jobNamespace(projectFlags(), &config.Cluster{CurrentProject: "blog", CurrentEnvironment: "prod"}))
}

func TestAJobProjectFlagWins(t *testing.T) {
	cmd := projectFlags()
	require.NoError(t, cmd.Flags().Set("project", "shop"))

	assert.Equal(t, "shop", jobNamespace(cmd, &config.Cluster{CurrentProject: "blog"}))
}
