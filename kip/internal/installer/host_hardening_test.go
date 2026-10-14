package installer

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostAuditFindings(t *testing.T) {
	tests := []struct {
		name  string
		audit HostAudit
		want  []string
	}{
		{
			name:  "clean host produces no findings",
			audit: HostAudit{},
			want:  nil,
		},
		{
			name:  "exposed rpcbind is reported",
			audit: HostAudit{RpcbindExposed: true},
			want:  []string{"rpcbind port 111 is listening on all interfaces"},
		},
		{
			name:  "exposed cockpit is reported",
			audit: HostAudit{CockpitExposed: true},
			want:  []string{"Cockpit socket is listening on all interfaces"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.audit.Findings())
		})
	}
}

func TestAuditHostFindsCockpit(t *testing.T) {
	r := &storageHostRunner{replies: map[string]string{
		"is-active cockpit.socket": "active\n",
		"-p Listen":                "[::]:9090 (Stream)\n",
	}}

	audit, err := AuditHost(r)

	assert.NoError(t, err)
	assert.True(t, audit.CockpitExposed)
	assert.False(t, audit.RpcbindExposed)
}

func TestHardenHostDisablesCockpit(t *testing.T) {
	r := &storageHostRunner{replies: map[string]string{
		"is-active cockpit.socket": "active\n",
		"-p Listen":                "9090 (Stream)\n",
	}}

	assert.NoError(t, HardenHost(r))

	all := strings.Join(r.commands, "\n")
	assert.Contains(t, all, "systemctl disable --now cockpit.socket")
	assert.Contains(t, all, "systemctl stop cockpit.service", "a session already started keeps running otherwise")
}

// The socket unit distinguishes Cockpit from other services using port 9090.
func TestCockpitIsOnlyCockpitOnEveryInterface(t *testing.T) {
	otherService := &storageHostRunner{replies: map[string]string{":9090": "tcp LISTEN 0 4096 *:9090 *:*\n"}}
	audit, err := AuditHost(otherService)
	assert.NoError(t, err)
	assert.False(t, audit.CockpitExposed)

	proxied := &storageHostRunner{replies: map[string]string{
		"is-active cockpit.socket": "active\n",
		"-p Listen":                "127.0.0.1:9091 (Stream)\n",
		":9090":                    "tcp LISTEN 0 4096 *:9090 *:*\n",
	}}
	assert.NoError(t, HardenHost(proxied))
	assert.NotContains(t, strings.Join(proxied.commands, "\n"), "disable --now cockpit")
}

func TestHardenHostLeavesAHostWithoutCockpitAlone(t *testing.T) {
	r := &storageHostRunner{}

	assert.NoError(t, HardenHost(r))

	assert.NotContains(t, strings.Join(r.commands, "\n"), "disable --now cockpit")
}

func TestHardenHostReportsAFailedCockpitShutdown(t *testing.T) {
	r := &storageHostRunner{
		replies: map[string]string{"is-active cockpit.socket": "active\n", "-p Listen": "[::]:9090 (Stream)\n"},
		fail:    map[string]error{"disable --now cockpit.socket": errors.New("exit status 1")},
	}

	assert.ErrorContains(t, HardenHost(r), "disabling cockpit.socket")
}

func TestListensOnEveryInterface(t *testing.T) {
	for listen, want := range map[string]bool{
		"[::]:9090 (Stream)":                           true,
		"9090 (Stream)":                                true,
		"0.0.0.0:9090 (Stream)":                        true,
		"127.0.0.1:9091 (Stream)":                      false,
		"[::1]:9090 (Stream)\n127.0.0.1:9090 (Stream)": false,
		"":                                  false,
		"/run/cockpit/listen.sock (Stream)": false,
		"@cockpit (Stream)":                 false,
	} {
		assert.Equal(t, want, listensOnEveryInterface(listen), listen)
	}
}

// A real shell verifies failure propagation through the compound command.
func TestCockpitShutdownCommandFailsWhenSystemctlDoes(t *testing.T) {
	for _, failing := range []string{"disable", "stop"} {
		bin := t.TempDir()
		//nolint:gosec // the fake command has to be executable
		require.NoError(t, os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\ncase \"$1\" in "+failing+") exit 1 ;; esac\nexit 0\n"), 0o755))
		cmd := exec.Command("sh", "-c", cockpitShutdownCommand)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		assert.Error(t, cmd.Run(), failing)
	}
}

func TestHardeningRunsRightAfterTheStoragePackages(t *testing.T) {
	r := &storageHostRunner{
		replies: map[string]string{"list-unit-files rpcbind.socket": "rpcbind.socket\n"},
		fail:    map[string]error{"kubectl apply": errors.New("the API server went away")},
	}

	err := installLonghorn(r, debianHost, hardenAfterStoragePackages(true, r))

	require.Error(t, err, "the manifest step fails")
	all := strings.Join(r.commands, "\n")
	assert.Contains(t, all, "systemctl mask rpcbind.socket", "hardening already ran")
	assert.Less(t, strings.Index(all, "mask rpcbind.socket"), strings.Index(all, "kubectl apply"))
	assert.Nil(t, hardenAfterStoragePackages(false, r), "--harden=false leaves services alone")
}
