package cmd

import "testing"

// This covers what the CLI reports as its version, and nothing more. It does
// NOT prove the install path passes it on: deleting KipVersion from the Options
// literal in runInstall leaves this test green, which was checked rather than
// assumed. Extracting that literal to make it testable is disproportionate to
// what it would buy, so the protection lives in installer.Run instead, which
// refuses an install carrying no version before any side effect. Read this test
// as pinning the source of the value, and TestRunRefusesToInstallWithoutAKipVersion
// as pinning that something supplies one.
func TestInstallRecordsTheRunningKipVersionOnTheCRDsItWrites(t *testing.T) {
	original := rootCmd.Version
	t.Cleanup(func() { rootCmd.Version = original })

	rootCmd.Version = "v0.11.0"
	if got := installingKipVersion(); got != "v0.11.0" {
		t.Errorf("install must record the running version, got %q", got)
	}

	// And it must pass through whatever the binary reports, including a value
	// the guard cannot order, rather than substituting something orderable.
	rootCmd.Version = "dev"
	if got := installingKipVersion(); got != "dev" {
		t.Errorf("install must not invent a version, got %q", got)
	}
}
