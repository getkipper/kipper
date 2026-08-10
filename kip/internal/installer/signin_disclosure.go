package installer

import (
	"fmt"
	"io"
)

// announceAdminCredentials prints the admin account this install generated.
//
// The password is generated in memory and only its bcrypt hash reaches Dex, so
// this is the one moment it can be read. `kip auth reset-password` is named
// because it is the only way back afterwards, and the operator who needs it is
// already locked out when they read this.
func announceAdminCredentials(out io.Writer, domain, password string) {
	if password == "" {
		return
	}
	// A failed write to a terminal is not actionable, and this runs at the end
	// of an install that has already succeeded.
	_, _ = fmt.Fprintf(out, "  Admin sign-in\n")
	_, _ = fmt.Fprintf(out, "  Email:      admin@%s\n", domain)
	_, _ = fmt.Fprintf(out, "  Password:   %s\n\n", password)
	_, _ = fmt.Fprintf(out, "  Save these credentials now. They will not be shown again.\n")
	_, _ = fmt.Fprintf(out, "  If lost, run: kip auth reset-password\n\n")
}

// signInWithCredentials discloses the admin credentials and then runs the
// sign-in gate.
//
// The order is the contract rather than a detail of the output. The gate opens
// a browser asking for exactly the password disclosed here, so running them the
// other way round asks a first install to authenticate with something it has
// never shown. The end-of-install summary, which is where the credentials used
// to appear, is not reached until the gate has given up.
func signInWithCredentials(out io.Writer, domain, password string, gate func() GateResult) GateResult {
	announceAdminCredentials(out, domain, password)
	return gate()
}
