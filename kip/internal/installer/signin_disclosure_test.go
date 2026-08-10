package installer

import (
	"bytes"
	"strings"
	"testing"
)

// The gate prompts for the admin password, so the password has to be on the
// operator's screen by the time it runs. Asserting that from inside the gate is
// what makes this a test of the ordering rather than of the printing: a version
// that discloses afterwards still produces the same final output, and only the
// moment of observation tells the two apart.
func TestSignInDisclosesCredentialsBeforeTheGateRuns(t *testing.T) {
	const generated = "b3d4f6a8c0e2"

	var out bytes.Buffer
	var seenByGate string
	gate := func() GateResult {
		seenByGate = out.String()
		return GateResult{AuthMode: "oidc"}
	}

	result := signInWithCredentials(&out, "storefront.example", generated, gate)

	if !strings.Contains(seenByGate, generated) {
		t.Errorf("gate ran before the password was printed; it saw:\n%s", seenByGate)
	}
	if !strings.Contains(seenByGate, "admin@storefront.example") {
		t.Errorf("gate ran before the admin email was printed; it saw:\n%s", seenByGate)
	}
	if result.AuthMode != "oidc" {
		t.Errorf("AuthMode = %q, want the gate's own result %q", result.AuthMode, "oidc")
	}
}

// A cluster installed with credentials supplied by the operator generates no
// password, and printing an empty one would tell them to save nothing.
func TestSignInDisclosesNothingWithoutAGeneratedPassword(t *testing.T) {
	var out bytes.Buffer
	gate := func() GateResult { return GateResult{AuthMode: "deferred"} }

	signInWithCredentials(&out, "storefront.example", "", gate)

	if out.Len() != 0 {
		t.Errorf("printed a credential block with no generated password:\n%s", out.String())
	}
}

// The remedy has to name a command that exists. `kip auth reset-password` is
// the only way back once the plaintext is gone, and an operator reads this line
// at the point where they are already stuck.
func TestCredentialDisclosureNamesTheRecoveryCommand(t *testing.T) {
	var out bytes.Buffer
	announceAdminCredentials(&out, "storefront.example", "b3d4f6a8c0e2")

	if !strings.Contains(out.String(), "kip auth reset-password") {
		t.Errorf("credential block does not name the recovery command:\n%s", out.String())
	}
}
