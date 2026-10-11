package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/getkipper/kipper/gateway/internal/registry"
)

// unwritableDataPath makes saves fail until the test restores the previous path.
func unwritableDataPath(t *testing.T) {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	previous := dataPath
	dataPath = filepath.Join(blocker, "registry.json")
	t.Cleanup(func() { dataPath = previous })
}

// Issue a token only after saving the registration so it survives a restart.
func TestRegisterRefusesANewRegistrationItCouldNotSave(t *testing.T) {
	reg := registry.New()
	handler := handleRegister(reg, "kipper.run", neverObserve)
	unwritableDataPath(t)

	code, resp := postRegister(t, handler, `{"subdomain":"myapp","ip":"198.51.100.1"}`)

	if code != http.StatusInternalServerError {
		t.Fatalf("an unsaved registration must answer 500, got %d", code)
	}
	if resp.Token != "" {
		t.Error("no token may be handed out for a registration that was not recorded")
	}
	if reg.Count() != 0 {
		t.Error("the unsaved registration must be withdrawn so a retry can create it")
	}
}

// Report failed saves so the token holder can retry the renewal.
func TestRegisterRefusesARenewalItCouldNotSave(t *testing.T) {
	reg := registry.New()
	handler := handleRegister(reg, "kipper.run", neverObserve)
	code, created := postRegister(t, handler, `{"subdomain":"myapp","ip":"198.51.100.1"}`)
	if code != http.StatusCreated {
		t.Fatalf("seeding: got %d", code)
	}
	unwritableDataPath(t)

	code, _ = postRegister(t, handler, `{"subdomain":"myapp","ip":"198.51.100.2","token":"`+created.Token+`"}`)

	if code != http.StatusInternalServerError {
		t.Errorf("an unsaved renewal must answer 500, got %d", code)
	}
}
