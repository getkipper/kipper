package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getkipper/kipper/gateway/internal/registry"
)

// captureLog changes the global logger; callers must run serially.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(flags)
	})
	return &buf
}

func TestRegisterLogsANewRegistration(t *testing.T) {
	dataPath = filepath.Join(t.TempDir(), "registry.json")
	logs := captureLog(t)

	code, _ := postRegister(t, handleRegister(registry.New(), "kipper.run", neverObserve), `{"subdomain":"myapp","ip":"198.51.100.1"}`)

	if code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", code)
	}
	if got := logs.String(); !strings.Contains(got, "registered myapp for 198.51.100.1") {
		t.Errorf("a new registration must be logged with its name and address, got %q", got)
	}
}

func TestRegisterRenewalIsNotLogged(t *testing.T) {
	dataPath = filepath.Join(t.TempDir(), "registry.json")
	reg := registry.New()
	entry, _, _ := reg.Register("myapp", "198.51.100.1", "")
	logs := captureLog(t)

	_, _ = postRegister(t, handleRegister(reg, "kipper.run", neverObserve), `{"subdomain":"myapp","ip":"198.51.100.1","token":"`+entry.Token+`"}`)

	if got := logs.String(); strings.Contains(got, "registered") || strings.Contains(got, "refused") {
		t.Errorf("a daily renewal must not add a registration line, got %q", got)
	}
}

func TestRegisterLogsEachRefusalWithItsReason(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"reserved label", `{"subdomain":"admin","ip":"198.51.100.1"}`,
			"refused registration of admin: subdomain is reserved"},
		{"non-public address", `{"subdomain":"myapp","ip":"10.0.0.1"}`,
			"refused registration of myapp: ip must be a public address"},
		{"address-shaped label for another address", `{"subdomain":"198-51-100-7","ip":"198.51.100.1"}`,
			"refused registration of 198-51-100-7: a subdomain that spells an IP address may only be registered by that address"},
		{"name held by someone else", `{"subdomain":"taken","ip":"198.51.100.2"}`,
			"refused registration of taken: subdomain is already registered: \"taken\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataPath = filepath.Join(t.TempDir(), "registry.json")
			reg := registry.New()
			_, _, _ = reg.Register("taken", "198.51.100.1", "")
			logs := captureLog(t)

			code, _ := postRegister(t, handleRegister(reg, "kipper.run", neverObserve), tt.body)

			if code < 400 {
				t.Fatalf("expected a refusal, got %d", code)
			}
			if got := logs.String(); !strings.Contains(got, tt.want) {
				t.Errorf("expected a log line containing %q, got %q", tt.want, got)
			}
		})
	}
}

func TestRegisterRefusalDoesNotEchoAnUnvalidatedLabel(t *testing.T) {
	logs := captureLog(t)

	code, _ := postRegister(t, handleRegister(registry.New(), "kipper.run", neverObserve), `{"subdomain":"evil\nregistered admin for 198.51.100.9","ip":"198.51.100.1"}`)

	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", code)
	}
	got := logs.String()
	if !strings.Contains(got, "refused a registration: malformed subdomain") {
		t.Errorf("a malformed label must still be logged as a refusal, got %q", got)
	}
	if strings.Contains(got, "evil") {
		t.Errorf("the raw label must not reach the log, got %q", got)
	}
}

func TestDeregisterLogsTheRelease(t *testing.T) {
	dataPath = filepath.Join(t.TempDir(), "registry.json")
	reg := registry.New()
	entry, _, _ := reg.Register("myapp", "198.51.100.1", "")
	logs := captureLog(t)

	handler := handleDeregister(reg, &Proxy{Registry: reg, BaseDomain: "kipper.run"})
	r := httptest.NewRequest(http.MethodDelete, "/register", strings.NewReader(`{"token":"`+entry.Token+`"}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if got := logs.String(); !strings.Contains(got, `released "myapp" at its holder's request`) {
		t.Errorf("a release must be logged, got %q", got)
	}
}

func TestProofLogsAnUnknownSubdomainQuoted(t *testing.T) {
	reg, _ := proofSetup(t)
	logs := captureLog(t)

	handler := handleProof(reg, "kipper.run", stubObserve(&genKey(t).PublicKey, "spki"))
	postProof(t, handler, proofRequest{Subdomain: "ghost\nproof recorded", Token: "x", Nonce: "y", Signature: "z"})

	if got := logs.String(); !strings.Contains(got, `refused proof for "ghost\nproof recorded": unknown subdomain`) {
		t.Errorf("an unknown name must be logged quoted, got %q", got)
	}
}

func TestProofLogsAChallengeMismatch(t *testing.T) {
	reg, entry := proofSetup(t)
	logs := captureLog(t)

	handler := handleProof(reg, "kipper.run", stubObserve(&genKey(t).PublicKey, "spki"))
	postProof(t, handler, proofRequest{Subdomain: "myapp", Token: entry.Token, Nonce: "not-issued", Signature: "z"})

	if got := logs.String(); !strings.Contains(got, `refused proof for "myapp": invalid token or no matching outstanding challenge`) {
		t.Errorf("a proof with no matching challenge must be logged, got %q", got)
	}
}

func TestSweepNamesTheRegistrationsItReleases(t *testing.T) {
	dir := t.TempDir()
	dataPath = filepath.Join(dir, "registry.json")
	created := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339Nano)
	seed := `{"entries":[{"subdomain":"stale","ip":"198.51.100.1","token":"t","created_at":"` + created +
		`","last_seen":"` + created + `"}]}`
	if err := os.WriteFile(dataPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	reg.EnforceProof = true
	if err := reg.LoadFrom(dataPath); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)

	sweep(reg, &Proxy{Registry: reg, BaseDomain: "kipper.run"})

	if reg.Lookup("stale") != nil {
		t.Fatal("an unproven registration past its reservation must be released")
	}
	if got := logs.String(); !strings.Contains(got, `released 1 lapsed registration(s): "stale"`) {
		t.Errorf("the sweep must name what it released, got %q", got)
	}
}

// loadSnapshot writes a registry snapshot and loads it, bypassing the label
// validation the HTTP API applies.
func loadSnapshot(t *testing.T, entries string) *registry.Registry {
	t.Helper()
	dataPath = filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(dataPath, []byte(`{"entries":[`+entries+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	reg.EnforceProof = true
	if err := reg.LoadFrom(dataPath); err != nil {
		t.Fatal(err)
	}
	return reg
}

const forgedName = `legacy\nregistered admin for 198.51.100.9`

func TestLogsQuoteANameLoadedFromASnapshot(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	entry := `{"subdomain":"` + forgedName + `","ip":"198.51.100.1","token":"tok","created_at":"` + now + `","last_seen":"` + now + `"}`

	t.Run("proof refusal", func(t *testing.T) {
		reg := loadSnapshot(t, entry)
		logs := captureLog(t)
		handler := handleProof(reg, "kipper.run", stubObserve(&genKey(t).PublicKey, "spki"))
		postProof(t, handler, proofRequest{Subdomain: "legacy\nregistered admin for 198.51.100.9", Token: "wrong", Nonce: "n", Signature: "s"})
		if got := logs.String(); !strings.Contains(got, `refused proof for "legacy\nregistered admin for 198.51.100.9"`) {
			t.Errorf("a snapshot name must be logged quoted, got %q", got)
		}
	})

	t.Run("holder release", func(t *testing.T) {
		reg := loadSnapshot(t, entry)
		logs := captureLog(t)
		handler := handleDeregister(reg, &Proxy{Registry: reg, BaseDomain: "kipper.run"})
		r := httptest.NewRequest(http.MethodDelete, "/register", strings.NewReader(`{"token":"tok"}`))
		handler.ServeHTTP(httptest.NewRecorder(), r)
		if got := logs.String(); !strings.Contains(got, `released "legacy\nregistered admin for 198.51.100.9" at its holder's request`) {
			t.Errorf("a snapshot name must be logged quoted, got %q", got)
		}
	})

	t.Run("sweep", func(t *testing.T) {
		old := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339Nano)
		reg := loadSnapshot(t, `{"subdomain":"`+forgedName+`","ip":"198.51.100.1","token":"a","created_at":"`+old+`","last_seen":"`+old+`"},`+
			`{"subdomain":"stale","ip":"198.51.100.2","token":"b","created_at":"`+old+`","last_seen":"`+old+`"}`)
		logs := captureLog(t)
		sweep(reg, &Proxy{Registry: reg, BaseDomain: "kipper.run"})
		if got := logs.String(); !strings.Contains(got, `released 2 lapsed registration(s): "legacy\nregistered admin for 198.51.100.9", "stale"`) {
			t.Errorf("every released name must be logged quoted, got %q", got)
		}
	})
}

func TestProofLogTruncatesAnOverlongUnknownName(t *testing.T) {
	reg, _ := proofSetup(t)
	logs := captureLog(t)
	handler := handleProof(reg, "kipper.run", stubObserve(&genKey(t).PublicKey, "spki"))
	postProof(t, handler, proofRequest{Subdomain: strings.Repeat("a", 3000), Token: "x", Nonce: "y", Signature: "z"})
	got := logs.String()
	if !strings.Contains(got, `refused proof for "`+strings.Repeat("a", 63)+`": unknown subdomain`) {
		t.Errorf("an unknown name must be logged truncated to 63 characters, got %q", got)
	}
}
