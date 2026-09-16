package main

import (
	"io"
	"net/http"
	"testing"
)

func TestDenyAnswersNotFound(t *testing.T) {
	srv := testServer(t, alwaysFresh())

	resp := get(t, srv.URL+"/deny", "")

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the response empty to avoid disclosing endpoint details.
	if len(body) != 0 {
		t.Errorf("body = %q, want it empty", body)
	}
	for _, h := range []string{"Content-Type", "Retry-After", "WWW-Authenticate"} {
		if got := resp.Header.Get(h); got != "" {
			t.Errorf("%s = %q, want it absent", h, got)
		}
	}
}

func TestDenyIgnoresAnAPIKey(t *testing.T) {
	srv := testServer(t, alwaysFresh())

	resp := get(t, srv.URL+"/deny", testKey)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}
