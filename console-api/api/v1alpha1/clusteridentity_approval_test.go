package v1alpha1

import "testing"

func TestCutoverApprovalHashIsStable(t *testing.T) {
	from := ResolvedHosts{Console: "console-acme.kipper.run", ConsoleAPI: "console-api-acme.kipper.run", Dex: "dex-acme.kipper.run", Issuer: "https://dex-acme.kipper.run/dex"}
	to := ResolvedHosts{Console: "console--acme.kipper.run", ConsoleAPI: "console-api--acme.kipper.run", Dex: "dex--acme.kipper.run", Issuer: "https://dex--acme.kipper.run/dex"}

	a := CutoverApprovalHash(3, from, to, "nonce-xyz")
	b := CutoverApprovalHash(3, from, to, "nonce-xyz")
	if a != b {
		t.Fatalf("hash is not deterministic: %q != %q", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("expected a 64-char sha256 hex digest, got %d chars", len(a))
	}
}

// Every bound field must change the hash, so a stale or replayed approval can
// never match a different transition.
func TestCutoverApprovalHashBindsEveryField(t *testing.T) {
	from := ResolvedHosts{Console: "c-old", ConsoleAPI: "ca-old", Dex: "d-old", Issuer: "https://d-old/dex"}
	to := ResolvedHosts{Console: "c-new", ConsoleAPI: "ca-new", Dex: "d-new", Issuer: "https://d-new/dex"}
	base := CutoverApprovalHash(1, from, to, "n1")

	cases := map[string]string{
		"generation": CutoverApprovalHash(2, from, to, "n1"),
		"nonce":      CutoverApprovalHash(1, from, to, "n2"),
		"from host":  CutoverApprovalHash(1, ResolvedHosts{Console: "c-x", ConsoleAPI: "ca-old", Dex: "d-old", Issuer: "https://d-old/dex"}, to, "n1"),
		"to host":    CutoverApprovalHash(1, from, ResolvedHosts{Console: "c-x", ConsoleAPI: "ca-new", Dex: "d-new", Issuer: "https://d-new/dex"}, "n1"),
		"to issuer":  CutoverApprovalHash(1, from, ResolvedHosts{Console: "c-new", ConsoleAPI: "ca-new", Dex: "d-new", Issuer: "https://other/dex"}, "n1"),
	}
	for name, h := range cases {
		if h == base {
			t.Errorf("hash did not change when %s changed", name)
		}
	}
}

// The field separator must prevent aliasing: moving a character across the
// boundary between two hosts must not produce the same hash.
func TestCutoverApprovalHashNoFieldAliasing(t *testing.T) {
	a := CutoverApprovalHash(1, ResolvedHosts{Console: "ab", ConsoleAPI: "c"}, ResolvedHosts{}, "n")
	b := CutoverApprovalHash(1, ResolvedHosts{Console: "a", ConsoleAPI: "bc"}, ResolvedHosts{}, "n")
	if a == b {
		t.Fatal("hash aliases across the host-field boundary")
	}
}
