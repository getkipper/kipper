package localrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// commandRunner is what the installer asks of both runners.
type commandRunner interface {
	Run(string) (string, error)
	RunStdin(string, *strings.Reader) (string, error)
}

// sshRunner adapts the SSH client to the shape the corpus uses.
type sshRunner struct{ c *ssh.Client }

func (s sshRunner) Run(cmd string) (string, error) { return s.c.Run(cmd) }
func (s sshRunner) RunStdin(cmd string, in *strings.Reader) (string, error) {
	return s.c.RunStdin(cmd, in)
}

type localRunner struct{ r *Runner }

func (l localRunner) Run(cmd string) (string, error) { return l.r.Run(cmd) }
func (l localRunner) RunStdin(cmd string, in *strings.Reader) (string, error) {
	return l.r.RunStdin(cmd, in)
}

// assertContract checks what the installer's command strings rely on. The SSH
// client is the reference; the local runner must give the same answers.
func assertContract(t *testing.T, r commandRunner, wantDir string) {
	t.Helper()
	_, err := r.Run("set -o pipefail; false | true")
	require.Error(t, err, "pipefail")
	assert.Equal(t, 1, exitCode(t, err))

	// SSH carries stdout and stderr separately; check ordering within each stream.
	out, err := r.Run("echo one; echo two >&2; echo three")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"one", "two", "three"}, strings.Fields(out), "both streams merged")
	assert.Less(t, strings.Index(out, "one"), strings.Index(out, "three"), "stdout in order")

	_, err = r.Run("exit 3")
	assert.Equal(t, 3, exitCode(t, err))

	out, err = r.Run("pwd -P")
	require.NoError(t, err)
	assert.Equal(t, wantDir, strings.TrimSpace(out), "working directory")

	out, err = r.Run("cat; echo done")
	require.NoError(t, err)
	assert.Equal(t, "done\n", out, "stdin at end of file")

	out, err = r.RunStdin("cat", strings.NewReader("payload"))
	require.NoError(t, err)
	assert.Equal(t, "payload", out)

	out, err = r.Run("command -v sshd >/dev/null && echo found")
	require.NoError(t, err)
	assert.Equal(t, "found\n", out, "sbin on PATH, so a bare sshd -T resolves")
}

// TestParityWithSSH runs the contract through the real SSH client against the
// server in KIP_PARITY_HOST (key in KIP_PARITY_KEY), as root. It changes
// nothing on the server.
func TestParityWithSSH(t *testing.T) {
	host := os.Getenv("KIP_PARITY_HOST")
	if host == "" {
		t.Skip("set KIP_PARITY_HOST (and KIP_PARITY_KEY) to compare against a real server")
	}
	client, err := ssh.Dial(ssh.Config{Host: host, User: "root", KeyPath: os.Getenv("KIP_PARITY_KEY")})
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	assertContract(t, sshRunner{client}, "/root")
}

// TestLocalRunnerMeetsTheContract skips the contract check when sshd is unavailable.
func TestLocalRunnerMeetsTheContract(t *testing.T) {
	r, home := testRunner(t)
	if _, err := r.Run("command -v sshd"); err != nil {
		t.Skip("no sshd on this machine to resolve through sbin")
	}
	dir, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	assertContract(t, localRunner{r}, dir)
}
