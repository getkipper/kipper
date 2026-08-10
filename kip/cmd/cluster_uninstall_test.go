package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/config"
)

func TestConfirmDestroy_MatchingNameAccepts(t *testing.T) {
	r := strings.NewReader("storefront\n")
	assert.True(t, confirmDestroy(r, "storefront"))
}

func TestConfirmDestroy_MismatchRejects(t *testing.T) {
	r := strings.NewReader("storefront-typo\n")
	assert.False(t, confirmDestroy(r, "storefront"))
}

func TestConfirmDestroy_WhitespaceTrimmed(t *testing.T) {
	r := strings.NewReader("   storefront   \n")
	assert.True(t, confirmDestroy(r, "storefront"))
}

func TestConfirmDestroy_EmptyInputRejects(t *testing.T) {
	r := strings.NewReader("\n")
	assert.False(t, confirmDestroy(r, "storefront"))
}

func TestConfirmDestroy_EOFRejects(t *testing.T) {
	r := strings.NewReader("")
	assert.False(t, confirmDestroy(r, "storefront"))
}

func TestRemoveLocalClusterEntry_DropsClusterAndDeletesKubeconfig(t *testing.T) {
	home := withFakeHome(t)
	clusters := filepath.Join(home, ".kip", "clusters")
	if err := os.MkdirAll(clusters, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	kubeconfig := filepath.Join(clusters, "storefront.yaml")
	if err := os.WriteFile(kubeconfig, []byte("kubeconfig data"), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	cfg := &config.Config{
		CurrentCluster: "storefront",
		Clusters: []config.Cluster{
			{Name: "storefront", Host: "1.2.3.4", Kubeconfig: kubeconfig},
			{Name: "other", Host: "5.6.7.8", Kubeconfig: "/elsewhere/other.yaml"},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := removeLocalClusterEntry("storefront", ""); err != nil {
		t.Fatalf("remove: %v", err)
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	assert.Len(t, got.Clusters, 1)
	assert.Equal(t, "other", got.Clusters[0].Name)
	assert.Equal(t, "other", got.CurrentCluster, "current should fall back to remaining cluster")
	_, statErr := os.Stat(kubeconfig)
	assert.True(t, os.IsNotExist(statErr), "kubeconfig file should have been deleted")
}

func TestRemoveLocalClusterEntry_CurrentBecomesEmptyWhenLastClusterRemoved(t *testing.T) {
	withFakeHome(t)
	cfg := &config.Config{
		CurrentCluster: "only",
		Clusters: []config.Cluster{
			{Name: "only", Host: "1.2.3.4", Kubeconfig: scratchKubeconfig(t)},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := removeLocalClusterEntry("only", ""); err != nil {
		t.Fatalf("remove: %v", err)
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	assert.Empty(t, got.Clusters)
	assert.Equal(t, "", got.CurrentCluster)
}

func TestRemoveLocalClusterEntry_UnknownClusterIsNoOp(t *testing.T) {
	withFakeHome(t)
	cfg := &config.Config{
		CurrentCluster: "stay",
		Clusters: []config.Cluster{
			{Name: "stay", Host: "1.2.3.4", Kubeconfig: scratchKubeconfig(t)},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := removeLocalClusterEntry("never-existed", ""); err != nil {
		t.Fatalf("remove: %v", err)
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	assert.Len(t, got.Clusters, 1)
	assert.Equal(t, "stay", got.CurrentCluster)
}

func TestRemoveLocalClusterEntry_CurrentReassignedWhenRemovingCurrent(t *testing.T) {
	withFakeHome(t)
	cfg := &config.Config{
		CurrentCluster: "first",
		Clusters: []config.Cluster{
			{Name: "first", Host: "203.0.113.1", Kubeconfig: scratchKubeconfig(t)},
			{Name: "second", Host: "203.0.113.2", Kubeconfig: scratchKubeconfig(t)},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := removeLocalClusterEntry("first", ""); err != nil {
		t.Fatalf("remove: %v", err)
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	assert.Equal(t, "second", got.CurrentCluster)
}

// scratchKubeconfig returns a throwaway kubeconfig path. removeLocalClusterEntry
// deletes the file it is pointed at, so these tests must never name a real one:
// /dev/null used to stand in here, and deleting the device node broke every
// later compile in the same container.
func scratchKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig.yaml")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatalf("write scratch kubeconfig: %v", err)
	}
	return path
}

// The removal is refused when the entry no longer holds the credential the
// caller acted on. Another kip run moving a domain during a wipe leaves a live
// registration under the same name, and deleting it discards the only local copy
// of a credential nobody agreed to give up.
func TestRemoveLocalClusterEntryRefusesAnEntryThatChanged(t *testing.T) {
	withFakeHome(t)

	if err := config.Update(func(cfg *config.Config) error {
		cfg.AddCluster(config.Cluster{
			Name: "storefront", Host: "203.0.113.10",
			Domain: "storefront.kipper.run", GatewayToken: "tok-someone-elses",
		})
		return nil
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	err := removeLocalClusterEntry("storefront", "tok-this-run-spent")
	if !errors.Is(err, ErrEntryChanged) {
		t.Fatalf("err = %v, want ErrEntryChanged", err)
	}

	cfg, _ := config.Load()
	if cfg.GetCluster("storefront") == nil {
		t.Error("deleted an entry holding a credential this run never acted on")
	}
}

// And it goes ahead when the entry still holds what was spent.
func TestRemoveLocalClusterEntryRemovesTheEntryItActedOn(t *testing.T) {
	withFakeHome(t)

	if err := config.Update(func(cfg *config.Config) error {
		cfg.AddCluster(config.Cluster{
			Name: "storefront", Host: "203.0.113.10",
			Domain: "storefront.kipper.run", GatewayToken: "tok-1",
		})
		return nil
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if err := removeLocalClusterEntry("storefront", "tok-1"); err != nil {
		t.Fatalf("removeLocalClusterEntry: %v", err)
	}
	cfg, _ := config.Load()
	if cfg.GetCluster("storefront") != nil {
		t.Error("kept an entry whose credential this run spent")
	}
}

// The bypass the guard used to have. A wipe the operator consented to without a
// readable credential spends nothing, and runs for minutes; another command
// registering a name into that entry meanwhile leaves a live token in it. Empty
// matching anything would delete it.
func TestRemoveLocalClusterEntryRefusesAnEntryThatGainedACredential(t *testing.T) {
	withFakeHome(t)

	if err := config.Update(func(cfg *config.Config) error {
		cfg.AddCluster(config.Cluster{
			Name: "storefront", Host: "203.0.113.10",
			Domain: "storefront.kipper.run", GatewayToken: "tok-registered-meanwhile",
		})
		return nil
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	err := removeLocalClusterEntry("storefront", "")
	if !errors.Is(err, ErrEntryChanged) {
		t.Fatalf("err = %v, want ErrEntryChanged", err)
	}
	cfg, _ := config.Load()
	if cfg.GetCluster("storefront") == nil {
		t.Error("deleted an entry that gained a credential while this run was working")
	}
}

// A cluster that never registered a name holds nothing and is spent nothing, and
// must still be removable.
func TestRemoveLocalClusterEntryRemovesAClusterThatNeverHeldACredential(t *testing.T) {
	withFakeHome(t)

	if err := config.Update(func(cfg *config.Config) error {
		cfg.AddCluster(config.Cluster{Name: "shop", Host: "203.0.113.10", Domain: "shop.example.com"})
		return nil
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if err := removeLocalClusterEntry("shop", ""); err != nil {
		t.Fatalf("a custom-domain cluster registered nothing and must still go: %v", err)
	}
	cfg, _ := config.Load()
	if cfg.GetCluster("shop") != nil {
		t.Error("kept an entry with nothing to protect")
	}
}

// The retry write expects an empty entry, and refuses one that has gained a
// credential. The uninstall tests use a double for this, and a double proves
// only that it agrees with itself — this drives the real writer against a real
// config file.
func TestMirrorGatewayTokenRefusesToOverwriteWhatItDidNotExpect(t *testing.T) {
	withFakeHome(t)

	if err := config.Update(func(cfg *config.Config) error {
		cfg.AddCluster(config.Cluster{
			Name: "storefront", Host: "203.0.113.10",
			Domain: "storefront.kipper.run", GatewayToken: "tok-live",
		})
		return nil
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	err := mirrorGatewayTokenToConfig("storefront", "", "tok-refused")
	if !errors.Is(err, ErrMirrorHolds) {
		t.Fatalf("err = %v, want ErrMirrorHolds", err)
	}
	cfg, _ := config.Load()
	if got := cfg.GetCluster("storefront").GatewayToken; got != "tok-live" {
		t.Errorf("token = %q — overwrote the only local copy of a live credential", got)
	}
}

func TestMirrorGatewayTokenWritesIntoTheEntryItExpected(t *testing.T) {
	withFakeHome(t)

	if err := config.Update(func(cfg *config.Config) error {
		cfg.AddCluster(config.Cluster{Name: "storefront", Host: "203.0.113.10", Domain: "storefront.kipper.run"})
		return nil
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if err := mirrorGatewayTokenToConfig("storefront", "", "tok-refused"); err != nil {
		t.Fatalf("an empty entry must take the token: %v", err)
	}
	cfg, _ := config.Load()
	if got := cfg.GetCluster("storefront").GatewayToken; got != "tok-refused" {
		t.Errorf("token = %q, want the credential the retry needs", got)
	}
}

// And it replaces a value the caller did expect, which is how a stale token from
// an earlier install gets corrected by one read off the cluster.
func TestMirrorGatewayTokenReplacesTheValueItExpected(t *testing.T) {
	withFakeHome(t)

	if err := config.Update(func(cfg *config.Config) error {
		cfg.AddCluster(config.Cluster{
			Name: "storefront", Host: "203.0.113.10",
			Domain: "storefront.kipper.run", GatewayToken: "tok-stale",
		})
		return nil
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if err := mirrorGatewayTokenToConfig("storefront", "tok-stale", "tok-from-the-cluster"); err != nil {
		t.Fatalf("mirrorGatewayTokenToConfig: %v", err)
	}
	cfg, _ := config.Load()
	if got := cfg.GetCluster("storefront").GatewayToken; got != "tok-from-the-cluster" {
		t.Errorf("token = %q, want the cluster's own credential", got)
	}
}
