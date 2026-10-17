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
		{contains: "df ", out: "40000M\n"},
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
		{contains: "df ", out: "40000M\n"},
		{contains: "nproc", out: "nproc: command not found\n"},
	}}
	_, err := GatherSystemInfo(server)
	assert.ErrorContains(t, err, "detecting CPUs")
}
