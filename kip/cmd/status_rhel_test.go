package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/k8s"
)

// TestStorageRestartCoverageChecksRHELNodes verifies coverage of NFS client
// and image cleanup settings alongside the notice about package-triggered restarts.
func TestStorageRestartCoverageChecksRHELNodes(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	rocky := k8s.NodeInfo{Name: "worker-1", OSImage: "Rocky Linux 10.0 (Red Quartz)", MachineID: id}

	out := captureStdout(t, func() { reportStorageRestartCoverage([]k8s.NodeInfo{rocky}) })

	assert.Contains(t, out, "1 node(s) run a distribution without needrestart")
	assert.Contains(t, out, "worker-1 (no configuration record)")
	assert.Contains(t, out, "repair-host")

	rocky.Annotations = map[string]string{
		installer.StorageRestartsVersionAnnotation: installer.StorageRestartConfigVersion,
		installer.StorageRestartsMachineAnnotation: id,
	}
	out = captureStdout(t, func() { reportStorageRestartCoverage([]k8s.NodeInfo{rocky}) })
	assert.Contains(t, out, "✔  host configuration is current on every node")
}

func TestStorageRestartCoverageStillChecksDebianNodesBesideRHEL(t *testing.T) {
	rocky := k8s.NodeInfo{Name: "worker-1", OSImage: "Rocky Linux 10.0 (Red Quartz)"}
	debian := k8s.NodeInfo{Name: "server", OSImage: "Debian GNU/Linux 13 (trixie)", MachineID: "0123456789abcdef0123456789abcdef"}

	out := captureStdout(t, func() { reportStorageRestartCoverage([]k8s.NodeInfo{rocky, debian}) })

	assert.Contains(t, out, "server (no configuration record)")
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

func TestHostConfigurationStatusCoversEveryHostSetting(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	current := k8s.NodeInfo{Name: "server", OSImage: "Ubuntu 24.04.2 LTS", MachineID: id, Annotations: map[string]string{
		installer.StorageRestartsVersionAnnotation: installer.StorageRestartConfigVersion,
		installer.StorageRestartsMachineAnnotation: id,
	}}
	older := k8s.NodeInfo{Name: "worker-1", OSImage: "Ubuntu 24.04.2 LTS", MachineID: id, Annotations: map[string]string{
		installer.StorageRestartsVersionAnnotation: "1",
		installer.StorageRestartsMachineAnnotation: id,
	}}

	out := captureStdout(t, func() { reportStorageRestartCoverage([]k8s.NodeInfo{current}) })
	assert.Contains(t, out, "✔  host configuration is current on every node")

	out = captureStdout(t, func() { reportStorageRestartCoverage([]k8s.NodeInfo{current, older}) })
	assert.Contains(t, out, "worker-1 (configured with an older version)")
	assert.Contains(t, out, "image cleanup")
	assert.Contains(t, out, "kip node repair-host")
}
