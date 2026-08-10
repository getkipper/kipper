package cmd

import (
	"strings"
	"testing"
)

// A container that has no such program is not a link that is shut. Reading one
// as the other is how a working link gets reported as broken — which is exactly
// what happened while proving the first cross-project link on a cluster: three
// probes said no before one said yes.
func TestAMissingToolIsNotAClosedLink(t *testing.T) {
	missing := []string{
		"sh: curl: not found",
		"OCI runtime exec failed: exec: \"curl\": executable file not found in $PATH",
		"/bin/sh: 1: nc: no such file",
	}
	for _, out := range missing {
		if !toolMissing(out) {
			t.Errorf("toolMissing(%q) = false; a container without the tool must not read as a refused connection", out)
		}
	}

	realFailures := []string{
		"",
		"nc: connect: Connection refused",
		"wget: download timed out",
		"curl: (28) Connection timed out after 5001 milliseconds",
	}
	for _, out := range realFailures {
		if toolMissing(out) {
			t.Errorf("toolMissing(%q) = true; a connection that genuinely failed must be reported as one", out)
		}
	}
}

// The two outcomes have to read differently, and both have to name where the
// attempt was made from — the allowance applies to the calling pod, so a result
// without that is not evidence about anything.
func TestAProbeSaysWhatItProvedAndFromWhere(t *testing.T) {
	ok := probeResult("nc", true, "docuseal.docuseal-test.svc.cluster.local", 3000, "hrportal-backend-abc")
	if !strings.HasPrefix(ok, "reachable") {
		t.Errorf("a successful probe reads %q; the caller keys off the leading word", ok)
	}
	for _, want := range []string{"nc", "docuseal.docuseal-test.svc.cluster.local:3000", "hrportal-backend-abc"} {
		if !strings.Contains(ok, want) {
			t.Errorf("a successful probe omits %q: %s", want, ok)
		}
	}

	bad := probeResult("nc", false, "docuseal.docuseal-test.svc.cluster.local", 3000, "hrportal-backend-abc")
	if strings.HasPrefix(bad, "reachable") {
		t.Errorf("a failed probe must not read as reachable: %s", bad)
	}
	if !strings.Contains(bad, "hrportal-backend-abc") {
		t.Errorf("a failed probe omits where it was tried from: %s", bad)
	}
}

// One note cannot describe both outcomes. It did, and the report said the
// address was "not yet in the running pod" for a pod that had it — the check
// passed and the wording came from the failure case.
func TestAnOutcomeIsWordedByWhatHappened(t *testing.T) {
	if got := either(true, "in the running pod", "not in the running pod yet"); got != "in the running pod" {
		t.Errorf("a passing check reported %q", got)
	}
	if got := either(false, "in the running pod", "not in the running pod yet"); got != "not in the running pod yet" {
		t.Errorf("a failing check reported %q", got)
	}
}
