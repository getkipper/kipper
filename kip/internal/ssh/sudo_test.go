package ssh

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteCommandLeavesRootCommandsAlone(t *testing.T) {
	for _, user := range []string{"", "root"} {
		c := &Client{cfg: Config{Host: "203.0.113.10", User: user}}
		assert.Equal(t, "kubectl get nodes", c.remoteCommand("kubectl get nodes"))
	}
}

// TestRemoteCommandRunsTheSameCommandThroughSudo checks that stripping sudo
// options preserves the command's quotes, variables and newlines.
func TestRemoteCommandRunsTheSameCommandThroughSudo(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "sudo")
	// Simulate sudo's hostname resolution warning to test stderr filtering.
	//nolint:gosec // The stub must be executable to run from the test's temporary directory.
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\necho 'sudo: unable to resolve host demo' >&2\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	command := "x='it'\"'\"'s'; printf '%s|%s\\n' \"$x\" \"$(echo a b)\"\necho second line; command -v sh >/dev/null && echo path-ok; echo to-stderr >&2"
	direct, err := exec.Command("sh", "-c", command).CombinedOutput()
	require.NoError(t, err)

	c := &Client{cfg: Config{Host: "203.0.113.10", User: "ubuntu"}}
	wrapped := c.remoteCommand(command)
	assert.Contains(t, wrapped, "sudo -n -H -- ")
	viaSudo, err := exec.Command("sh", "-c", wrapped).CombinedOutput() //nolint:gosec // The test runs its own wrapper against stubs in its temporary directory.
	require.NoError(t, err)
	assert.Equal(t, string(direct), string(viaSudo), "the command's own stderr stays and sudo's warning goes")
	assert.Contains(t, wrapped, "/usr/local/bin", "k3s and kubectl live there, outside RHEL's sudo secure_path")
}

func TestDialExplainsAMissingPasswordlessSudo(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "ssh")
	//nolint:gosec // The stub must be executable to run from the test's temporary directory.
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\necho 'sudo: a password is required' >&2\nexit 1\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := Dial(Config{Host: "203.0.113.10", User: "ubuntu"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ubuntu needs passwordless sudo")
}

func TestDialShowsAnyOtherSudoRefusal(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "ssh")
	//nolint:gosec // The stub must be executable to run from the test's temporary directory.
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\necho \"$@\" > "+filepath.Join(dir, "args")+"\necho 'sudo: sorry, you must have a tty to run sudo' >&2\nexit 1\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := Dial(Config{Host: "203.0.113.10", User: "ubuntu"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ubuntu needs passwordless sudo")
	assert.Contains(t, err.Error(), "you must have a tty", "sudo's own words reach the operator")
	args, _ := os.ReadFile(filepath.Join(dir, "args")) //nolint:gosec // The file is in the test's temporary directory.
	assert.Contains(t, string(args), sudoProbe, "the probe is not wrapped, so its stderr is kept")
}

func TestDialKeepsTheOutputOfAFailedRootProbe(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "ssh")
	//nolint:gosec // The stub must be executable to run from the test's temporary directory.
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\necho 'Please login as the user \"ubuntu\" rather than the user \"root\".'\nexit 1\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := Dial(Config{Host: "203.0.113.10", User: "root"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--ssh-user ubuntu", "the cloud image names the user to log in as")
}

func TestRemoteStreamCommandKeepsStderrApart(t *testing.T) {
	c := &Client{cfg: Config{Host: "203.0.113.10", User: "ubuntu"}}
	wrapped := c.remoteStreamCommand("journalctl -f")
	assert.NotContains(t, wrapped, "2>&1")
	assert.NotContains(t, wrapped, "2>/dev/null")
	assert.Equal(t, "journalctl -f", (&Client{cfg: Config{User: "root"}}).remoteStreamCommand("journalctl -f"))
}

// TestRemoteCommandKeepsSudosRefusalWhenTheCommandFails checks that sudo's
// diagnostic and exit status reach the operator if, for example, the sudo
// policy changes after a successful probe.
func TestRemoteCommandKeepsSudosRefusalWhenTheCommandFails(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "sudo")
	//nolint:gosec // The stub must be executable to run from the test's temporary directory.
	require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\necho 'sudo: a password is required' >&2\nexit 1\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	c := &Client{cfg: Config{Host: "203.0.113.10", User: "ubuntu"}}
	out, err := exec.Command("sh", "-c", c.remoteCommand("echo never")).CombinedOutput() //nolint:gosec // The test runs its own wrapper against stubs in its temporary directory.
	require.Error(t, err)
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 1, exit.ExitCode())
	assert.Contains(t, string(out), "a password is required")
	assert.NotContains(t, string(out), "never")
}

func TestCloudImageUserReadsTheImagesRefusal(t *testing.T) {
	user, ok := CloudImageUser("Please login as the user \"ec2-user\" rather than the user \"root\".\n")
	assert.True(t, ok)
	assert.Equal(t, "ec2-user", user)
	_, ok = CloudImageUser("Permission denied (publickey).")
	assert.False(t, ok)
}

func TestRemoteCommandRemovesItsTemporaryFileWhenInterrupted(t *testing.T) {
	dir := t.TempDir()
	//nolint:gosec // The stub must be executable to run from the test's temporary directory.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sudo"), []byte("#!/bin/sh\nkill -TERM $PPID\nsleep 1\n"), 0o755))
	//nolint:gosec // The stub must be executable to run from the test's temporary directory.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mktemp"), []byte("#!/bin/sh\ntouch "+filepath.Join(dir, "stderr")+"\necho "+filepath.Join(dir, "stderr")+"\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	c := &Client{cfg: Config{Host: "203.0.113.10", User: "ubuntu"}}
	_ = exec.Command("sh", "-c", c.remoteCommand("true")).Run() //nolint:gosec // The test runs its own wrapper against stubs in its temporary directory.

	_, err := os.Stat(filepath.Join(dir, "stderr"))
	assert.True(t, os.IsNotExist(err), "the trap removes the file when the shell is terminated")
}
