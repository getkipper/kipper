package installer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParsePortListeners(t *testing.T) {
	out := `LISTEN 0      511          0.0.0.0:80        0.0.0.0:*    users:(("nginx",pid=812,fd=6),("nginx",pid=811,fd=6))
LISTEN 0      511             [::]:443          [::]:*    users:(("nginx",pid=812,fd=8))
LISTEN 0      4096               *:6443            *:*    users:(("k3s-server",pid=1366,fd=12))
LISTEN 0      128        127.0.0.1:8080      0.0.0.0:*
`
	got := parsePortListeners(out)

	assert.Equal(t, map[int][]string{
		80:   {"nginx"},
		443:  {"nginx"},
		6443: {"k3s-server"},
		8080: {""},
	}, got)
}

func TestParsePortListeners_Empty(t *testing.T) {
	assert.Empty(t, parsePortListeners(""))
}

func TestParsePortListeners_IgnoresDiagnostics(t *testing.T) {
	out := `Warning: Permanently added 'host' (ED25519) to the list of known hosts.
LISTEN 0      511          0.0.0.0:443       0.0.0.0:*    users:(("nginx",pid=812,fd=6))
`
	assert.Equal(t, map[int][]string{443: {"nginx"}}, parsePortListeners(out))
}

func TestGatherSystemInfoReadsTheCPUCount(t *testing.T) {
	server := &routedRunner{routes: []route{
		{contains: "cat /etc/os-release", out: "ID=ubuntu\nVERSION_ID=\"24.04\"\n"},
		{contains: "MemTotal", out: "8000000\n"},
		{contains: "df ", out: "36000M 40000M /\n36000M 40000M /\n"},
		{contains: "nproc", out: "2\n"},
	}}
	sys, err := GatherSystemInfo(server)
	assert.NoError(t, err)
	assert.Equal(t, 2, sys.CPUs)
}

func TestGatherSystemInfoRefusesAnUnreadableCPUCount(t *testing.T) {
	server := &routedRunner{routes: []route{
		{contains: "cat /etc/os-release", out: "ID=ubuntu\nVERSION_ID=\"24.04\"\n"},
		{contains: "MemTotal", out: "8000000\n"},
		{contains: "df ", out: "36000M 40000M /\n36000M 40000M /\n"},
		{contains: "nproc", out: "nproc: command not found\n"},
	}}
	_, err := GatherSystemInfo(server)
	assert.ErrorContains(t, err, "detecting CPUs")
}

// Separate data mounts need independent capacity checks.
func TestGatherSystemInfoReadsBothDataFilesystems(t *testing.T) {
	server := &routedRunner{routes: []route{
		{contains: "cat /etc/os-release", out: "ID=ubuntu\nVERSION_ID=\"24.04\"\n"},
		{contains: "MemTotal", out: "8000000\n"},
		{contains: "nproc", out: "2\n"},
		{contains: "df ", out: "36000M 40000M /\n36000M 40000M /\n"},
	}}
	sys, err := GatherSystemInfo(server)
	assert.NoError(t, err)
	assert.Equal(t, 36000, sys.DiskMB)
	assert.Equal(t, 40000, sys.DiskSizeMB)
	assert.False(t, sys.SeparateImageDisk)
	assert.Zero(t, sys.ImagesPresentMB)
	assert.True(t, server.ran("/var/lib/longhorn"), "the Longhorn data path is measured when it exists")
	assert.True(t, server.ran(`r=/var/lib/rancher/k3s; [ -d "$r" ] || r=/var/lib/rancher;`), "and k3s's own data path, which can be a mount of its own")
	assert.True(t, server.ran(`timeout 60 du -sm /var/lib/rancher/k3s/agent/containerd`), "the image store is sized within a time limit, so a re-run pays only for what is missing")
}

func TestParseDiskReport(t *testing.T) {
	shared, err := parseDiskReport("Warning: Permanently added 'host' to the list of known hosts.\n36000M 40000M /\n36000M 40000M /\n")
	assert.NoError(t, err)
	assert.Equal(t, diskReport{free: 36000, size: 40000, imageFree: 36000}, shared)

	separate, err := parseDiskReport("30000M 40000M /var/lib/longhorn\n5000M 20000M /\nimages-present[8700]\n")
	assert.NoError(t, err)
	assert.Equal(t, diskReport{free: 30000, size: 40000, imageFree: 5000, separate: true, imagesPresentMB: 8700}, separate)

	timedOut, err := parseDiskReport("36000M 40000M /\n36000M 40000M /\nimages-present[]\n")
	assert.NoError(t, err)
	assert.Zero(t, timedOut.imagesPresentMB, "an image store that could not be sized in time earns no credit")

	_, err = parseDiskReport("36000M 40000M /\n")
	assert.ErrorContains(t, err, "parsing disk space", "one line is not a report of both filesystems")
}
