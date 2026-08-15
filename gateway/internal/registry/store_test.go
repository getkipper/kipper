package registry

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/getkipper/kipper/controller/pkg/hostnames"
)

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	r1 := New()
	_, _, _ = r1.Register("myapp", "198.51.100.1", "")
	_, _, _ = r1.Register("staging", "10.0.0.2", "")

	if err := r1.SaveTo(path); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	r2 := New()
	if err := r2.LoadFrom(path); err != nil {
		t.Fatalf("load failed: %v", err)
	}

	if r2.Count() != 2 {
		t.Errorf("expected 2 entries, got %d", r2.Count())
	}

	entry := r2.Lookup("myapp")
	if entry == nil {
		t.Fatal("expected to find myapp")
	}
	if entry.IP != "198.51.100.1" {
		t.Errorf("expected 198.51.100.1, got %s", entry.IP)
	}
	if entry.Token == "" {
		t.Error("expected token to survive round-trip")
	}
}

func TestPruneDropsRejectedIPs(t *testing.T) {
	r := New()
	_, _, _ = r.Register("keep", "198.51.100.1", "")
	stale, _, _ := r.Register("drop", "203.0.113.7", "")

	// Simulate a stricter policy that rejects the second entry's IP.
	removed := r.Prune(func(ip string) bool { return ip != "203.0.113.7" })
	if removed != 1 {
		t.Fatalf("expected 1 entry pruned, got %d", removed)
	}
	if r.Lookup("drop") != nil {
		t.Error("pruned entry should be gone")
	}
	if r.Lookup("keep") == nil {
		t.Error("kept entry should remain")
	}
	// The pruned entry's token must not survive, or a stale token could still
	// ping the entry back to life.
	if err := r.Ping(stale.Token); err == nil {
		t.Error("pruned entry's token should no longer be accepted")
	}
}

func TestLoadFromMissingFileStartsEmpty(t *testing.T) {
	r := New()
	err := r.LoadFrom("/nonexistent/registry.json")
	if err != nil {
		t.Fatalf("expected no error for missing file, got: %v", err)
	}
	if r.Count() != 0 {
		t.Errorf("expected 0 entries, got %d", r.Count())
	}
}

func TestLoadFromCorruptedFileReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	_ = os.WriteFile(path, []byte("{{{broken"), 0o600)

	r := New()
	if err := r.LoadFrom(path); err == nil {
		t.Fatal("expected error for corrupted file")
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	r := New()
	_, _, _ = r.Register("myapp", "203.0.113.1", "")

	if err := r.SaveTo(path); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file should be cleaned up after save")
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("registry file should exist: %v", err)
	}
}

func TestFlushIfDirtyNoopWhenClean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	r := New()
	_, _, _ = r.Register("myapp", "198.51.100.1", "")

	// Register marks the registry dirty so a failed handler save is retried
	// by the periodic flush; the first flush after it writes once.
	saved, err := r.FlushIfDirty(path)
	if err != nil {
		t.Fatalf("flush failed: %v", err)
	}
	if !saved {
		t.Error("expected the registration to be flushed")
	}

	// With no intervening change the next flush is a no-op.
	saved, err = r.FlushIfDirty(path)
	if err != nil {
		t.Fatalf("clean flush failed: %v", err)
	}
	if saved {
		t.Error("expected no write when nothing changed")
	}
}

func TestFlushIfDirtyPersistsPing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	r := New()
	entry, _, _ := r.Register("myapp", "198.51.100.1", "")
	if err := r.SaveTo(path); err != nil {
		t.Fatalf("initial save failed: %v", err)
	}

	time.Sleep(2 * time.Millisecond)
	if err := r.Ping(entry.Token); err != nil {
		t.Fatalf("ping failed: %v", err)
	}
	pinged := entry.LastSeen

	saved, err := r.FlushIfDirty(path)
	if err != nil {
		t.Fatalf("flush failed: %v", err)
	}
	if !saved {
		t.Fatal("expected a write after a ping")
	}

	// The persisted LastSeen must reflect the ping, or an actively-pinged
	// subdomain would revert to its registration time after a restart.
	r2 := New()
	if err := r2.LoadFrom(path); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	loaded := r2.Lookup("myapp")
	if loaded == nil {
		t.Fatal("expected to find myapp after reload")
	}
	if !loaded.LastSeen.Equal(pinged) {
		t.Errorf("persisted LastSeen %v does not match pinged %v", loaded.LastSeen, pinged)
	}

	// The flag clears on a successful flush, so a follow-up flush is a no-op.
	saved, err = r.FlushIfDirty(path)
	if err != nil {
		t.Fatalf("second flush failed: %v", err)
	}
	if saved {
		t.Error("expected no write on a second flush with no new ping")
	}
}

func TestTokensSurviveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	r1 := New()
	entry, _, _ := r1.Register("myapp", "203.0.113.1", "")
	token := entry.Token

	_ = r1.SaveTo(path)

	r2 := New()
	_ = r2.LoadFrom(path)

	if _, err := r2.Deregister(token); err != nil {
		t.Fatalf("deregister with original token failed: %v", err)
	}
	if r2.Count() != 0 {
		t.Error("expected 0 entries after deregister")
	}
}

// Startup revalidation is where a policy tightened after a snapshot was written
// gets applied. Without it the strongest new reservations protect only unused
// names: an entry registered as "login" when that was allowed keeps serving
// login.kipper.run, and every derived host under it, indefinitely.
func TestPruneEntriesDropsWhatCurrentPolicyRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	now := time.Now().UTC().Format(time.RFC3339)
	entry := func(sub, ip string) string {
		return `{"subdomain":"` + sub + `","ip":"` + ip + `","token":"tok-` + sub +
			`","created_at":"` + now + `","last_seen":"` + now + `"}`
	}
	snap := `{"entries":[` +
		entry("login", "203.0.113.1") + `,` + // reserved only by the newer policy
		entry("203-0-113-77", "198.51.100.9") + `,` + // another server's default name
		entry("203-0-113-78", "203.0.113.78") + `,` + // its own default name, legitimate
		entry("console--acme", "203.0.113.3") + `,` + // would shadow a derived service route
		entry("acme", "203.0.113.2") + // an ordinary name
		`]}`
	if err := os.WriteFile(path, []byte(snap), 0o600); err != nil {
		t.Fatal(err)
	}

	r := New()
	if err := r.LoadFrom(path); err != nil {
		t.Fatal(err)
	}
	dropped := r.PruneEntries(registrable)
	if dropped != 3 {
		t.Errorf("dropped %d, want 3 (reserved label, mismatched default name, route-shadowing label)", dropped)
	}
	for _, gone := range []string{"login", "203-0-113-77", "console--acme"} {
		if r.Lookup(gone) != nil {
			t.Errorf("%s must not survive startup revalidation", gone)
		}
	}
	for _, kept := range []string{"203-0-113-78", "acme"} {
		if r.Lookup(kept) == nil {
			t.Errorf("%s is registrable under current policy and must survive", kept)
		}
	}

	// The token index has to move with the entries, or a dropped registration's
	// token still names a subdomain that is gone.
	if err := r.Ping("tok-login"); err == nil {
		t.Error("a dropped registration's token must no longer ping")
	}
}

// registrable mirrors the gateway's own admission rule for this test.
func registrable(subdomain, ip string) bool {
	return hostnames.ValidateClusterLabel(subdomain) == nil &&
		(!hostnames.IPShapedLabel(subdomain) || subdomain == hostnames.LabelForIP(ip))
}
