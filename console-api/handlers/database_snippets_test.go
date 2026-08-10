package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkipper/kipper/console-api/middleware"
)

func TestSnippetKey(t *testing.T) {
	cases := map[string]string{
		"recent users": "recent_users",
		"top-10":       "top-10",
		"q.with.dots":  "q.with.dots",
		"weird/slash":  "weird_slash",
		"emoji 🚀 fun":  "emoji___fun",
		"":             "",
	}
	for in, want := range cases {
		if got := snippetKey(in); got != want {
			t.Errorf("snippetKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDataKey_SanitisesEmails(t *testing.T) {
	// Secret data keys must match [-._a-zA-Z0-9]+; @ in emails was
	// the original failure mode that left history empty for a real user.
	cases := map[string]string{
		"admin@203-0-113-237.kipper.run": "admin_203-0-113-237.kipper.run",
		"team@example.com":               "team_example.com",
		"alice+work@example.com":         "alice_work_example.com",
		"anonymous":                      "anonymous",
	}
	for in, want := range cases {
		if got := dataKey(in); got != want {
			t.Errorf("dataKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUserIdentifier_Anonymous(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	if got := userIdentifier(r); got != "anonymous" {
		t.Errorf("got %q, want anonymous", got)
	}
}

func TestUserIdentifier_FromClaims(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	claims := &middleware.Claims{Email: "alice@example.com"}
	ctx := context.WithValue(r.Context(), middleware.UserContextKey, claims)
	r = r.WithContext(ctx)
	if got := userIdentifier(r); got != "alice@example.com" {
		t.Errorf("got %q, want alice@example.com", got)
	}
}

func TestMustJSON(t *testing.T) {
	out := mustJSON(map[string]string{"a": "b"})
	if !strings.Contains(out, `"a":"b"`) {
		t.Errorf("got %q", out)
	}
}

func TestSnippetSortOrder_PinnedFirstThenAlpha(t *testing.T) {
	// We don't have a sortable handler test without spinning up a fake
	// Kubernetes cluster, but we can exercise the same comparator
	// directly by replicating the slice.Sort call in ListSnippets.
	in := []snippet{
		{Name: "zebra"},
		{Name: "alpha", Pinned: true},
		{Name: "Beta"},
		{Name: "gamma", Pinned: true},
	}
	// Mirror the comparator inline.
	for pass := 0; pass < len(in); pass++ {
		for i := 0; i < len(in)-1; i++ {
			a, b := in[i], in[i+1]
			less := false
			if a.Pinned != b.Pinned {
				less = a.Pinned
			} else {
				less = strings.ToLower(a.Name) < strings.ToLower(b.Name)
			}
			if !less {
				in[i], in[i+1] = b, a
			}
		}
	}
	want := []string{"alpha", "gamma", "Beta", "zebra"}
	for i, name := range want {
		if in[i].Name != name {
			t.Errorf("position %d: got %q, want %q", i, in[i].Name, name)
		}
	}
}

func TestHistoryEntry_RoundTripsAsJSON(t *testing.T) {
	e := historyEntry{
		SQL: "SELECT 1", DurationMs: 5, Error: "", Timestamp: "2026-04-27T12:00:00Z", User: "alice@example.com",
	}
	raw := mustJSON([]historyEntry{e})
	var back []historyEntry
	if err := json.Unmarshal([]byte(raw), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back) != 1 || back[0].SQL != "SELECT 1" || back[0].User != "alice@example.com" {
		t.Errorf("round-trip mismatch: %+v", back)
	}
}

// Replicates the dedupe logic from recordHistory: re-running the same
// SQL bumps the existing entry to the top with fresh metadata instead
// of stacking duplicates.
func TestHistoryDedupe_SameSQLBumpsToTop(t *testing.T) {
	existing := []historyEntry{
		{SQL: "SELECT 2", DurationMs: 8, Timestamp: "t2"},
		{SQL: "SELECT 1", DurationMs: 5, Timestamp: "t1"},
	}
	entry := historyEntry{SQL: "SELECT 1", DurationMs: 12, Timestamp: "t3"}

	filtered := existing[:0]
	for _, e := range existing {
		if e.SQL != entry.SQL {
			filtered = append(filtered, e)
		}
	}
	out := append([]historyEntry{entry}, filtered...)

	if len(out) != 2 {
		t.Fatalf("expected 2 entries after dedupe, got %d: %+v", len(out), out)
	}
	if out[0].SQL != "SELECT 1" || out[0].DurationMs != 12 || out[0].Timestamp != "t3" {
		t.Errorf("expected SELECT 1 promoted with fresh metadata, got %+v", out[0])
	}
	if out[1].SQL != "SELECT 2" {
		t.Errorf("expected SELECT 2 retained, got %+v", out[1])
	}
}

// http.Request is referenced through net/http; keep a build-time
// reference so future refactors don't drop the import accidentally.
var _ = http.MethodGet
