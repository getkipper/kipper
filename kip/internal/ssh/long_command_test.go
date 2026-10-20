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

// localShellSSH installs a stub ssh that passes its last argument to sh -c,
// modelling sshd's invocation of the user's shell, and records the argument's
// length. Connection probes ending in " true" succeed without being recorded.
func localShellSSH(t *testing.T) (lengths string) {
	t.Helper()
	dir := t.TempDir()
	lengths = filepath.Join(dir, "lengths")
	stub := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\n" +
		"case \"$*\" in *' true') exit 0 ;; esac\n" +
		"for last; do :; done\n" +
		"printf '%s\\n' \"${#last}\" >> " + lengths + "\n" +
		"exec sh -c \"$last\"\n"
	//nolint:gosec // The stub in the test's temporary directory must be executable.
	require.NoError(t, os.WriteFile(stub, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return lengths
}

func TestALongCommandTravelsOutsideTheCommandLine(t *testing.T) {
	lengths := localShellSSH(t)
	c, err := Dial(Config{Host: "demo.example.com", User: "root"})
	require.NoError(t, err)

	manifest := strings.Repeat("# a long manifest line\n", 500)
	out, err := c.Run("cat <<'EOF' | wc -l\n" + manifest + "EOF\nprintf 'stdin:'; cat; echo")

	require.NoError(t, err)
	assert.Contains(t, out, "500", "the whole command ran")
	assert.Contains(t, out, "stdin:\n", "the command's own stdin is empty, as it is for a short command")
	recorded, err := os.ReadFile(lengths)
	require.NoError(t, err)
	for _, line := range strings.Fields(string(recorded)) {
		n, convErr := strconv.Atoi(line)
		require.NoError(t, convErr)
		assert.Less(t, n, maxInlineCommand, "OpenSSH shares connections through a local socket whose default buffer size on macOS is 8 KiB, so long command lines can fail there")
	}
}

func TestAShortCommandStaysOnTheCommandLine(t *testing.T) {
	lengths := localShellSSH(t)
	c, err := Dial(Config{Host: "demo.example.com", User: "root"})
	require.NoError(t, err)

	out, err := c.Run("echo hello")

	require.NoError(t, err)
	assert.Equal(t, "hello\n", out)
	recorded, err := os.ReadFile(lengths)
	require.NoError(t, err)
	assert.Contains(t, string(recorded), strconv.Itoa(len("echo hello")))
}

func TestALongStreamedCommandTravelsOutsideTheCommandLine(t *testing.T) {
	lengths := localShellSSH(t)
	c, err := Dial(Config{Host: "demo.example.com", User: "root"})
	require.NoError(t, err)

	var stdout, stderr strings.Builder
	err = c.RunStreamTo("cat <<'EOF' | wc -l\n"+strings.Repeat("# line\n", 1000)+"EOF\necho err >&2", &stdout, &stderr)

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "1000")
	assert.Equal(t, "err\n", stderr.String(), "stdout and stderr stay separate")
	recorded, err := os.ReadFile(lengths)
	require.NoError(t, err)
	for _, line := range strings.Fields(string(recorded)) {
		n, _ := strconv.Atoi(line)
		assert.Less(t, n, maxInlineCommand)
	}
}

func TestALongCommandKeepsItsExitStatus(t *testing.T) {
	localShellSSH(t)
	c, err := Dial(Config{Host: "demo.example.com", User: "root"})
	require.NoError(t, err)

	_, err = c.Run(strings.Repeat("true\n", 2000) + "exit 7")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit status 7")
}

func TestALongCommandKeepsItsTrailingNewlines(t *testing.T) {
	localShellSSH(t)
	c, err := Dial(Config{Host: "demo.example.com", User: "root"})
	require.NoError(t, err)

	out, err := c.Run(strings.Repeat("true\n", 1000) + "cat <<'EOF'\nlast\n\n")

	require.NoError(t, err)
	assert.Equal(t, "last\n\n", out, "the command's trailing newlines reach the shell intact, as they would inline")
}
