package cmd

import (
	"strings"
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

func TestWorkerRejoinIsDocumentedWhereAnAdminLooksForIt(t *testing.T) {
	const symptom = "failed to retrieve configuration from server: not authorized"
	nodeAdd := docSection(t, "cli-reference.md", "## kip node add")
	assert.Contains(t, nodeAdd, symptom)
	assert.Contains(t, nodeAdd, "certificates have expired or gone missing")
	assert.Contains(t, nodeAdd, "keeps the node's name and the data on its disks")
	assert.Contains(t, nodeAdd, "renews them at each agent start while they are still valid")

	recovery := docSection(t, "maintenance.md", "## Host maintenance and recovery")
	assert.Contains(t, recovery, "[Worker rejoin](/en/cli-reference#kip-node-add)")

	assert.Contains(t, strings.Join(strings.Fields(nodeAddCmd.Long), " "), "rejoins one whose agent certificates are missing or expired.")
}

func TestNodeAddDocsSayTheWorkerFollowsTheServersFirewall(t *testing.T) {
	nodeAdd := docSection(t, "cli-reference.md", "## kip node add")
	assert.Contains(t, nodeAdd, "`--firewall`")
	assert.Contains(t, nodeAdd, "kip sets up a host firewall on a new node only when it manages the server's firewall")
	assert.NotContains(t, nodeAdd, "Existing nodes without a host firewall keep that configuration")
	assert.Contains(t, nodeAdd, "A UFW firewall that kip does not manage on the server or another existing node stops the join")
	assert.Contains(t, docSection(t, "security.md", "## What Kipper provides out of the box"), "A UFW firewall that kip does not manage on the server or another existing node stops the join")

	assert.Contains(t, docSection(t, "security.md", "## What Kipper provides out of the box"), "`kip node add` inherits the server's kip-managed firewall setting by default.")
}

func TestSecurityDocsDescribeTheUFWRollback(t *testing.T) {
	assert.Contains(t, docSection(t, "security.md", "## What Kipper provides out of the box"), "saves its configuration and active state, and schedules a rollback on the host")
}
