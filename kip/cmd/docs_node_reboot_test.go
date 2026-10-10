package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCLIReferenceDocumentsNodeReboot(t *testing.T) {
	section := docSection(t, "cli-reference.md", "## kip node reboot")
	for _, name := range localFlagNames(nodeRebootCmd) {
		assert.Contains(t, section, "`"+name+"`", "the kip node reboot reference does not document %s", name)
	}
	for _, want := range []string{"backup", "boot", "Pending restarts"} {
		assert.Contains(t, section, want)
	}
}

func TestMaintenanceLinksToNodeReboot(t *testing.T) {
	assert.Contains(t, docSection(t, "maintenance.md", "## Host maintenance and recovery"), "/en/cli-reference#kip-node-reboot")
}
