package identity

import "testing"

// The approval hash is a cross-module contract: the CLI computes it from CR
// status and the reconciler recomputes it. This golden value pins the exact
// byte encoding so a change on either side that would silently invalidate every
// operator's approval fails here first.
func TestApprovalHashGolden(t *testing.T) {
	from := HostKey("console.x", "console-api.x", "dex.x", "https://dex.x/dex")
	to := HostKey("console.y", "console-api.y", "dex.y", "https://dex.y/dex")
	got := ApprovalHash(3, from, to, "abcd1234")
	const want = "def30a15caf7f187e0106e7afed368cdf3d9e472a5f81539047848720f7138b2"
	if got != want {
		t.Fatalf("approval hash encoding changed:\n got  %s\n want %s", got, want)
	}
}

func TestApprovalHashDistinctInputs(t *testing.T) {
	base := ApprovalHash(1, HostKey("a", "b", "c", "d"), HostKey("e", "f", "g", "h"), "n")
	cases := map[string]string{
		"generation": ApprovalHash(2, HostKey("a", "b", "c", "d"), HostKey("e", "f", "g", "h"), "n"),
		"from":       ApprovalHash(1, HostKey("z", "b", "c", "d"), HostKey("e", "f", "g", "h"), "n"),
		"to":         ApprovalHash(1, HostKey("a", "b", "c", "d"), HostKey("e", "f", "g", "z"), "n"),
		"nonce":      ApprovalHash(1, HostKey("a", "b", "c", "d"), HostKey("e", "f", "g", "h"), "m"),
	}
	for name, h := range cases {
		if h == base {
			t.Errorf("changing %s must change the hash", name)
		}
	}
}
