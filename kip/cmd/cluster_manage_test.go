package cmd

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/kip/internal/clusteridentity"
	"github.com/getkipper/kipper/kip/internal/config"
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
	assert.Contains(t, string(content), "--cluster-domain\n          - new.example.com")

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
