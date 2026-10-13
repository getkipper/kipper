package localrun

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

// testRunner isolates commands in a temporary home so tests can run without root.
func testRunner(t *testing.T) (*Runner, string) {
	t.Helper()
	home := t.TempDir()
	return &Runner{Home: home, Dir: home, Path: RootPath}, home
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	var exit *exec.ExitError
	require.True(t, errors.As(err, &exit), "want an *exec.ExitError, got %v", err)
	return exit.ExitCode()
}

func TestRunUsesBashWithPipefail(t *testing.T) {
	r, _ := testRunner(t)

	_, err := r.Run("set -o pipefail; false | true")

	require.Error(t, err)
	assert.Equal(t, 1, exitCode(t, err))
}

func TestRunMergesStdoutAndStderrInOrder(t *testing.T) {
	r, _ := testRunner(t)

	out, err := r.Run("echo one; echo two >&2; echo three")

	require.NoError(t, err)
	assert.Equal(t, "one\ntwo\nthree\n", out)
}

func TestRunReportsTheExitCodeAndTheOutput(t *testing.T) {
	r, _ := testRunner(t)

	out, err := r.Run("echo broke >&2; exit 3")

	assert.Equal(t, 3, exitCode(t, err))
	assert.Equal(t, "broke\n", out)
	assert.Contains(t, err.Error(), "broke", "the error carries the output, as over SSH")
}

func TestRunStartsInTheHomeDirectory(t *testing.T) {
	r, home := testRunner(t)

	out, err := r.Run("pwd -P")

	require.NoError(t, err)
	want, _ := filepath.EvalSymlinks(home)
	assert.Equal(t, want, strings.TrimSpace(out))
}

func TestRunGivesTheCommandNoInput(t *testing.T) {
	r, _ := testRunner(t)

	out, err := r.Run("cat; echo done")

	require.NoError(t, err)
	assert.Equal(t, "done\n", out)
}

func TestRunStdinFeedsOnlyTheGivenInput(t *testing.T) {
	r, _ := testRunner(t)

	out, err := r.RunStdin("cat", strings.NewReader("secret material"))

	require.NoError(t, err)
	assert.Equal(t, "secret material", out)
}

func TestRunSetsItsOwnPathAndHome(t *testing.T) {
	r, home := testRunner(t)
	t.Setenv("PATH", "/nonexistent")
	t.Setenv("HOME", "/somewhere/else")

	out, err := r.Run(`echo "$PATH"; echo "$HOME"; command -v ls`)

	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 3)
	assert.Equal(t, RootPath, lines[0])
	assert.Equal(t, home, lines[1])
	assert.NotEmpty(t, lines[2], "ls resolves through the runner's PATH, not the parent's")
	assert.Contains(t, RootPath, "/usr/sbin")
	assert.Contains(t, RootPath, "/sbin")
}

func TestRunDropsInheritedClusterAndShellSettings(t *testing.T) {
	r, home := testRunner(t)
	startup := filepath.Join(home, "startup.sh")
	require.NoError(t, os.WriteFile(startup, []byte("echo injected\n"), 0o600))
	t.Setenv("KUBECONFIG", "/home/operator/.kube/other-cluster")
	t.Setenv("BASH_ENV", startup)

	out, err := r.Run(`echo "kubeconfig=${KUBECONFIG:-unset}"`)

	require.NoError(t, err)
	assert.Equal(t, "kubeconfig=unset\n", out, "neither KUBECONFIG nor a BASH_ENV startup file reaches the command")
}

func TestUploadCreatesThePrivateFileAndReplacesASymlink(t *testing.T) {
	r, home := testRunner(t)
	target := filepath.Join(home, "elsewhere")
	require.NoError(t, os.WriteFile(target, []byte("untouched"), 0o600))
	dest := filepath.Join(home, "creds file")
	require.NoError(t, os.Symlink(target, dest))

	require.NoError(t, r.Upload(strings.NewReader("key=value"), dest, 0o600))

	info, err := os.Lstat(dest)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.Zero(t, info.Mode()&os.ModeSymlink, "the planted symlink is replaced, not followed")
	got, _ := os.ReadFile(dest)
	assert.Equal(t, "key=value", string(got))
	untouched, _ := os.ReadFile(target)
	assert.Equal(t, "untouched", string(untouched))
}

func TestTheLocalRunnerSurvivesTheSSHRateLimit(t *testing.T) {
	r, _ := testRunner(t)

	assert.True(t, r.SurvivesSSHRateLimit(), "it opens no SSH connections")
}

func TestRunNamesAMissingBash(t *testing.T) {
	r, home := testRunner(t)
	r.Path = home

	_, err := r.Run("true")

	assert.ErrorContains(t, err, "bash is not installed")
}
