package ssh

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSSH installs an ssh stand-in that records its arguments and prints out.
func fakeSSH(t *testing.T, out string, exit int) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + calls + "\nprintf '%s' '" + out + "'\nexit " + strconv.Itoa(exit) + "\n"
	//nolint:gosec // a stub in the test's own temp dir has to be executable
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

// sudo's env_reset drops SSH_CONNECTION, so the address is read by the remote
// shell itself, over a fresh connection pinned to one family.
func TestObservedAddressReadsSSHConnectionOutsideSudo(t *testing.T) {
	calls := fakeSSH(t, "198.51.100.7 51514 203.0.113.10 22", 0)
	c := &Client{cfg: Config{Host: "demo.example.com", User: "ubuntu"}, controlPath: "/tmp/kip-test.sock"}
	addr, err := c.ObservedAddress("4")
	require.NoError(t, err)
	assert.Equal(t, "198.51.100.7", addr)

	b, err := os.ReadFile(calls)
	require.NoError(t, err)
	args := strings.Split(strings.TrimSpace(string(b)), "\n")
	assert.Equal(t, `printf '%s' "$SSH_CONNECTION"`, args[len(args)-1], "the raw command, not wrapped in sudo")
	assert.Equal(t, "ubuntu@demo.example.com", args[len(args)-2])
	assert.Contains(t, args, "-4")
	assert.Contains(t, args, "ControlPath=none", "a fresh connection, not the shared one")
	assert.NotContains(t, string(b), "/tmp/kip-test.sock")
}

func TestObservedAddressPinsIPv6(t *testing.T) {
	calls := fakeSSH(t, "2001:db8:1:2::abcd 51514 2001:db8::10 22", 0)
	c := &Client{cfg: Config{Host: "demo.example.com", User: "root"}}
	addr, err := c.ObservedAddress("6")
	require.NoError(t, err)
	assert.Equal(t, "2001:db8:1:2::abcd", addr)
	b, _ := os.ReadFile(calls)
	assert.Contains(t, strings.Split(string(b), "\n"), "-6")
}

func TestObservedAddressRefusesAnEmptyVariable(t *testing.T) {
	fakeSSH(t, "", 0)
	c := &Client{cfg: Config{Host: "demo.example.com", User: "root"}}
	_, err := c.ObservedAddress("4")
	assert.ErrorContains(t, err, "SSH_CONNECTION")
}

func TestObservedAddressReportsAFailedConnection(t *testing.T) {
	fakeSSH(t, "", 9)
	c := &Client{cfg: Config{Host: "demo.example.com", User: "root"}}
	_, err := c.ObservedAddress("4")
	assert.Error(t, err)
	_, err = c.ObservedAddress("5")
	assert.ErrorContains(t, err, "family")
}
