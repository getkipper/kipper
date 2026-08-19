package cmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

// adminKubeconfig is the k3s-shaped file kip auth kubeconfig replaces: the
// embedded certificate is the only credential that reaches the cluster until
// an operator login is proven to work.
const adminKubeconfig = `apiVersion: v1
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

// stageClusterWithAdminCert writes a cluster whose kubeconfig carries the admin
// certificate, and returns the path to it.
func stageClusterWithAdminCert(t *testing.T) string {
	t.Helper()
	home := withFakeHome(t)

	clustersDir := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clustersDir, 0o700))
	path := filepath.Join(clustersDir, "shop.example.yaml")
	require.NoError(t, os.WriteFile(path, []byte(adminKubeconfig), 0o600))

	require.NoError(t, config.Save(&config.Config{
		CurrentCluster: "shop",
		Clusters: []config.Cluster{
			{Name: "shop", Host: "203.0.113.10", Domain: "shop.example", Kubeconfig: path},
		},
	}))
	return path
}

// withProof swaps in a proof outcome for the duration of one test.
func withProof(t *testing.T, result installer.ProofResult, detail string, err error) *int {
	t.Helper()
	calls := 0
	original := proveOperatorLogin
	proveOperatorLogin = func(context.Context, *config.Cluster, string, []byte) (installer.ProofResult, string, error) {
		calls++
		return result, detail, err
	}
	t.Cleanup(func() { proveOperatorLogin = original })
	return &calls
}

func runKubeconfigCommand(t *testing.T) error {
	t.Helper()
	_, err := runKubeconfigCommandOutput(t)
	return err
}

func runKubeconfigCommandOutput(t *testing.T) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() { err = runAuthKubeconfig(authKubeconfigCmd, nil) })
	return out, err
}

// The defect this pins: the command gave up the certificate on a cluster whose
// API server has no authenticator at all, leaving nothing that could reach it.
func TestAuthKubeconfigKeepsTheCertificateWhenTheAPIServerRejectsTheLogin(t *testing.T) {
	path := stageClusterWithAdminCert(t)
	withProof(t, installer.ProofAuthnRejected, "the API server rejected the token", nil)

	err := runKubeconfigCommand(t)

	require.Error(t, err)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, adminKubeconfig, string(content),
		"the credential that still works must survive a login that does not")
}

func TestAuthKubeconfigKeepsTheCertificateWhenTheProofCannotComplete(t *testing.T) {
	path := stageClusterWithAdminCert(t)
	withProof(t, installer.ProofTransportError, "connection refused", nil)

	err := runKubeconfigCommand(t)

	require.Error(t, err)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, adminKubeconfig, string(content),
		"an unreachable API server answers nothing, which is not the same as answering yes")
}

func TestAuthKubeconfigKeepsTheCertificateWhenAuthorizationIsDenied(t *testing.T) {
	path := stageClusterWithAdminCert(t)
	withProof(t, installer.ProofAuthzDeniedAsAdmin, "the kipper-initial-admin binding is not effective", nil)

	err := runKubeconfigCommand(t)

	require.Error(t, err)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, adminKubeconfig, string(content))
}

// Running this before 'kip auth login' is ordinary sequencing, and the proof
// answers it with an error naming that command. The operator has to see that
// error rather than a generic refusal, and keep the credential they arrived with.
func TestAuthKubeconfigPassesThroughWhyTheProofCouldNotBeMade(t *testing.T) {
	path := stageClusterWithAdminCert(t)
	withProof(t, installer.ProofAuthnRejected, "", assert.AnError)

	err := runKubeconfigCommand(t)

	require.ErrorIs(t, err, assert.AnError)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, adminKubeconfig, string(content))
}

func TestAuthKubeconfigConvertsWhenTheLoginIsProven(t *testing.T) {
	path := stageClusterWithAdminCert(t)
	calls := withProof(t, installer.ProofPass, "oidc:anna@shop.example", nil)

	out, err := runKubeconfigCommandOutput(t)
	require.NoError(t, err)

	assert.Equal(t, 1, *calls, "the conversion must be gated on a proof, not merely accompanied by one")
	assert.Contains(t, out, "Checking that your login reaches this cluster")
	assert.Contains(t, out, path+" now authenticates as your OIDC identity")
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(content), "client-certificate-data",
		"a proven login means the certificate can leave this machine")
	assert.Contains(t, string(content), "kubectl-token")
	assert.Contains(t, string(content), "https://203.0.113.10:6443", "the server address survives")
}

// This assertion is inverted from what it once made. It read "an operator who
// is not cluster-admin still holds a working login, and the kubeconfig they
// get is the one their permissions allow", and it staged the admin certificate
// to prove it — so it pinned the command giving up that certificate for an
// identity with no proven access at all.
//
// ProofPassNonAdmin is returned when the API server authenticated the operator
// and *denied* the access review, so "not cluster-admin" and "can do nothing"
// are the same answer here. The conversion is now judged by what replacing the
// file destroys, which is the rule the import guard already used.
//
// The non-admin who legitimately converts is the one holding no credential;
// that case lives in auth_kubeconfig_lockout_test.go alongside this one's
// replacement.
func TestAuthKubeconfigKeepsTheCertificateWhenTheLoginProvesNoAccess(t *testing.T) {
	path := stageClusterWithAdminCert(t)
	calls := withProof(t, installer.ProofPassNonAdmin, "authenticated as oidc:sam@shop.example, which is not cluster-admin", nil)

	err := runKubeconfigCommand(t)

	require.Error(t, err)
	assert.Equal(t, 1, *calls, "the proof still runs; its answer is what changed")
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, adminKubeconfig, string(content))
}

// The command tree itself. Everything above calls the handler directly and
// would keep passing with the command re-pointed at an ungated one.
func TestAuthKubeconfigCommandRunsTheGatedHandler(t *testing.T) {
	path := stageClusterWithAdminCert(t)
	withProof(t, installer.ProofAuthnRejected, "the API server rejected the token", nil)

	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{"auth", "kubeconfig"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	var runErr error
	captureStdout(t, func() { runErr = rootCmd.Execute() })

	require.Error(t, runErr)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, adminKubeconfig, string(content))
}

// The proof has to be made against the endpoint the rewrite will keep. Proving
// one server and rewriting another would pass a cluster that was never asked.
func TestAuthKubeconfigProvesAgainstTheEndpointItKeeps(t *testing.T) {
	stageClusterWithAdminCert(t)

	var gotServer string
	var gotCA []byte
	original := proveOperatorLogin
	proveOperatorLogin = func(_ context.Context, _ *config.Cluster, server string, caData []byte) (installer.ProofResult, string, error) {
		gotServer, gotCA = server, caData
		return installer.ProofPass, "oidc:anna@shop.example", nil
	}
	t.Cleanup(func() { proveOperatorLogin = original })

	require.NoError(t, runKubeconfigCommand(t))

	assert.Equal(t, "https://203.0.113.10:6443", gotServer)
	assert.Equal(t, []byte("ca-pem"), gotCA)
}

// The proof takes up to a minute, and another process can replace the file in
// that time. Converting afterwards would hand the exec plugin to a cluster the
// proof never reached, and destroy whatever credential the new file carried.
func TestAuthKubeconfigLeavesAFileThatChangedDuringTheProof(t *testing.T) {
	path := stageClusterWithAdminCert(t)

	// A second cluster's kubeconfig, written while the proof for the first is
	// still in flight.
	substituted := strings.Replace(adminKubeconfig, "203.0.113.10", "198.51.100.7", 1)
	original := proveOperatorLogin
	proveOperatorLogin = func(context.Context, *config.Cluster, string, []byte) (installer.ProofResult, string, error) {
		require.NoError(t, os.WriteFile(path, []byte(substituted), 0o600))
		return installer.ProofPass, "oidc:anna@shop.example", nil
	}
	t.Cleanup(func() { proveOperatorLogin = original })

	err := runKubeconfigCommand(t)

	require.Error(t, err)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, substituted, string(content),
		"the credential that arrived mid-check was never proven and must survive")
}

func TestReadClusterEndpointReadsTheActiveContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shop.example.yaml")
	require.NoError(t, os.WriteFile(path, []byte(adminKubeconfig), 0o600))

	endpoint, err := readClusterEndpoint(path)
	require.NoError(t, err)
	assert.Equal(t, "https://203.0.113.10:6443", endpoint.server)
	assert.Equal(t, []byte("ca-pem"), endpoint.caData)
	assert.Equal(t, adminKubeconfig, string(endpoint.snapshot),
		"the bytes the answer came from travel with it, so the rewrite can act on the same read")
}

// A file naming no current context but holding exactly one is unambiguous, and
// the rewrite already reads it that way. Both have to agree on which entry is
// live or the proof attests to a cluster the rewrite does not keep.
func TestReadClusterEndpointInfersTheOnlyContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shop.example.yaml")
	single := `apiVersion: v1
kind: Config
clusters:
  - name: only
    cluster:
      server: https://203.0.113.20:6443
      certificate-authority-data: Y2EtcGVt
contexts:
  - name: only
    context: {cluster: only, user: only}
users:
  - name: only
    user:
      client-certificate-data: Y2VydA==
`
	require.NoError(t, os.WriteFile(path, []byte(single), 0o600))

	endpoint, err := readClusterEndpoint(path)
	require.NoError(t, err)
	assert.Equal(t, "https://203.0.113.20:6443", endpoint.server)
}

func TestReadClusterEndpointRefusesAFileWithNothingToVerifyAgainst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.yaml")
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600))

	_, err := readClusterEndpoint(path)
	assert.Error(t, err)
}
