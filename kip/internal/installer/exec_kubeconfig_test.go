package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
)

func TestRenderExecKubeconfigCarriesNoCredential(t *testing.T) {
	rendered := renderExecKubeconfig("cluster.example.com", "https://203.0.113.10:6443", []byte("ca-pem"), "kip")

	cfg, err := clientcmd.Load([]byte(rendered))
	require.NoError(t, err, "the rendered kubeconfig must be loadable by client-go")

	cluster := cfg.Clusters["cluster.example.com"]
	require.NotNil(t, cluster)
	assert.Equal(t, "https://203.0.113.10:6443", cluster.Server)
	assert.Equal(t, []byte("ca-pem"), cluster.CertificateAuthorityData)

	user := cfg.AuthInfos["oidc@cluster.example.com"]
	require.NotNil(t, user)
	require.NotNil(t, user.Exec, "the user must be an exec plugin, nothing embedded")
	assert.Equal(t, "kip", user.Exec.Command)
	assert.Equal(t, []string{"auth", "kubectl-token", "--cluster-domain", "cluster.example.com"}, user.Exec.Args,
		"the file must name its own cluster; kubectl tells the plugin nothing about who called it")
	assert.Equal(t, "client.authentication.k8s.io/v1", user.Exec.APIVersion)

	// The whole point: no certificate, no key, no token anywhere.
	assert.Empty(t, user.ClientCertificateData)
	assert.Empty(t, user.ClientKeyData)
	assert.Empty(t, user.Token)
	assert.NotContains(t, rendered, "client-certificate-data")
	assert.NotContains(t, rendered, "client-key-data")
}

func TestRewriteKubeconfigToExec(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home, _ := os.UserHomeDir()
	clustersDir := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clustersDir, 0o700))

	// A k3s-shaped admin kubeconfig: embedded client certificate — the
	// credential this rewrite exists to remove from the machine.
	admin := `apiVersion: v1
kind: Config
clusters:
  - name: default
    cluster:
      server: https://203.0.113.10:6443
      certificate-authority-data: Y2EtcGVt
contexts:
  - name: default
    context: {cluster: default, user: default}
current-context: default
users:
  - name: default
    user:
      client-certificate-data: Y2VydA==
      client-key-data: a2V5
`
	path := filepath.Join(clustersDir, "cluster.example.com.yaml")
	require.NoError(t, os.WriteFile(path, []byte(admin), 0o600))

	got, err := RewriteKubeconfigToExec("cluster.example.com", path)
	require.NoError(t, err)
	assert.Equal(t, path, got)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(content), "client-certificate-data",
		"the admin certificate must be gone from this machine")
	assert.Contains(t, string(content), "kubectl-token")
	assert.Contains(t, string(content), "https://203.0.113.10:6443", "the server address survives")
	assert.Contains(t, string(content), "Y2EtcGVt", "the cluster CA survives")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestRewriteKubeconfigToExecRefusesUnusableSource(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home, _ := os.UserHomeDir()
	clustersDir := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clustersDir, 0o700))

	// No CA to carry over → the rewrite must fail rather than write a
	// kubeconfig that cannot verify the API server.
	require.NoError(t, os.WriteFile(filepath.Join(clustersDir, "bare.yaml"), []byte(strings.TrimSpace(`
apiVersion: v1
kind: Config
clusters:
  - name: default
    cluster:
      server: https://203.0.113.10:6443
`)), 0o600))

	_, err := RewriteKubeconfigToExec("bare", filepath.Join(clustersDir, "bare.yaml"))
	require.Error(t, err)
}

func TestRewriteKubeconfigResolvesActiveContextAmongMany(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home, _ := os.UserHomeDir()
	clustersDir := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clustersDir, 0o700))

	// Two clusters; the active context references the SECOND entry. Keeping
	// the wrong one would destroy the real credential file while preserving
	// a stranger's server and CA.
	merged := `apiVersion: v1
kind: Config
clusters:
  - name: other
    cluster:
      server: https://198.51.100.9:6443
      certificate-authority-data: b3RoZXItY2E=
  - name: mine
    cluster:
      server: https://203.0.113.10:6443
      certificate-authority-data: bXktY2E=
contexts:
  - name: other
    context: {cluster: other, user: other}
  - name: mine
    context: {cluster: mine, user: mine}
current-context: mine
users:
  - name: other
    user: {client-certificate-data: eA==, client-key-data: eQ==}
  - name: mine
    user: {client-certificate-data: eA==, client-key-data: eQ==}
`
	path := filepath.Join(clustersDir, "multi.yaml")
	require.NoError(t, os.WriteFile(path, []byte(merged), 0o600))

	_, err := RewriteKubeconfigToExec("multi", path)
	require.NoError(t, err)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(content), "https://203.0.113.10:6443")
	assert.Contains(t, string(content), "bXktY2E=")
	assert.NotContains(t, string(content), "198.51.100.9")
}

func TestRewriteKubeconfigRefusesAmbiguousContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	home, _ := os.UserHomeDir()
	clustersDir := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clustersDir, 0o700))

	// Multiple contexts and no current-context: refusing beats guessing
	// when the operation destroys the original file.
	ambiguous := `apiVersion: v1
kind: Config
clusters:
  - name: a
    cluster: {server: "https://198.51.100.9:6443", certificate-authority-data: YQ==}
  - name: b
    cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Yg==}
contexts:
  - name: a
    context: {cluster: a, user: a}
  - name: b
    context: {cluster: b, user: b}
`
	path := filepath.Join(clustersDir, "ambiguous.yaml")
	require.NoError(t, os.WriteFile(path, []byte(ambiguous), 0o600))

	_, err := RewriteKubeconfigToExec("ambiguous", path)
	require.Error(t, err)

	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, ambiguous, string(content), "a refused rewrite must leave the file untouched")
}

func TestRenderExecFromAdmin(t *testing.T) {
	// A k3s admin kubeconfig with an embedded client certificate → a
	// credential-free exec kubeconfig, in memory, touching no disk.
	admin := `apiVersion: v1
kind: Config
clusters:
  - name: default
    cluster:
      server: https://203.0.113.10:6443
      certificate-authority-data: Y2EtcGVt
contexts:
  - name: default
    context: {cluster: default, user: default}
current-context: default
users:
  - name: default
    user:
      client-certificate-data: Y2VydA==
      client-key-data: a2V5
`
	content, server, caData, err := RenderExecFromAdmin("cluster.example.com", admin, "kip")
	require.NoError(t, err)
	assert.Equal(t, "https://203.0.113.10:6443", server)
	assert.Equal(t, []byte("ca-pem"), caData)
	assert.NotContains(t, content, "client-certificate-data", "the admin certificate must not survive the conversion")
	assert.Contains(t, content, "kubectl-token")
	assert.Contains(t, content, "Y2EtcGVt", "the cluster CA is carried over")
}

func TestRenderExecFromAdminAbsoluteCommand(t *testing.T) {
	admin := `apiVersion: v1
kind: Config
clusters:
  - name: default
    cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
contexts:
  - name: default
    context: {cluster: default, user: default}
current-context: default
users:
  - name: default
    user: {token: t}
`
	content, _, _, err := RenderExecFromAdmin("c.example.com", admin, "/usr/local/bin/kip")
	require.NoError(t, err)
	assert.Contains(t, content, "command: /usr/local/bin/kip",
		"when kip is off PATH the kubeconfig pins the binary's absolute location")
}

func TestRenderExecFromAdminRejectsNoCA(t *testing.T) {
	admin := `apiVersion: v1
kind: Config
clusters:
  - name: default
    cluster: {server: "https://203.0.113.10:6443"}
contexts:
  - name: default
    context: {cluster: default, user: default}
current-context: default
users:
  - name: default
    user: {token: t}
`
	_, _, _, err := RenderExecFromAdmin("c.example.com", admin, "kip")
	require.Error(t, err)
}

func TestWriteClusterKubeconfigModeAware(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	admin := `apiVersion: v1
kind: Config
clusters:
  - name: default
    cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
contexts:
  - name: default
    context: {cluster: default, user: default}
current-context: default
users:
  - name: default
    user: {client-certificate-data: Y2VydA==, client-key-data: a2V5}
`
	// Exec modes must convert to credential-free and expose server+CA for the
	// gate; the admin certificate must never reach disk.
	for _, mode := range []KubeconfigMode{KubeconfigExecInteractive, KubeconfigExecDeferred} {
		path, server, ca, err := writeClusterKubeconfig("c.example.com", admin, mode)
		require.NoError(t, err)
		assert.Equal(t, "https://203.0.113.10:6443", server)
		assert.NotEmpty(t, ca)
		content, rerr := os.ReadFile(path)
		require.NoError(t, rerr)
		assert.NotContains(t, string(content), "client-certificate-data", "mode %v must not write the admin cert", mode)
		assert.Contains(t, string(content), "kubectl-token")
	}

	// The explicit opt-out writes the admin certificate and exposes no exec
	// server (the gate does not run).
	path, server, ca, err := writeClusterKubeconfig("c.example.com", admin, KubeconfigAdminCert)
	require.NoError(t, err)
	assert.Empty(t, server)
	assert.Empty(t, ca)
	content, rerr := os.ReadFile(path)
	require.NoError(t, rerr)
	assert.Contains(t, string(content), "client-certificate-data", "the opt-out writes the admin certificate")
}

func TestRepinExecKubeconfigFollowsADomainChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.yaml")
	original := renderExecKubeconfig("old.example.com", "https://203.0.113.10:6443", []byte("ca-pem"), "kip")
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))

	repinned, err := RepinExecKubeconfig("new.example.com", path)
	require.NoError(t, err)
	assert.True(t, repinned)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	cfg, err := clientcmd.Load(content)
	require.NoError(t, err)

	user := cfg.AuthInfos["oidc@new.example.com"]
	require.NotNil(t, user, "the user entry follows the domain")
	assert.Equal(t, []string{"auth", "kubectl-token", "--cluster-domain", "new.example.com"}, user.Exec.Args)
	assert.NotContains(t, string(content), "old.example.com", "a stale pin asks for a session that no longer exists")

	cluster := cfg.Clusters["new.example.com"]
	require.NotNil(t, cluster)
	assert.Equal(t, "https://203.0.113.10:6443", cluster.Server, "the address and CA are carried over untouched")
	assert.Equal(t, []byte("ca-pem"), cluster.CertificateAuthorityData)
}

// Converting a certificate-bearing kubeconfig is 'kip auth kubeconfig' to do,
// on request. A domain change doing it as a side effect would take away a
// working credential from someone who never asked.
func TestRepinExecKubeconfigLeavesACertificateKubeconfigAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin.yaml")
	admin := `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://203.0.113.10:6443
    certificate-authority-data: Y2EtcGVt
users:
- name: admin
  user:
    client-certificate-data: Y2VydA==
    client-key-data: a2V5
contexts:
- name: c
  context:
    cluster: c
    user: admin
current-context: c
`
	require.NoError(t, os.WriteFile(path, []byte(admin), 0o600))

	repinned, err := RepinExecKubeconfig("new.example.com", path)
	require.NoError(t, err)
	assert.False(t, repinned, "nothing to re-pin, and nothing to convert")

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, admin, string(content), "the file is left exactly as it was")
}

// A file holding both kinds of user, with the certificate one active. Taking
// any exec entry anywhere as permission to rewrite would discard the
// credential the operator is actually using.
func TestRepinExecKubeconfigJudgesTheActiveCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mixed.yaml")
	mixed := `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
users:
- name: oidc@old.example.com
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: kip
      args: ["auth", "kubectl-token", "--cluster-domain", "old.example.com"]
- name: admin
  user:
    client-certificate-data: Y2VydA==
    client-key-data: a2V5
contexts:
- name: c
  context: {cluster: c, user: admin}
current-context: c
`
	require.NoError(t, os.WriteFile(path, []byte(mixed), 0o600))

	repinned, err := RepinExecKubeconfig("new.example.com", path)
	require.NoError(t, err)
	assert.False(t, repinned, "the active credential is the certificate, so there is no pin to move")

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, mixed, string(content), "the admin certificate must survive a domain change")
}

// A cluster entry may point at a kubeconfig authenticating through another
// tool's exec plugin. kip cannot reissue that credential, so a domain change
// must not replace it.
func TestRepinExecKubeconfigLeavesAnotherToolsPluginAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "eks.yaml")
	foreign := `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
users:
- name: operator
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws
      args: ["eks", "get-token", "--cluster-name", "shop"]
contexts:
- name: c
  context: {cluster: c, user: operator}
current-context: c
`
	require.NoError(t, os.WriteFile(path, []byte(foreign), 0o600))

	repinned, err := RepinExecKubeconfig("new.example.com", path)
	require.NoError(t, err)
	assert.False(t, repinned, "kip did not issue this credential and cannot reissue it")

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, foreign, string(content), "another tool's plugin must survive a domain change")
}

// A foreign plugin that happens to take "kubectl-token" as an argument value.
// Recognition is by the shape kip renders, so this is not kip's.
func TestRepinExecKubeconfigLeavesAForeignPluginBorrowingTheWord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "borrowed.yaml")
	borrowed := `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
users:
- name: operator
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: another-tool
      args: ["credential", "kubectl-token"]
contexts:
- name: c
  context: {cluster: c, user: operator}
current-context: c
`
	require.NoError(t, os.WriteFile(path, []byte(borrowed), 0o600))

	repinned, err := RepinExecKubeconfig("new.example.com", path)
	require.NoError(t, err)
	assert.False(t, repinned)

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, borrowed, string(content))
}

// The absolute-path command an install pins when kip is not on PATH is still
// kip's own plugin, and must keep following a domain change.
func TestRepinExecKubeconfigAcceptsAnAbsolutePinnedCommand(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pinned.yaml")
	pinned := renderExecKubeconfig("old.example.com", "https://203.0.113.10:6443", []byte("ca-pem"), "/usr/local/bin/kip")
	require.NoError(t, os.WriteFile(path, []byte(pinned), 0o600))

	repinned, err := RepinExecKubeconfig("new.example.com", path)
	require.NoError(t, err)
	assert.True(t, repinned, "an absolute path to kip is still kip")
}

// An inferred context still has a name, and an error that omits it sends the
// reader looking through a file for an entry it declined to name.
func TestExecFromAPIConfigNamesAnInferredContext(t *testing.T) {
	inferred := `apiVersion: v1
kind: Config
clusters:
- name: present
  cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
contexts:
- name: only
  context: {cluster: absent, user: operator}
users:
- name: operator
  user: {client-certificate-data: eA==, client-key-data: eQ==}
`
	_, _, _, err := RenderExecFromAdmin("cluster.example.com", inferred, "kip")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"only"`, "the inferred context is named")
	assert.Contains(t, err.Error(), `"absent"`)
}
