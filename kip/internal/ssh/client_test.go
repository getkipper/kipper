package ssh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSSHArgsDefaults checks that a minimal Config produces an arg list
// containing the standard safety options and a root@host target.
func TestSSHArgsDefaults(t *testing.T) {
	c := &Client{cfg: Config{Host: "203.0.113.10"}}
	args := c.sshArgs()

	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "-o StrictHostKeyChecking=accept-new")
	assert.Contains(t, joined, "-o ConnectTimeout=10")
	assert.Equal(t, "root@203.0.113.10", args[len(args)-1])
}

// TestSSHArgsKeyPathAddsIdentitiesOnly confirms the IdentitiesOnly
// option fires whenever an explicit key is set, mirroring OpenSSH's
// own convention for `ssh -i`.
func TestSSHArgsKeyPathAddsIdentitiesOnly(t *testing.T) {
	c := &Client{cfg: Config{Host: "203.0.113.10", KeyPath: "/path/to/key"}}
	args := c.sshArgs()

	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "-i /path/to/key")
	assert.Contains(t, joined, "-o IdentitiesOnly=yes")
}

// TestSSHArgsCustomUserAndPort confirms non-default user and port flow
// through to the right ssh flags.
func TestSSHArgsCustomUserAndPort(t *testing.T) {
	c := &Client{cfg: Config{Host: "203.0.113.10", User: "ubuntu", Port: "2222"}}
	args := c.sshArgs()

	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "-p 2222")
	assert.Equal(t, "ubuntu@203.0.113.10", args[len(args)-1])
}

// TestSSHArgsDefaultPortOmitted: explicit port 22 should not appear in
// the args (it's the default; cluttering the command line is noise).
func TestSSHArgsDefaultPortOmitted(t *testing.T) {
	c := &Client{cfg: Config{Host: "203.0.113.10", Port: "22"}}
	args := c.sshArgs()

	for _, a := range args {
		assert.NotEqual(t, "-p", a, "default port 22 should not be passed explicitly")
	}
}

// TestSSHArgsExtraOptions threads arbitrary -o options through.
func TestSSHArgsExtraOptions(t *testing.T) {
	c := &Client{cfg: Config{
		Host:    "203.0.113.10",
		Options: []string{"ProxyJump=bastion.example.com", "ServerAliveInterval=30"},
	}}
	args := c.sshArgs()

	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "-o ProxyJump=bastion.example.com")
	assert.Contains(t, joined, "-o ServerAliveInterval=30")
}

// TestSSHArgsFallbackKeyDoesNotForceIdentitiesOnly is the regression
// test for a real bug where users with ssh-agent loaded keys were
// forced to enter passwords because kip defaulted KeyPath to
// `~/.ssh/id_ed25519` and added IdentitiesOnly=yes. A fallback key
// must be a soft hint only, never a forced exclusion.
func TestSSHArgsFallbackKeyDoesNotForceIdentitiesOnly(t *testing.T) {
	tmp := t.TempDir()
	keyPath := filepath.Join(tmp, "fallback")
	if err := os.WriteFile(keyPath, []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &Client{cfg: Config{Host: "203.0.113.10", FallbackKeyPath: keyPath}}
	args := c.sshArgs()

	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "-i "+keyPath, "fallback key should be hinted to ssh")
	assert.NotContains(t, joined, "IdentitiesOnly=yes", "fallback must not force IdentitiesOnly — agent must still be consulted")
}

// TestSSHArgsFallbackKeyIgnoredWhenMissing: when the fallback key file
// does not exist, kip must not pass `-i` at all so OpenSSH falls
// through cleanly to ssh-agent and ~/.ssh/config.
func TestSSHArgsFallbackKeyIgnoredWhenMissing(t *testing.T) {
	c := &Client{cfg: Config{
		Host:            "203.0.113.10",
		FallbackKeyPath: "/definitely/does/not/exist",
	}}
	args := c.sshArgs()

	for _, a := range args {
		assert.NotEqual(t, "-i", a, "missing fallback must not produce a stale -i flag")
	}
}

// TestSSHArgsExplicitKeyTrumpsFallback: when both KeyPath and
// FallbackKeyPath are set, the explicit one wins and IdentitiesOnly
// fires.
func TestSSHArgsExplicitKeyTrumpsFallback(t *testing.T) {
	c := &Client{cfg: Config{
		Host:            "203.0.113.10",
		KeyPath:         "/explicit/key",
		FallbackKeyPath: "/some/fallback",
	}}
	args := c.sshArgs()

	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "-i /explicit/key")
	assert.Contains(t, joined, "IdentitiesOnly=yes")
	assert.NotContains(t, joined, "/some/fallback", "fallback must not appear when an explicit key is set")
}
