package installer

import (
	"testing"

	"github.com/getkipper/kipper/kip/internal/config"
)

// isolateHome points ~/.kip at a temporary directory. The store reads and writes
// the operator's real config otherwise.
func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

// The stub this writes on a first install is not scratch state: installer.Run
// reads it straight back and inherits its Name for the finished cluster. Naming
// it by IP left every fresh install called after its own address.
func TestLocalTokenStoreNamesAFirstInstallByItsDomain(t *testing.T) {
	isolateHome(t)
	store := localTokenStore{}

	if err := store.save("203.0.113.10", "203-0-113-10.kipper.run", "tok-1"); err != nil {
		t.Fatalf("save: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	entry := cfg.GetClusterByHost("203.0.113.10")
	if entry == nil {
		t.Fatal("the token must be durable before the install can fail")
	}
	if entry.Name != "203-0-113-10.kipper.run" {
		t.Errorf("Name = %q; the finished cluster inherits this, so it must be the domain", entry.Name)
	}
	if entry.Domain != "203-0-113-10.kipper.run" {
		t.Errorf("Domain = %q, want the claimed domain", entry.Domain)
	}
	if entry.GatewayToken != "tok-1" {
		t.Errorf("GatewayToken = %q, want tok-1", entry.GatewayToken)
	}
}

// A second save must update the entry rather than add another for the same host.
func TestLocalTokenStoreUpdatesAnExistingEntry(t *testing.T) {
	isolateHome(t)
	store := localTokenStore{}

	if err := store.save("203.0.113.10", "203-0-113-10.kipper.run", "tok-1"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.save("203.0.113.10", "203-0-113-10.kipper.run", "tok-2"); err != nil {
		t.Fatalf("second save: %v", err)
	}

	cfg, _ := config.Load()
	if n := len(cfg.Clusters); n != 1 {
		t.Fatalf("clusters = %d, want one entry per host", n)
	}
	if got := store.tokenFor("203.0.113.10"); got != "tok-2" {
		t.Errorf("tokenFor = %q, want the latest token", got)
	}
}

// Clearing a token for a host with no entry must not invent one, or a rollback
// leaves a stub naming a cluster that never existed.
func TestLocalTokenStoreDoesNotCreateAnEntryToHoldNothing(t *testing.T) {
	isolateHome(t)

	if err := (localTokenStore{}).save("203.0.113.10", "203-0-113-10.kipper.run", ""); err != nil {
		t.Fatalf("clearing an absent entry is not an error: %v", err)
	}
	cfg, _ := config.Load()
	if len(cfg.Clusters) != 0 {
		t.Errorf("clusters = %d, want none — nothing was worth recording", len(cfg.Clusters))
	}
}

// An unknown host holds no token; that is an empty answer, not a failure.
func TestLocalTokenStoreReportsNoTokenForAnUnknownHost(t *testing.T) {
	isolateHome(t)
	if got := (localTokenStore{}).tokenFor("203.0.113.99"); got != "" {
		t.Errorf("tokenFor = %q, want empty", got)
	}
}

// Reinstalling onto a host whose earlier wipe could not release its name. The
// marker that sends `kip cluster uninstall` straight to the gateway has to go,
// or the next uninstall hands back the name of a cluster it never wiped and
// deletes the entry, leaving a live k3s nothing records.
func TestClearHostWipedMarkerRetiresTheMarkerOnAReinstall(t *testing.T) {
	isolateHome(t)
	seedWipedCluster(t)

	if err := ClearHostWipedMarker("203.0.113.10"); err != nil {
		t.Fatalf("ClearHostWipedMarker: %v", err)
	}

	cfg, _ := config.Load()
	entry := cfg.GetClusterByHost("203.0.113.10")
	if entry == nil {
		t.Fatal("the entry must survive")
	}
	if entry.HostWiped {
		t.Error("left the marker set, so the next uninstall would skip the host it must destroy")
	}
	if entry.GatewayToken != "tok-old" {
		t.Errorf("GatewayToken = %q; clearing the marker must not disturb the credential", entry.GatewayToken)
	}
}

// A host nobody has an entry for, and an entry that was never wiped. Neither is
// a failure, and neither should write the config.
func TestClearHostWipedMarkerIsQuietWhenThereIsNothingToClear(t *testing.T) {
	isolateHome(t)

	if err := ClearHostWipedMarker("203.0.113.99"); err != nil {
		t.Errorf("an unknown host is not a failure: %v", err)
	}

	cfg, _ := config.Load()
	cfg.AddCluster(config.Cluster{Name: "shop", Host: "203.0.113.10", Domain: "shop.example.com"})
	if err := config.Save(cfg); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := ClearHostWipedMarker("203.0.113.10"); err != nil {
		t.Errorf("a live cluster is not a failure: %v", err)
	}
}

func seedWipedCluster(t *testing.T) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.AddCluster(config.Cluster{
		Name: "203-0-113-10.kipper.run", Host: "203.0.113.10",
		Domain: "203-0-113-10.kipper.run", GatewayToken: "tok-old", HostWiped: true,
	})
	if err := config.Save(cfg); err != nil {
		t.Fatalf("seeding: %v", err)
	}
}

// A config can hold more than one entry for a host — an install whose checkpoint
// could not read the config appends a second under a different name. The
// first-match lookup skips straight past the marked one when an unmarked entry
// comes first, leaving the marker set on a host that is going live again.
func TestClearHostWipedMarkerClearsEveryEntryForTheHost(t *testing.T) {
	isolateHome(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.AddCluster(config.Cluster{
		Name: "storefront", Host: "203.0.113.10", Domain: "storefront.example.com",
	})
	cfg.AddCluster(config.Cluster{
		Name: "203-0-113-10.kipper.run", Host: "203.0.113.10",
		Domain: "203-0-113-10.kipper.run", GatewayToken: "tok-old", HostWiped: true,
	})
	if err := config.Save(cfg); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if err := ClearHostWipedMarker("203.0.113.10"); err != nil {
		t.Fatalf("ClearHostWipedMarker: %v", err)
	}

	cfg, _ = config.Load()
	for _, c := range cfg.Clusters {
		if c.Host == "203.0.113.10" && c.HostWiped {
			t.Errorf("%s still marked wiped on a host that is going live again", c.Name)
		}
	}
}

// The token this install is handing back must go, but only from an entry that
// still holds it. A failing install can reach here after another command has
// recorded a live credential under the same host, and clearing that would
// destroy the only local copy of it.
func TestClearTokenIfHeldLeavesACredentialItDoesNotOwn(t *testing.T) {
	isolateHome(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.AddCluster(config.Cluster{
		Name: "203-0-113-10.kipper.run", Host: "203.0.113.10",
		Domain: "203-0-113-10.kipper.run", GatewayToken: "tok-recorded-since",
	})
	if err := config.Save(cfg); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if err := clearTokenIfHeld("tok-this-install-gave-back"); err != nil {
		t.Fatalf("an entry holding something else is not a failure: %v", err)
	}

	cfg, _ = config.Load()
	if got := cfg.GetClusterByHost("203.0.113.10").GatewayToken; got != "tok-recorded-since" {
		t.Errorf("token = %q — cleared a credential this install never issued", got)
	}
}

// And the dead one does go, or a later command presents it and reads the
// gateway's "not registered" as the name having been released.
func TestClearTokenIfHeldClearsTheTokenItGaveBack(t *testing.T) {
	isolateHome(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.AddCluster(config.Cluster{
		Name: "203-0-113-10.kipper.run", Host: "203.0.113.10",
		Domain: "203-0-113-10.kipper.run", GatewayToken: "tok-gave-back",
	})
	if err := config.Save(cfg); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if err := clearTokenIfHeld("tok-gave-back"); err != nil {
		t.Fatalf("clearTokenIfHeld: %v", err)
	}
	cfg, _ = config.Load()
	if got := cfg.GetClusterByHost("203.0.113.10").GatewayToken; got != "" {
		t.Errorf("token = %q, want the dead credential gone", got)
	}
}
