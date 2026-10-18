package cmd

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

func lookupTable(table map[string][]net.IP) func(string) ([]net.IP, error) {
	return func(host string) ([]net.IP, error) {
		if ips, ok := table[host]; ok {
			return ips, nil
		}
		return nil, errors.New("no such host")
	}
}

func TestCheckLocalHost(t *testing.T) {
	local := func() ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.5"), net.ParseIP("192.0.2.10")}, nil
	}
	lookup := lookupTable(map[string][]net.IP{
		"localhost":          {net.ParseIP("127.0.0.1")},
		"ip6-localhost":      {net.ParseIP("::1")},
		"server.example.com": {net.ParseIP("192.0.2.10")},
		"nat.example.com":    {net.ParseIP("203.0.113.7")},
	})

	bound, err := checkLocalHost("192.0.2.10", lookup, local)
	require.NoError(t, err)
	assert.True(t, bound)

	bound, err = checkLocalHost("server.example.com", lookup, local)
	require.NoError(t, err)
	assert.True(t, bound)

	bound, err = checkLocalHost("nat.example.com", lookup, local)
	require.NoError(t, err)
	assert.False(t, bound, "a NAT'd public address resolves but is not on this machine")

	for _, host := range []string{"localhost", "ip6-localhost", "127.0.0.1", "::1"} {
		_, err = checkLocalHost(host, lookup, local)
		assert.ErrorContains(t, err, "loopback", host)
	}

	_, err = checkLocalHost("laptop-alias", lookup, local)
	assert.ErrorContains(t, err, "does not resolve on this machine")
}

func TestCheckLocalPrivileges(t *testing.T) {
	assert.NoError(t, checkLocalPrivileges(0, "/root"))
	assert.ErrorContains(t, checkLocalPrivileges(1000, "/home/ops"), "as root")
	assert.ErrorContains(t, checkLocalPrivileges(0, ""), "HOME=/root")
	assert.ErrorContains(t, checkLocalPrivileges(0, "/home/ops"), "HOME=/root")
}

func TestInstallLocalFlags(t *testing.T) {
	for _, name := range []string{"local", "host-maps-here", "result"} {
		assert.NotNil(t, installCmd.Flags().Lookup(name), "--%s", name)
	}
}

const resultKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
users:
- name: operator
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: /root/.kip/jobs/run-1/kip
      args: ["auth", "kubectl-token", "--cluster-domain", "shop.example"]
contexts:
- name: c
  context: {cluster: c, user: operator}
current-context: c
`

func TestInstallResultRoundTripsThroughClusterAdd(t *testing.T) {
	server := withFakeHome(t)
	kubeconfigPath := filepath.Join(server, "shop.yaml")
	require.NoError(t, os.WriteFile(kubeconfigPath, []byte(resultKubeconfig), 0o600))
	require.NoError(t, config.Save(&config.Config{Clusters: []config.Cluster{{
		Name: "shop", Provider: "baremetal", Host: "203.0.113.10", Domain: "shop.example",
		Kubeconfig: kubeconfigPath, Org: "acme", OrgDisplayName: "Acme Inc",
		GatewayToken:   "tok-gateway",
		BackupStorage:  &config.BackupStorageRef{Mode: "external", Bucket: "shop-backups", Region: "eu-west-1", Endpoint: "https://s3.example.com"},
		DNSResolvers:   []string{"192.0.2.53", "198.51.100.53"},
		TrustedProxies: []string{"198.51.100.0/24"},
	}}}))
	resultPath := filepath.Join(t.TempDir(), "result.kip")
	require.NoError(t, writeInstallResult(resultPath, "203.0.113.10"))
	written, err := os.ReadFile(resultPath) //nolint:gosec // inside the test's temp home
	require.NoError(t, err)
	assert.Contains(t, string(written), "kip cluster add --install-result <file>", "the file names the command that imports it whole")
	info, err := os.Stat(resultPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the result carries the gateway token")

	withFakeHome(t)
	require.NoError(t, clusterAddCmd.Flags().Set("install-result", "true"))
	t.Cleanup(func() { _ = clusterAddCmd.Flags().Set("install-result", "false") })
	require.NoError(t, runClusterAdd(clusterAddCmd, []string{resultPath}))

	saved, err := config.Load()
	require.NoError(t, err)
	entry := saved.GetCluster("shop")
	require.NotNil(t, entry)
	assert.Equal(t, "shop.example", entry.Domain)
	assert.Equal(t, "Acme Inc", entry.OrgDisplayName)
	assert.Equal(t, "tok-gateway", entry.GatewayToken)
	assert.Equal(t, &config.BackupStorageRef{Mode: "external", Bucket: "shop-backups", Region: "eu-west-1", Endpoint: "https://s3.example.com"}, entry.BackupStorage)
	assert.Equal(t, []string{"192.0.2.53", "198.51.100.53"}, entry.DNSResolvers)
	assert.Equal(t, []string{"198.51.100.0/24"}, entry.TrustedProxies)
	kubeconfig, err := os.ReadFile(entry.Kubeconfig) //nolint:gosec // inside the test's temp home
	require.NoError(t, err)
	assert.NotContains(t, string(kubeconfig), "/root/.kip/jobs", "the server's binary path never reaches this machine")
}

func TestClusterAddIgnoresResultFieldsInAnExport(t *testing.T) {
	home := withFakeHome(t)
	path := exportBundle(t, home)
	data, err := os.ReadFile(path) //nolint:gosec // test fixture in a temp home
	require.NoError(t, err)
	planted := filepath.Join(home, "planted.kip")
	require.NoError(t, os.WriteFile(planted, append([]byte("gateway_token: tok-planted\nbackup_storage_mode: external\nbackup_storage_bucket: theirs\n"), data...), 0o600)) //nolint:gosec // both paths are inside the test's temp home

	require.NoError(t, runClusterAdd(clusterAddCmd, []string{planted}))

	saved, err := config.Load()
	require.NoError(t, err)
	entry := saved.GetCluster("shop")
	require.NotNil(t, entry)
	assert.Empty(t, entry.GatewayToken)
	assert.Nil(t, entry.BackupStorage)
}

func TestRefuseUninstallingThisMachine(t *testing.T) {
	local := func() ([]net.IP, error) { return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.5")}, nil }
	lookup := lookupTable(map[string][]net.IP{"server.example.com": {net.ParseIP("10.0.0.5")}})
	thisMachine := func() (string, error) { return "0123456789abcdef0123456789abcdef", nil }

	refuse := func(c *config.Cluster) error { return refuseUninstallingThisMachine(c, thisMachine, lookup, local) }

	natted := &config.Cluster{Host: "203.0.113.7", MachineID: "0123456789abcdef0123456789abcdef"}
	assert.ErrorContains(t, refuse(natted), "from another machine", "a NAT'd --local install is still this machine")
	assert.ErrorContains(t, refuse(&config.Cluster{Host: "10.0.0.5"}), "from another machine", "an older entry without a machine id falls back to the address")
	assert.ErrorContains(t, refuse(&config.Cluster{Host: "server.example.com"}), "from another machine")
	assert.NoError(t, refuse(&config.Cluster{Host: "203.0.113.7", MachineID: "fedcba9876543210fedcba9876543210"}))
	assert.NoError(t, refuse(&config.Cluster{Host: "gone.example.com"}))
}

func TestClusterAddResultHeaderAloneChangesNothing(t *testing.T) {
	home := withFakeHome(t)
	require.NoError(t, config.Save(&config.Config{Clusters: []config.Cluster{{
		Name: "shop", Host: "203.0.113.10", Domain: "shop.example", GatewayToken: "tok-mine",
		BackupStorage: &config.BackupStorageRef{Mode: "external", Bucket: "mine"},
	}}}))
	path := exportBundle(t, home)
	data, err := os.ReadFile(path) //nolint:gosec // test fixture in a temp home
	require.NoError(t, err)
	crafted := filepath.Join(home, "crafted.kip")
	require.NoError(t, os.WriteFile(crafted, append([]byte(installResultHeader+"gateway_token: tok-theirs\nbackup_storage_mode: external\nbackup_storage_bucket: theirs\n"), data...), 0o600)) //nolint:gosec // inside the test's temp home

	assert.ErrorContains(t, runClusterAdd(clusterAddCmd, []string{crafted}), "--install-result", "a result is never imported as an export")
	entry := loadShop(t)
	assert.Equal(t, "tok-mine", entry.GatewayToken)
	assert.Equal(t, "mine", entry.BackupStorage.Bucket)

	require.NoError(t, clusterAddCmd.Flags().Set("install-result", "true"))
	t.Cleanup(func() { _ = clusterAddCmd.Flags().Set("install-result", "false") })
	assert.ErrorContains(t, runClusterAdd(clusterAddCmd, []string{crafted}), "already has a cluster")
	assert.Equal(t, "tok-mine", loadShop(t).GatewayToken)
}

func TestClusterAddInstallResultRejectsInvalidSettings(t *testing.T) {
	for _, line := range []string{
		"backup_storage_mode: somewhere",
		"backup_storage_endpoint: ftp://s3.example.com",
		"dns_resolver: not-an-ip",
		"trusted_proxy: 10.0.0.0/99",
		"gateway_token: tok with spaces",
	} {
		t.Run(line, func(t *testing.T) {
			home := withFakeHome(t)
			path := exportBundle(t, home)
			data, err := os.ReadFile(path) //nolint:gosec // test fixture in a temp home
			require.NoError(t, err)
			bad := filepath.Join(home, "bad.kip")
			require.NoError(t, os.WriteFile(bad, append([]byte(installResultHeader+line+"\n"), data...), 0o600)) //nolint:gosec // inside the test's temp home
			require.NoError(t, clusterAddCmd.Flags().Set("install-result", "true"))
			t.Cleanup(func() { _ = clusterAddCmd.Flags().Set("install-result", "false") })

			assert.Error(t, runClusterAdd(clusterAddCmd, []string{bad}))
		})
	}
}

func loadShop(t *testing.T) *config.Cluster {
	t.Helper()
	saved, err := config.Load()
	require.NoError(t, err)
	entry := saved.GetCluster("shop")
	require.NotNil(t, entry)
	return entry
}

func TestClusterAddRefusedResultLeavesTheKubeconfigAlone(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))
	existing := filepath.Join(clusters, "shop.yaml")
	// Use a replaceable kubeconfig with a distinct address to detect an overwrite.
	own, err := installer.RenderImportedKubeconfig("shop.example", strings.Replace(resultKubeconfig, "203.0.113.10", "203.0.113.99", 1))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(existing, []byte(own), 0o600))
	require.NoError(t, config.Save(&config.Config{Clusters: []config.Cluster{{Name: "shop", Host: "203.0.113.10", Domain: "shop.example", Kubeconfig: existing}}}))
	path := exportBundle(t, home)
	data, err := os.ReadFile(path) //nolint:gosec // test fixture in a temp home
	require.NoError(t, err)
	result := filepath.Join(home, "result.kip")
	require.NoError(t, os.WriteFile(result, append([]byte(installResultHeader), data...), 0o600)) //nolint:gosec // inside the test's temp home
	require.NoError(t, clusterAddCmd.Flags().Set("install-result", "true"))
	t.Cleanup(func() { _ = clusterAddCmd.Flags().Set("install-result", "false") })

	require.Error(t, runClusterAdd(clusterAddCmd, []string{result}))

	got, err := os.ReadFile(existing) //nolint:gosec // inside the test's temp home
	require.NoError(t, err)
	assert.Equal(t, own, string(got))
}

func TestClusterAddRecognisesAResultWithWindowsLineEndings(t *testing.T) {
	server := withFakeHome(t)
	kubeconfigPath := filepath.Join(server, "shop.yaml")
	require.NoError(t, os.WriteFile(kubeconfigPath, []byte(resultKubeconfig), 0o600))
	require.NoError(t, config.Save(&config.Config{Clusters: []config.Cluster{{
		Name: "shop", Provider: "baremetal", Host: "203.0.113.10", Domain: "shop.example",
		Kubeconfig: kubeconfigPath, GatewayToken: "tok-gateway",
		BackupStorage: &config.BackupStorageRef{Mode: "external", Bucket: "shop-backups", Region: "eu-west-1"},
	}}}))
	lf := filepath.Join(t.TempDir(), "result.kip")
	require.NoError(t, writeInstallResult(lf, "203.0.113.10"))
	data, err := os.ReadFile(lf) //nolint:gosec // inside the test's temp home
	require.NoError(t, err)
	crlf := filepath.Join(t.TempDir(), "result-crlf.kip")
	require.NoError(t, os.WriteFile(crlf, []byte(strings.ReplaceAll(string(data), "\n", "\r\n")), 0o600)) //nolint:gosec // inside the test's temp home

	withFakeHome(t)
	assert.ErrorContains(t, runClusterAdd(clusterAddCmd, []string{crlf}), "--install-result")

	require.NoError(t, clusterAddCmd.Flags().Set("install-result", "true"))
	t.Cleanup(func() { _ = clusterAddCmd.Flags().Set("install-result", "false") })
	require.NoError(t, runClusterAdd(clusterAddCmd, []string{crlf}))
	entry := loadShop(t)
	assert.Equal(t, "tok-gateway", entry.GatewayToken)
	require.NotNil(t, entry.BackupStorage)
	assert.Equal(t, "shop-backups", entry.BackupStorage.Bucket)
}

func TestClusterAddRecordsTheSSHUser(t *testing.T) {
	home := withFakeHome(t)
	path := exportBundle(t, home)
	require.NoError(t, clusterAddCmd.Flags().Set("ssh-user", "ubuntu"))
	t.Cleanup(func() { _ = clusterAddCmd.Flags().Set("ssh-user", "") })

	require.NoError(t, runClusterAdd(clusterAddCmd, []string{path}))

	assert.Equal(t, "ubuntu", loadShop(t).SSHUser)
}

func TestClusterAddRecordsTheSSHKey(t *testing.T) {
	home := withFakeHome(t)
	path := exportBundle(t, home)
	require.NoError(t, clusterAddCmd.Flags().Set("ssh-key", "/keys/shop"))
	t.Cleanup(func() { _ = clusterAddCmd.Flags().Set("ssh-key", "") })

	require.NoError(t, runClusterAdd(clusterAddCmd, []string{path}))

	assert.Equal(t, "/keys/shop", loadShop(t).SSHKey)
}
