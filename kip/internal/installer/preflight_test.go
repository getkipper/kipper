package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRunPreflightChecks(t *testing.T) {
	tests := []struct {
		name                string
		system              SystemInfo
		wantError           string
		wantWarningContains string
	}{
		{
			name: "passes with sufficient resources on ubuntu 22.04",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 40960,
				Ports: []int{80, 443, 6443},
			},
		},
		{
			name: "passes with sufficient resources on debian 12",
			system: SystemInfo{
				OS: "debian", OSVersion: "12",
				RAMMB: 8192, DiskMB: 40960,
				Ports: []int{80, 443, 6443, 8080},
			},
		},
		{
			name: "passes at exact 2048 MB floor with nano warning",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 2048, DiskMB: 40960,
				Ports: []int{80, 443, 6443},
			},
			wantWarningContains: "'nano' profile",
		},
		{
			name: "warns nano-style in the 3 GB range",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 3000, DiskMB: 40960,
				Ports: []int{80, 443, 6443},
			},
			wantWarningContains: "monitoring (Prometheus, Loki, Grafana) is disabled by default",
		},
		{
			name: "warns small-style for a 4 GB host",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "20.04",
				RAMMB: 4096, DiskMB: 40960,
				Ports: []int{80, 443, 6443},
			},
			wantWarningContains: "'small' profile",
		},
		{
			name: "fails with unsupported OS",
			system: SystemInfo{
				OS: "centos", OSVersion: "9",
				RAMMB: 8192, DiskMB: 40960,
				Ports: []int{80, 443, 6443},
			},
			wantError: "unsupported OS: centos (supported: ubuntu, debian)",
		},
		{
			name: "fails with unsupported OS version",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "18.04",
				RAMMB: 8192, DiskMB: 40960,
				Ports: []int{80, 443, 6443},
			},
			wantError: "unsupported ubuntu version: 18.04 (supported: 20.04, 22.04, 24.04, 26.04)",
		},
		{
			name: "accepts ubuntu 26.04",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "26.04",
				RAMMB: 8192, DiskMB: 40960,
				Ports: []int{80, 443, 6443},
			},
		},
		{
			name: "fails when RAM is below the 2048 MB floor",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 1024, DiskMB: 40960,
				Ports: []int{80, 443, 6443},
			},
			wantError: "insufficient RAM: 1024MB available, 2048MB required",
		},
		{
			name: "fails with insufficient disk",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 20480,
				Ports: []int{80, 443, 6443},
			},
			wantError: "insufficient disk: 20480MB available, 30720MB required",
		},
		{
			name: "fails with missing single port",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 40960,
				Ports: []int{80, 443},
			},
			wantError: "required ports unavailable: 6443",
		},
		{
			name: "fails with multiple missing ports",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 40960,
				Ports: []int{6443},
			},
			wantError: "required ports unavailable: 80, 443",
		},
		{
			name: "no warning at exactly the recommended floor",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 8192, DiskMB: 40960,
				Ports: []int{80, 443, 6443},
			},
		},
		{
			name: "no warning on a 64 GB cluster node",
			system: SystemInfo{
				OS: "ubuntu", OSVersion: "22.04",
				RAMMB: 64253, DiskMB: 1024000,
				Ports: []int{80, 443, 6443},
			},
		},
		{
			name: "reports first failing check when multiple fail",
			system: SystemInfo{
				OS: "windows", OSVersion: "11",
				RAMMB: 512, DiskMB: 1024,
				Ports: []int{},
			},
			wantError: "unsupported OS: windows (supported: ubuntu, debian)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := RunPreflightChecks(tt.system)
			if tt.wantError != "" {
				assert.EqualError(t, err, tt.wantError)
				return
			}
			assert.NoError(t, err)
			if tt.wantWarningContains == "" {
				assert.Empty(t, result.Warnings, "expected no warnings")
				return
			}
			found := false
			for _, w := range result.Warnings {
				if strings.Contains(w, tt.wantWarningContains) {
					found = true
					break
				}
			}
			assert.Truef(t, found, "expected warning containing %q, got %v",
				tt.wantWarningContains, result.Warnings)
		})
	}
}
