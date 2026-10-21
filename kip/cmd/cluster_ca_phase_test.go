package cmd

import (
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
