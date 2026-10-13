package installer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCheckOSAcceptsTheRHELFamilyByMajorVersion(t *testing.T) {
	for _, tc := range []struct {
		id, version string
		ok          bool
	}{
		{"rocky", "9.6", true},
		{"rocky", "10.0", true},
		{"almalinux", "9.6", true},
		{"almalinux", "10.0", true},
		{"centos", "9", true},
		{"centos", "10", true},
		{"rocky", "8.10", false},
		{"almalinux", "8.10", false},
		{"centos", "8", false},
		{"centos", "7", false},
		{"rhel", "9.6", false},
		{"fedora", "44", false},
		// Debian-family versions still match exactly.
		{"debian", "12.1", false},
	} {
		err := checkOS(SystemInfo{OS: tc.id, OSVersion: tc.version})
		if tc.ok {
			assert.NoError(t, err, "%s %s", tc.id, tc.version)
		} else {
			assert.Error(t, err, "%s %s", tc.id, tc.version)
		}
	}
}

func TestRHELFamilyReleasesCarryNoSupportWarning(t *testing.T) {
	assert.Empty(t, pastStandardSupportWarning(SystemInfo{OS: "rocky", OSVersion: "9.6"}))
}
