package migration

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestLooksLikeDomain(t *testing.T) {
	cases := map[string]bool{
		"appcann.com":         true,
		"acme.kipper.run":     true,
		"a.b.c.example.co.uk": true,
		"app":                 false, // single label, no dot
		"https://x.com":       false, // scheme
		"x.com/path":          false, // path
		"x .com":              false, // space
		"":                    false,
		"UPPER.com":           false, // must be lowercase
	}
	for in, want := range cases {
		if got := looksLikeDomain(in); got != want {
			t.Errorf("looksLikeDomain(%q) = %v, want %v", in, got, want)
		}
	}
}

func encodeToken(t *testing.T, tok Token) string {
	t.Helper()
	b, err := json.Marshal(tok) //nolint:gosec // G117: minting a test migration token is the point
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestDecodeToken_BaseDomain(t *testing.T) {
	base := Token{
		Endpoint: "https://console-api.appcann.com",
		Secret:   "deadbeef",
		Cluster:  "console.appcann.com",
		Expires:  time.Now().Add(time.Hour),
	}

	t.Run("valid base domain is carried through", func(t *testing.T) {
		tok := base
		tok.BaseDomain = "appcann.com"
		got, err := DecodeToken(encodeToken(t, tok))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.BaseDomain != "appcann.com" {
			t.Errorf("BaseDomain = %q, want appcann.com", got.BaseDomain)
		}
	})

	t.Run("malformed base domain is rejected", func(t *testing.T) {
		tok := base
		tok.BaseDomain = "https://evil.example/@steal"
		if _, err := DecodeToken(encodeToken(t, tok)); err == nil {
			t.Error("expected an error for a malformed base domain, got nil")
		}
	})

	t.Run("empty base domain is rejected", func(t *testing.T) {
		tok := base
		tok.BaseDomain = ""
		if _, err := DecodeToken(encodeToken(t, tok)); err == nil {
			t.Error("expected an error for a missing base domain, got nil")
		}
	})
}
