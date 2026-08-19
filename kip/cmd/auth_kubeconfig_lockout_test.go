package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

// kipExecKubeconfig is what this command writes and what an imported cluster
// already carries: no credential at all, only kip's own plugin. Replacing it
// costs nothing, because kip can render it again from the same inputs.
const kipExecKubeconfig = `apiVersion: v1
kind: Config
clusters:
  - name: default
    cluster:
      server: https://203.0.113.10:6443
      certificate-authority-data: Y2EtcGVt
contexts:
  - name: default
    context: {cluster: default, user: oidc@shop.example}
current-context: default
users:
  - name: oidc@shop.example
    user:
      exec:
        apiVersion: client.authentication.k8s.io/v1
        command: kip
        args: [auth, kubectl-token, --cluster-domain, shop.example]
        interactiveMode: Never
`

// foreignExecKubeconfig carries another tool's credential plugin. kip never
// issued it and cannot put it back, so it is as irreplaceable as a certificate
// even though no secret is written in the file.
const foreignExecKubeconfig = `apiVersion: v1
kind: Config
clusters:
  - name: default
    cluster:
      server: https://203.0.113.10:6443
      certificate-authority-data: Y2EtcGVt
contexts:
  - name: default
    context: {cluster: default, user: cloud}
current-context: default
users:
  - name: cloud
    user:
      exec:
        apiVersion: client.authentication.k8s.io/v1
        command: cloud-auth
        args: [get-token, --cluster, shop]
`

// kubeconfigWithAParkedCertificate has a credential-free active context and a
// certificate parked in a context nobody is using. The rewrite replaces the
// whole file, so that certificate is lost just as completely as one in front.
const kubeconfigWithAParkedCertificate = `apiVersion: v1
kind: Config
clusters:
  - name: default
    cluster:
      server: https://203.0.113.10:6443
      certificate-authority-data: Y2EtcGVt
contexts:
  - name: default
    context: {cluster: default, user: oidc@shop.example}
  - name: breakglass
    context: {cluster: default, user: admin}
current-context: default
users:
  - name: oidc@shop.example
    user:
      exec:
        apiVersion: client.authentication.k8s.io/v1
        command: kip
        args: [auth, kubectl-token, --cluster-domain, shop.example]
        interactiveMode: Never
  - name: admin
    user:
      client-certificate-data: Y2VydA==
      client-key-data: a2V5
`

// stageClusterWith writes an arbitrary kubeconfig for the current cluster.
func stageClusterWith(t *testing.T, kubeconfig string) string {
	t.Helper()
	home := withFakeHome(t)

	clustersDir := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clustersDir, 0o700))
	path := filepath.Join(clustersDir, "shop.example.yaml")
	require.NoError(t, os.WriteFile(path, []byte(kubeconfig), 0o600))

	require.NoError(t, config.Save(&config.Config{
		CurrentCluster: "shop",
		Clusters: []config.Cluster{
			{Name: "shop", Host: "203.0.113.10", Domain: "shop.example", Kubeconfig: path},
		},
	}))
	return path
}

// The lockout. ProofPassNonAdmin means the API server authenticated the
// operator and then denied the access review — it proves authentication and no
// authorization whatsoever. Trading a working certificate for it leaves the
// machine with an identity that can do nothing, and no kip command can reissue
// what was given up.
func TestAuthKubeconfigRefusesWhenTheReplacementProvesNoAccess(t *testing.T) {
	path := stageClusterWithAdminCert(t)
	withProof(t, installer.ProofPassNonAdmin, "authenticated as oidc:sam@shop.example, which is not cluster-admin", nil)

	err := runKubeconfigCommand(t)

	require.Error(t, err)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, adminKubeconfig, string(content),
		"a login that proves no access must not cost the credential that has it")
}

// The legitimate non-admin: a deployer who imported a credential-free bundle
// has nothing to lose, so their conversion is not gated on cluster-admin. This
// is why the rule keys on what is destroyed rather than on who is asking.
func TestAuthKubeconfigConvertsANonAdminWhoHoldsNoCredential(t *testing.T) {
	path := stageClusterWith(t, kipExecKubeconfig)
	calls := withProof(t, installer.ProofPassNonAdmin, "authenticated as oidc:sam@shop.example, which is not cluster-admin", nil)

	require.NoError(t, runKubeconfigCommand(t))

	assert.Equal(t, 1, *calls, "the proof still runs; it just cannot cost anything here")
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(content), "kubectl-token")
}

// Another tool's plugin is not a credential in the file, and is still access
// this machine cannot get back.
func TestAuthKubeconfigKeepsAForeignCredentialPlugin(t *testing.T) {
	path := stageClusterWith(t, foreignExecKubeconfig)
	withProof(t, installer.ProofPassNonAdmin, "authenticated as oidc:sam@shop.example, which is not cluster-admin", nil)

	err := runKubeconfigCommand(t)

	require.Error(t, err)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, foreignExecKubeconfig, string(content),
		"a plugin kip did not write cannot be re-rendered, so it counts as a credential")
}

// Scanning only the active context misses a certificate parked in another one,
// which the rewrite deletes just the same.
func TestAuthKubeconfigKeepsACredentialParkedInAnotherContext(t *testing.T) {
	path := stageClusterWith(t, kubeconfigWithAParkedCertificate)
	withProof(t, installer.ProofPassNonAdmin, "authenticated as oidc:sam@shop.example, which is not cluster-admin", nil)

	err := runKubeconfigCommand(t)

	require.Error(t, err)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, kubeconfigWithAParkedCertificate, string(content),
		"the whole file is replaced, so every context's credential is at stake")
}

// A proven cluster-admin converts whatever the file holds: that is the case
// the command exists for.
func TestAuthKubeconfigConvertsACertificateOnAProvenAdminLogin(t *testing.T) {
	path := stageClusterWithAdminCert(t)
	withProof(t, installer.ProofPass, "oidc:admin@shop.example", nil)

	require.NoError(t, runKubeconfigCommand(t))

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(content), "kubectl-token")
	assert.NotContains(t, string(content), "client-certificate-data")
}

// The refusal has to name the way out, because the operator reading it is
// already stuck and the escape hatch is a filesystem move rather than a flag.
func TestAuthKubeconfigRefusalNamesTheWayOut(t *testing.T) {
	stageClusterWithAdminCert(t)
	withProof(t, installer.ProofPassNonAdmin, "authenticated as oidc:sam@shop.example, which is not cluster-admin", nil)

	err := runKubeconfigCommand(t)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kubeconfig unchanged")
}

// impersonatingKubeconfig carries kip's own plugin and assumes another
// identity. No credential is written in the file, and the identity in those
// fields may be the only one with access, so the rewrite that drops them costs
// exactly what was working.
const impersonatingKubeconfig = `apiVersion: v1
kind: Config
clusters:
  - name: default
    cluster:
      server: https://203.0.113.10:6443
      certificate-authority-data: Y2EtcGVt
contexts:
  - name: default
    context: {cluster: default, user: oidc@shop.example}
current-context: default
users:
  - name: oidc@shop.example
    user:
      as: ops-runner
      as-groups: [platform]
      exec:
        apiVersion: client.authentication.k8s.io/v1
        command: kip
        args: [auth, kubectl-token, --cluster-domain, shop.example]
        interactiveMode: Never
`

func TestAuthKubeconfigKeepsAnImpersonatedIdentity(t *testing.T) {
	path := stageClusterWith(t, impersonatingKubeconfig)
	withProof(t, installer.ProofPassNonAdmin, "authenticated as oidc:sam@shop.example, which is not cluster-admin", nil)

	err := runKubeconfigCommand(t)

	require.Error(t, err)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, impersonatingKubeconfig, string(content),
		"the rewrite keeps no impersonation settings, so they are access it would destroy")
}

// runKubeconfigTo drives the command with --output, which is the route for an
// operator whose access is namespaced: a cluster-wide check cannot see it, so
// they get a file to try rather than a refusal or a destroyed credential.
func runKubeconfigTo(t *testing.T, output string) error {
	t.Helper()
	cmd := &cobra.Command{RunE: runAuthKubeconfig}
	cmd.Flags().String("output", "", "")
	require.NoError(t, cmd.Flags().Parse([]string{"--output", output}))
	var err error
	captureStdout(t, func() { err = runAuthKubeconfig(cmd, nil) })
	return err
}

// A project member holds namespaced access and a credential in their file. The
// cluster-wide review denies them, so the conversion in place is refused, but
// writing elsewhere destroys nothing and goes ahead.
func TestAuthKubeconfigWritesElsewhereForANamespacedOperator(t *testing.T) {
	original := stageClusterWithAdminCert(t)
	withProof(t, installer.ProofPassNonAdmin, "authenticated as oidc:sam@shop.example, which is not cluster-admin", nil)
	output := filepath.Join(t.TempDir(), "kip-oidc.yaml")

	require.NoError(t, runKubeconfigTo(t, output))

	written, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Contains(t, string(written), "kubectl-token")

	kept, err := os.ReadFile(original)
	require.NoError(t, err)
	assert.Equal(t, adminKubeconfig, string(kept), "writing elsewhere gives nothing up")
}

// Writing elsewhere is only safe because nothing is replaced, so a path that
// already holds something is refused rather than overwritten.
func TestAuthKubeconfigRefusesToOverwriteTheOutputPath(t *testing.T) {
	stageClusterWithAdminCert(t)
	withProof(t, installer.ProofPassNonAdmin, "authenticated as oidc:sam@shop.example, which is not cluster-admin", nil)
	output := filepath.Join(t.TempDir(), "occupied.yaml")
	require.NoError(t, os.WriteFile(output, []byte("something already here\n"), 0o600))

	err := runKubeconfigTo(t, output)

	require.Error(t, err)
	content, readErr := os.ReadFile(output)
	require.NoError(t, readErr)
	assert.Equal(t, "something already here\n", string(content))
}

// sharedKubeconfig is the shape most operators actually have: several clusters
// in one file. The rewrite renders a fresh single-context file over the whole
// path, so converting one context here would delete the others.
const sharedKubeconfig = `apiVersion: v1
kind: Config
clusters:
  - name: shop
    cluster:
      server: https://203.0.113.10:6443
      certificate-authority-data: Y2EtcGVt
  - name: corporate
    cluster:
      server: https://203.0.113.99:6443
      certificate-authority-data: Y2EtcGVt
contexts:
  - name: shop
    context: {cluster: shop, user: shop-admin}
  - name: corporate
    context: {cluster: corporate, user: corporate-sso}
current-context: shop
users:
  - name: shop-admin
    user:
      client-certificate-data: Y2VydA==
      client-key-data: a2V5
  - name: corporate-sso
    user:
      exec:
        apiVersion: client.authentication.k8s.io/v1
        command: corporate-auth
        args: [token]
`

// Even a proven cluster-admin must not lose the rest of their file. The trade
// this command offers is the active context's credential; every other cluster
// in the file is collateral nobody asked to give up, and it went silently.
func TestAuthKubeconfigRefusesToWipeOtherClustersEvenForAnAdmin(t *testing.T) {
	path := stageClusterWith(t, sharedKubeconfig)
	withProof(t, installer.ProofPass, "oidc:admin@shop.example", nil)

	err := runKubeconfigCommand(t)

	require.Error(t, err)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, sharedKubeconfig, string(content),
		"a proven login is permission to replace this cluster's credential, not every other one in the file")
	assert.Contains(t, err.Error(), "other clusters")
}

// And the way through is the same one: write elsewhere, give nothing up.
func TestAuthKubeconfigWritesASharedFileElsewhere(t *testing.T) {
	original := stageClusterWith(t, sharedKubeconfig)
	withProof(t, installer.ProofPass, "oidc:admin@shop.example", nil)
	output := filepath.Join(t.TempDir(), "kip-oidc.yaml")

	require.NoError(t, runKubeconfigTo(t, output))

	kept, err := os.ReadFile(original)
	require.NoError(t, err)
	assert.Equal(t, sharedKubeconfig, string(kept))
	written, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Contains(t, string(written), "kubectl-token")
}

// A file holding only this cluster is the ordinary case, and a proven admin
// converts it in place as before.
func TestAuthKubeconfigStillConvertsASingleClusterFileInPlace(t *testing.T) {
	path := stageClusterWithAdminCert(t)
	withProof(t, installer.ProofPass, "oidc:admin@shop.example", nil)

	require.NoError(t, runKubeconfigCommand(t))

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(content), "kubectl-token")
}

// replaceableMultiClusterKubeconfig holds two clusters, both reached through
// kip's own plugin, so nothing in it is irreplaceable. The rewrite still keeps
// one cluster and discards the other, and a context an operator has to rebuild
// by hand was destroyed by a command they ran for a different cluster.
const replaceableMultiClusterKubeconfig = `apiVersion: v1
kind: Config
clusters:
  - name: shop
    cluster:
      server: https://203.0.113.10:6443
      certificate-authority-data: Y2EtcGVt
  - name: warehouse
    cluster:
      server: https://203.0.113.20:6443
      certificate-authority-data: Y2EtcGVt
contexts:
  - name: shop
    context: {cluster: shop, user: oidc@shop.example}
  - name: warehouse
    context: {cluster: warehouse, user: oidc@warehouse.example}
current-context: shop
users:
  - name: oidc@shop.example
    user:
      exec:
        apiVersion: client.authentication.k8s.io/v1
        command: kip
        args: [auth, kubectl-token, --cluster-domain, shop.example]
        interactiveMode: Never
  - name: oidc@warehouse.example
    user:
      exec:
        apiVersion: client.authentication.k8s.io/v1
        command: kip
        args: [auth, kubectl-token, --cluster-domain, warehouse.example]
        interactiveMode: Never
`

func TestAuthKubeconfigRefusesToDiscardOtherClustersEvenWhenReplaceable(t *testing.T) {
	path := stageClusterWith(t, replaceableMultiClusterKubeconfig)
	withProof(t, installer.ProofPass, "oidc:admin@shop.example", nil)

	err := runKubeconfigCommand(t)

	require.Error(t, err)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, replaceableMultiClusterKubeconfig, string(content),
		"the rewrite keeps one cluster, so every other one in the file is destroyed")
	assert.Contains(t, err.Error(), "other clusters")
}

// A proven admin converting in place must not lose a break-glass certificate
// parked in an unused entry of the same cluster. Checking only for *other
// clusters* misses it, because there is only one cluster in this file.
func TestAuthKubeconfigKeepsAParkedCertificateForAProvenAdmin(t *testing.T) {
	path := stageClusterWith(t, kubeconfigWithAParkedCertificate)
	withProof(t, installer.ProofPass, "oidc:admin@shop.example", nil)

	err := runKubeconfigCommand(t)

	require.Error(t, err)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, kubeconfigWithAParkedCertificate, string(content),
		"a proven login is permission to replace the active credential, not one parked beside it")
}
