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

func TestRebootAndNetworkDocsDescribeMeasuredImpact(t *testing.T) {
	reboot := docSection(t, "cli-reference.md", "## kip node reboot")
	assert.Contains(t, reboot, "Longhorn rebuilds degraded volumes on its own")
	assert.NotContains(t, reboot, "risks volume data")

	upgrade := docSection(t, "maintenance.md", "## kip upgrade")
	assert.Contains(t, upgrade, "The restart leaves running containers alone")
	assert.NotContains(t, upgrade, "may disrupt pod traffic")
	assert.Contains(t, upgrade, "(usually 10 to 30 seconds, longer on small or busy servers), and the whole change takes about a minute")

	install := docSection(t, "installation.md", "## kip install")
	assert.Contains(t, install, "apps that call the API are affected for that time")
	assert.Contains(t, install, "(usually 10 to 30 seconds, longer on small or busy servers)")
	assert.NotContains(t, install, "potentially pod traffic")
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

func TestRebootDocsNameSkipBackupForAnUnreadableNodeList(t *testing.T) {
	reboot := docSection(t, "cli-reference.md", "## kip node reboot")
	assert.Contains(t, reboot, "When kip cannot read the node list")
	assert.NotContains(t, reboot, "If the node list is unavailable, pass `--ignore-degraded-volumes`")
}

func TestNodeAddDocsSayTheDataStaysOnTheServer(t *testing.T) {
	nodeAdd := docSection(t, "cli-reference.md", "## kip node add")
	assert.Contains(t, nodeAdd, "Kipper configures each database, registry and in-cluster backup volume to keep one copy on the server")
	assert.NotContains(t, nodeAdd, "the registry uses one replica with single-replica storage")
	assert.Contains(t, nodeAdd, "Otherwise the reboot names the volume and stops before cordoning the worker")
	assert.Contains(t, nodeAdd, "or capacity changes after that check, the drain blocks until `--drain-timeout`, then fails, and kip cancels the reboot")
}

func TestSecurityDocsDescribeTheUFWRollback(t *testing.T) {
	assert.Contains(t, docSection(t, "security.md", "## What Kipper provides out of the box"), "saves its configuration and active state, and schedules a rollback on the host")
}
