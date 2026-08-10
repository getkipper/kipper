package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

// Two runs changing different clusters must both survive. Load-modify-save as
// three separate acts loses whichever finishes first, which is how an uninstall
// deleted an entry another command had just written.
func TestUpdateDoesNotLoseAConcurrentChange(t *testing.T) {
	isolateHome(t)

	if err := Update(func(cfg *Config) error {
		cfg.AddCluster(Cluster{Name: "one", Host: "203.0.113.10", Domain: "one.example.com"})
		cfg.AddCluster(Cluster{Name: "two", Host: "203.0.113.11", Domain: "two.example.com"})
		return nil
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	var wg sync.WaitGroup
	for _, name := range []string{"one", "two"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			_ = Update(func(cfg *Config) error {
				cfg.GetCluster(name).GatewayToken = "tok-" + name
				return nil
			})
		}(name)
	}
	wg.Wait()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, name := range []string{"one", "two"} {
		if got := cfg.GetCluster(name).GatewayToken; got != "tok-"+name {
			t.Errorf("%s token = %q, want tok-%s — the other run's write was lost", name, got, name)
		}
	}
}

// A mutation with nothing to do must leave the file alone. Rewriting it is what
// makes a concurrent reader see a different file for no reason.
func TestUpdateLeavesTheFileAloneOnErrNoChange(t *testing.T) {
	isolateHome(t)

	if err := Update(func(cfg *Config) error {
		cfg.AddCluster(Cluster{Name: "one", Host: "203.0.113.10", Domain: "one.example.com"})
		return nil
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	path, err := Path()
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if err := Update(func(*Config) error { return ErrNoChange }); err != nil {
		t.Fatalf("ErrNoChange is not a failure: %v", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("rewrote the config for a mutation that changed nothing")
	}
}

// A mutation that fails must not be written, or a refusal becomes a partial edit.
func TestUpdateWritesNothingWhenTheMutationFails(t *testing.T) {
	isolateHome(t)

	boom := errors.New("no")
	if err := Update(func(cfg *Config) error {
		cfg.AddCluster(Cluster{Name: "one", Host: "203.0.113.10", Domain: "one.example.com"})
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the mutation's own error", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Clusters) != 0 {
		t.Errorf("clusters = %d, want none written", len(cfg.Clusters))
	}
}

// The lock file is not the config file, because the config is replaced by
// rename: a lock on the old inode orders nobody.
func TestUpdateLocksASeparateFile(t *testing.T) {
	isolateHome(t)

	if err := Update(func(*Config) error { return nil }); err != nil {
		t.Fatalf("update: %v", err)
	}
	dir, _ := Dir()
	if _, err := os.Stat(filepath.Join(dir, "config.lock")); err != nil {
		t.Errorf("no lock file: %v", err)
	}
}
