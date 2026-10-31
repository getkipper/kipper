package cmd

import (
	"slices"
	"strings"
	"testing"

	"github.com/getkipper/kipper/kip/internal/installer"
)

func TestTheResumeNoteDescribesWhatThePhaseTrusts(t *testing.T) {
	for phase, want := range map[installer.CAPhase]string{
		installer.CAPhaseStaged:   "the new authority is stored but not trusted yet",
		installer.CAPhaseExpanded: "the cluster trusts one authority more than it strictly needs to",
		installer.CAPhasePromoted: "the cluster trusts one authority more than it strictly needs to",
		installer.CAPhaseNarrowed: "the old authority is still stored but has been removed from the anchor file",
	} {
		if got := caPhaseNote(phase); !strings.Contains(got, want) {
			t.Errorf("%s: got %q, want it to contain %q", phase, got, want)
		}
	}
}

func TestTheCAWarningSaysOnlyWhatWasChecked(t *testing.T) {
	gateway := []string{"login--acme.kipper.run"}
	custom := []string{"login.example.com"}
	outage := []string{"login is failing", "logins are failing", "would break them", "logins are working", "logins work for now", "logins are unaffected", "safe state", "affects nothing", "fails only once", "still trusted", "the current authority signed"}
	without := func(keep ...string) []string {
		var out []string
		for _, claim := range outage {
			if !slices.Contains(keep, claim) {
				out = append(out, claim)
			}
		}
		return out
	}
	cases := []struct {
		name    string
		status  installer.CAStatus
		want    string
		notWant []string
	}{
		{"loaded", installer.CAStatus{Hosts: gateway, AnchorUsed: true, AnchorLoaded: true}, "kipper.run logins fail whenever the gateway", outage},
		{"loaded, served by another authority", installer.CAStatus{Hosts: gateway, AnchorUsed: true, AnchorLoaded: true, ServedByActive: new(bool)}, "kipper.run logins fail whenever the gateway", outage},
		{"unanswered", installer.CAStatus{Hosts: gateway, AnchorUsed: true, AnchorLoadedUnknown: true}, "if it has loaded that anchor,", outage},
		{"not loaded", installer.CAStatus{Hosts: gateway, AnchorUsed: true}, "cannot be told from here. syncing the anchor", outage},
		{"custom-domain issuer only, loaded", installer.CAStatus{Hosts: custom, AnchorLoaded: true}, "logins are unaffected", without("logins are unaffected")},
		{"custom-domain issuer only, not loaded", installer.CAStatus{Hosts: custom}, "has not loaded that config, so which one it runs", outage},
		{"custom-domain issuer only, unanswered", installer.CAStatus{Hosts: custom, AnchorLoadedUnknown: true}, "which config it runs is unknown", outage},
		{"covered, custom-domain issuer only, unanswered", installer.CAStatus{Hosts: custom, AnchorCovers: true, AnchorLoadedUnknown: true}, "whether it has loaded the login config on", outage},
		{"covered, not loaded", installer.CAStatus{Hosts: gateway, AnchorUsed: true, AnchorCovers: true}, "kipper.run logins cannot be told from here", outage},
		{"covered, unanswered, replacing an authority", installer.CAStatus{Hosts: gateway, AnchorUsed: true, AnchorCovers: true, AnchorLoadedUnknown: true, Outgoing: &installer.CertSummary{Subject: "old"}}, "still stored", outage},
		{"covered and loaded, served cert fails to verify", installer.CAStatus{Hosts: gateway, AnchorUsed: true, AnchorCovers: true, AnchorLoaded: true, TrustedByAPIServer: true, ServedByActive: new(bool)}, "does not verify against the current", outage},
		{"covered, custom-domain issuer only, not loaded", installer.CAStatus{Hosts: custom, AnchorCovers: true}, "the config on disk has only custom-domain", outage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.status.Phase = installer.CAPhaseExpanded
			c.status.Resume = "1.4"
			out := strings.ToLower(captureStdout(t, func() { printCAStatus("test", c.status) }))
			if !c.status.AnchorCovers && !strings.Contains(out, "does not cover") {
				t.Errorf("the warning does not name the uncovered anchor:\n%s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("want %q in:\n%s", c.want, out)
			}
			for _, claim := range c.notWant {
				if strings.Contains(out, claim) {
					t.Errorf("the output claims %q, which was not checked:\n%s", claim, out)
				}
			}
		})
	}
}

func TestAReplacementIsCalledSafeOnlyWhenItsChecksPass(t *testing.T) {
	verified := true
	cases := []struct {
		name   string
		served *bool
		want   string
		safe   bool
	}{
		{"served cert unchecked", nil, "This is a safe state to sit in", true},
		{"served cert verifies", &verified, "This is a safe state to sit in", true},
		// After promotion and before certificate installation, the gateway serves
		// the outgoing authority's certificate. A failed check cannot confirm this cause.
		{"served cert fails to verify", new(bool), "Before step 2.3 the gateway serves", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := captureStdout(t, func() {
				printCAStatus("test", installer.CAStatus{
					Phase: installer.CAPhasePromoted, Resume: "2.3", Hosts: []string{"login--acme.kipper.run"},
					AnchorUsed: true, AnchorCovers: true, AnchorLoaded: true, TrustedByAPIServer: true, ServedByActive: c.served,
				})
			})
			if !strings.Contains(out, c.want) {
				t.Errorf("want %q in:\n%s", c.want, out)
			}
			if got := strings.Contains(out, "safe state"); got != c.safe {
				t.Errorf("called safe = %v, want %v:\n%s", got, c.safe, out)
			}
		})
	}
}

func TestACustomDomainReplacementClaimsNoTrustInTheAnchor(t *testing.T) {
	out := captureStdout(t, func() {
		printCAStatus("test", installer.CAStatus{
			Phase: installer.CAPhaseExpanded, Resume: "1.6", Hosts: []string{"login.example.com"},
			AnchorCovers: true, AnchorLoaded: true, TrustedByAPIServer: true,
			Leaf: installer.CertSummary{Subject: "kipper-hop"},
		})
	})
	if !strings.Contains(out, "No login issuer uses the anchor") || strings.Contains(out, "trusts one authority more") {
		t.Errorf("a custom-domain cluster is said to trust the anchor's authorities:\n%s", out)
	}
	if !strings.Contains(out, "Stored cert") || strings.Contains(out, "Served cert") {
		t.Errorf("the certificate read from the Secret is labeled as served:\n%s", out)
	}
}

func TestTheCAStatusSaysWhenTheLoadedConfigUsesNoAnchor(t *testing.T) {
	out := captureStdout(t, func() {
		printCAStatus("test", installer.CAStatus{Phase: installer.CAPhaseExpanded, Hosts: []string{"login.example.com"}, AnchorCovers: true, AnchorLoaded: true})
	})
	if !strings.Contains(out, "loaded its login config, which uses no anchor") || strings.Contains(out, "has loaded that anchor") {
		t.Errorf("a custom-domain login config is reported as loading the anchor:\n%s", out)
	}
}

func TestTheCAStatusSaysSoWhenNoLoginIssuerIsConfigured(t *testing.T) {
	out := captureStdout(t, func() {
		printCAStatus("test", installer.CAStatus{Problems: []string{"no issuer"}})
	})
	if !strings.Contains(out, "No login issuer is configured") || strings.Contains(out, "has loaded that anchor") {
		t.Errorf("with no issuer the status claims something about a load nobody checked:\n%s", out)
	}
}
