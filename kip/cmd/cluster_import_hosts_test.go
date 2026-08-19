package cmd

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
)

// bundleWithHosts writes an export bundle carrying whatever host lines are
// given, so a test can stage a legacy bundle, a complete one, or a truncated
// one and drive the real importer over it.
func bundleWithHosts(t *testing.T, home, domain, hostLines string) string {
	t.Helper()
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
users:
- name: operator
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: kip
      args: ["auth", "kubectl-token", "--cluster-domain", "` + domain + `"]
contexts:
- name: c
  context: {cluster: c, user: operator}
current-context: c
`
	bundle := "name: shop\nprovider: baremetal\nhost: 203.0.113.10\ndomain: " + domain + "\n" + hostLines + "kubeconfig: |\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(kubeconfig))
	for i := 0; i < len(encoded); i += 76 {
		end := min(i+76, len(encoded))
		bundle += "  " + encoded[i:end] + "\n"
	}
	path := filepath.Join(home, "shop-export.yaml")
	require.NoError(t, os.WriteFile(path, []byte(bundle), 0o600))
	return path
}

func importedCluster(t *testing.T) *config.Cluster {
	t.Helper()
	cfg, err := config.Load()
	require.NoError(t, err)
	entry := cfg.GetCluster("shop")
	require.NotNil(t, entry)
	return entry
}

// The round trip that matters. A cluster whose domain is already a subdomain
// derives every service host wrong when the bundle does not carry them:
// dex.console.example.com for dex.example.com. Neither half proves this on its
// own — the exporter looks correct writing hosts nobody reads, and the
// importer looks correct deriving from a domain nobody disputed.
func TestImportingACustomHostedClusterKeepsItsRealHosts(t *testing.T) {
	home := withFakeHome(t)
	path := bundleWithHosts(t, home, "console.example.com",
		"console_domain: console.example.com\nconsole_api_domain: console-api.example.com\ndex_domain: dex.example.com\n")

	require.NoError(t, runClusterAdd(clusterAddCmd, []string{path}))

	entry := importedCluster(t)
	assert.Equal(t, "dex.example.com", entry.DexHost(), "the login host is where the certificate is")
	assert.Equal(t, "console.example.com", entry.ConsoleHost())
	assert.Equal(t, "console-api.example.com", entry.ConsoleAPIHost())
}

// The same bundle without the host lines is what a kip built before this
// change produces, and it still has to import.
func TestImportingALegacyBundleStillDerivesTheHosts(t *testing.T) {
	home := withFakeHome(t)
	path := bundleWithHosts(t, home, "example.com", "")

	require.NoError(t, runClusterAdd(clusterAddCmd, []string{path}))

	entry := importedCluster(t)
	assert.Equal(t, "dex.example.com", entry.DexHost())
	assert.Empty(t, entry.DexDomain, "nothing was carried, so nothing is pinned")
}

// A legacy bundle re-imported over an entry that already has good overrides
// must not clear them. Silence about the hosts is not an instruction to forget
// them, and treating it as one recreates the original defect on a machine that
// was working.
func TestReimportingALegacyBundleKeepsExistingHostOverrides(t *testing.T) {
	home := withFakeHome(t)
	complete := bundleWithHosts(t, home, "console.example.com",
		"console_domain: console.example.com\nconsole_api_domain: console-api.example.com\ndex_domain: dex.example.com\n")
	require.NoError(t, runClusterAdd(clusterAddCmd, []string{complete}))

	legacy := bundleWithHosts(t, home, "console.example.com", "")
	require.NoError(t, runClusterAdd(clusterAddCmd, []string{legacy}))

	assert.Equal(t, "dex.example.com", importedCluster(t).DexHost(),
		"a bundle that says nothing about hosts must not erase the ones that work")
}

// A fresh export of a cluster that has moved back to conventional hosts has to
// clear the stale overrides, which is the other half of the presence rule.
func TestReimportingAConventionalBundleClearsStaleOverrides(t *testing.T) {
	home := withFakeHome(t)
	custom := bundleWithHosts(t, home, "example.com",
		"console_domain: console.old.example.com\nconsole_api_domain: console-api.old.example.com\ndex_domain: dex.old.example.com\n")
	require.NoError(t, runClusterAdd(clusterAddCmd, []string{custom}))
	require.Equal(t, "dex.old.example.com", importedCluster(t).DexHost())

	conventional := bundleWithHosts(t, home, "example.com",
		"console_domain: console.example.com\nconsole_api_domain: console-api.example.com\ndex_domain: dex.example.com\n")
	require.NoError(t, runClusterAdd(clusterAddCmd, []string{conventional}))

	entry := importedCluster(t)
	assert.Equal(t, "dex.example.com", entry.DexHost())
	assert.Empty(t, entry.DexDomain, "a conventional host is stored as no override, so it tracks a later domain change")
}

// One or two host keys means the file was truncated or edited. Applying it
// would write empty overrides for the missing ones and derive the wrong hosts
// for exactly those services.
func TestImportRefusesABundleCarryingSomeHostsButNotAll(t *testing.T) {
	home := withFakeHome(t)
	path := bundleWithHosts(t, home, "console.example.com", "dex_domain: dex.example.com\n")

	err := runClusterAdd(clusterAddCmd, []string{path})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "incomplete")
}

// The export refuses to send userinfo in a server or proxy URL, so a bundle
// that arrives carrying one was written by something else. Preserving it for
// reachability without checking would store somebody's password on this
// machine and reach the address it names.
func TestImportRefusesABundleWhoseServerURLCarriesACredential(t *testing.T) {
	home := withFakeHome(t)
	//nolint:gosec // G101: the point of this fixture is a URL carrying userinfo,
	// which is what the import must refuse. The value is invented.
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://operator:hunter2@203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
users:
- name: operator
  user: {}
contexts:
- name: c
  context: {cluster: c, user: operator}
current-context: c
`
	bundle := "name: shop\nprovider: baremetal\nhost: 203.0.113.10\ndomain: example.com\nkubeconfig: |\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(kubeconfig))
	for i := 0; i < len(encoded); i += 76 {
		end := min(i+76, len(encoded))
		bundle += "  " + encoded[i:end] + "\n"
	}
	path := filepath.Join(home, "shop-export.yaml")
	require.NoError(t, os.WriteFile(path, []byte(bundle), 0o600))

	err := runClusterAdd(clusterAddCmd, []string{path})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "server address")
}

// The damaged bundle the key count exists to catch, driven through the real
// parser. A host line whose value is gone carries no ": " delimiter, so a
// count that only runs on parsed key/value pairs never sees it — and the file
// reads as one written before hosts were carried.
func TestImportRefusesABundleWhoseHostLinesLostTheirValues(t *testing.T) {
	home := withFakeHome(t)
	path := bundleWithHosts(t, home, "console.example.com",
		"console_domain:\nconsole_api_domain:\ndex_domain:\n")

	err := runClusterAdd(clusterAddCmd, []string{path})

	require.Error(t, err, "a damaged bundle was read as one that predates the host keys")
	assert.Contains(t, err.Error(), "incomplete")
}

// And one genuinely written before the keys existed still imports.
func TestImportStillAcceptsABundleWithNoHostLinesAtAll(t *testing.T) {
	home := withFakeHome(t)
	path := bundleWithHosts(t, home, "example.com", "")

	require.NoError(t, runClusterAdd(clusterAddCmd, []string{path}))
}

// The natural mistake: send ~/.kip/clusters/<name>.yaml, because it is the
// file that looks like the cluster config. It is not what registers a cluster,
// and "missing name or host" sends the recipient looking for a field rather
// than for the right file.
func TestImportRecognisesABareKubeconfig(t *testing.T) {
	home := withFakeHome(t)
	path := filepath.Join(home, "labb.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
contexts:
- name: c
  context: {cluster: c, user: operator}
current-context: c
users:
- name: operator
  user: {}
`), 0o600))

	err := runClusterAdd(clusterAddCmd, []string{path})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kubeconfig")
	assert.Contains(t, err.Error(), "kip cluster export", "the remedy runs on the other machine, so name it")
}
