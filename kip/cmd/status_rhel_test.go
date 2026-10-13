package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/k8s"
)

func TestStorageRestartCoverageLeavesRHELNodesOut(t *testing.T) {
	rocky := k8s.NodeInfo{Name: "worker-1", OSImage: "Rocky Linux 10.0 (Red Quartz)", MachineID: "0123456789abcdef0123456789abcdef"}

	out := captureStdout(t, func() { reportStorageRestartCoverage([]k8s.NodeInfo{rocky}) })

	assert.Contains(t, out, "1 node(s) run a distribution without needrestart")
	assert.NotContains(t, out, "repair-host")
	assert.NotContains(t, out, "needrestart leaves the storage path alone")
}

func TestStorageRestartCoverageStillChecksDebianNodesBesideRHEL(t *testing.T) {
	rocky := k8s.NodeInfo{Name: "worker-1", OSImage: "Rocky Linux 10.0 (Red Quartz)"}
	debian := k8s.NodeInfo{Name: "server", OSImage: "Debian GNU/Linux 13 (trixie)", MachineID: "0123456789abcdef0123456789abcdef"}

	out := captureStdout(t, func() { reportStorageRestartCoverage([]k8s.NodeInfo{rocky, debian}) })

	assert.Contains(t, out, "server (never configured)")
	assert.Contains(t, out, "repair-host")
}

func TestNeedrestartApplies(t *testing.T) {
	for image, want := range map[string]bool{
		"Ubuntu 24.04.2 LTS":                  true,
		"Debian GNU/Linux 13 (trixie)":        true,
		"Rocky Linux 9.6 (Blue Onyx)":         false,
		"AlmaLinux 10.0 (Purple Lion)":        false,
		"CentOS Stream 10 (Coughlan)":         false,
		"Red Hat Enterprise Linux 9.6 (Plow)": false,
		"":                                    true,
	} {
		assert.Equal(t, want, installer.NeedrestartApplies(image), image)
	}
}
