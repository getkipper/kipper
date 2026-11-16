package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
)

// stageOrgCluster makes the current cluster one whose projects carry the
// organization prefix acme, with the project blog in use.
func stageOrgCluster(t *testing.T) {
	t.Helper()
	home := withFakeHome(t)
	path := filepath.Join(home, "kubeconfig.yaml")
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
  - name: default
    cluster: {server: "https://203.0.113.10:6443", insecure-skip-tls-verify: true}
contexts:
  - name: default
    context: {cluster: default, user: default}
current-context: default
users:
  - name: default
    user: {token: test-token}
`
	require.NoError(t, os.WriteFile(path, []byte(kubeconfig), 0o600))
	require.NoError(t, config.Save(&config.Config{
		CurrentCluster: "shop",
		Clusters: []config.Cluster{{
			Name: "shop", Host: "203.0.113.10", Domain: "shop.example", Kubeconfig: path,
			Org: "acme", CurrentProject: "blog",
		}},
	}))
}

func namespaceFlags() *cobra.Command {
	cmd := projectFlags()
	cmd.Flags().String("namespace", "", "")
	return cmd
}

// A migration's steps name the namespace the migration keeps on both
// clusters. --project would resolve through this workstation's organization
// prefix and reach another namespace, so --namespace is taken as it is.
func TestAppNamespaceIsTakenExactly(t *testing.T) {
	stageOrgCluster(t)
	cmd := namespaceFlags()
	require.NoError(t, cmd.Flags().Set("namespace", "shop-prod"))

	ns, _, err := resolveAppNamespace(cmd, "web")

	require.NoError(t, err)
	assert.Equal(t, "shop-prod", ns)
}

func TestAppNamespaceRefusesAProjectAsWell(t *testing.T) {
	stageOrgCluster(t)
	for _, flag := range []string{"project", "environment"} {
		cmd := namespaceFlags()
		require.NoError(t, cmd.Flags().Set("namespace", "shop-prod"))
		require.NoError(t, cmd.Flags().Set(flag, "shop"))

		_, _, err := resolveAppNamespace(cmd, "web")

		assert.ErrorContains(t, err, "--namespace", "--namespace with --%s", flag)
	}
}

func TestAppStopStartAndUpdateTakeAnExactNamespace(t *testing.T) {
	for _, cmd := range []*cobra.Command{appStopCmd, appStartCmd, appUpdateCmd} {
		assert.NotNil(t, cmd.Flags().Lookup("namespace"), "kip app %s has no --namespace flag", cmd.Name())
	}
}
