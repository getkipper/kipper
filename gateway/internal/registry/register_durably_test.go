package registry

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func unwritablePath(t *testing.T) string {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(blocker, "registry.json")
}

func TestRegisterDurablyWithdrawsANewRegistrationItCouldNotSave(t *testing.T) {
	r := New()

	_, outcome, err := r.RegisterDurably("myapp", "198.51.100.1", "", unwritablePath(t))

	if !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("want ErrNotRecorded, got %v", err)
	}
	if outcome != Created {
		t.Errorf("want the Created outcome reported, got %v", outcome)
	}
	if r.Count() != 0 {
		t.Error("an unsaved new registration must be withdrawn")
	}
}

func TestRegisterDurablySavesANewRegistration(t *testing.T) {
	r := New()
	path := filepath.Join(t.TempDir(), "registry.json")

	entry, outcome, err := r.RegisterDurably("myapp", "198.51.100.1", "", path)

	if err != nil || outcome != Created || entry.Token == "" {
		t.Fatalf("got entry=%v outcome=%v err=%v", entry, outcome, err)
	}
	loaded := New()
	if err := loaded.LoadFrom(path); err != nil || loaded.Count() != 1 {
		t.Fatalf("the registration is not on disk: count=%d", loaded.Count())
	}
}

// Holding saveMu must delay registration so concurrent saves cannot include
// an entry that may still be rolled back.
func TestRegisterDurablyRegistersUnderTheSaveLock(t *testing.T) {
	r := New()
	path := filepath.Join(t.TempDir(), "registry.json")
	r.saveMu.Lock()
	done := make(chan struct{})
	go func() {
		_, _, _ = r.RegisterDurably("myapp", "198.51.100.1", "", path)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	if r.Count() != 0 {
		t.Error("the registration became visible to other saves before it held the save lock")
	}
	r.saveMu.Unlock()
	<-done
}
