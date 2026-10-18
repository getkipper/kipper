package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubMC simulates mc. The cat command returns the write test payload. The
// find command uses FIND_OUT, FIND_ERR and FIND_RC for stdout, stderr and exit
// status. The stat command reads "rc|stderr" from STAT_KEYED for --enc-c
// requests and STAT_PLAIN otherwise.
const stubMC = `#!/bin/sh
cmd=$1; shift
case "$cmd" in
find) printf '%s' "$FIND_OUT"; printf '%s' "$FIND_ERR" >&2; exit "$FIND_RC";;
cat) echo kipper;;
stat)
  spec=$STAT_PLAIN
  case " $* " in *" --enc-c "*) spec=$STAT_KEYED;; esac
  rc=${spec%%|*}; msg=${spec#*|}
  [ -n "$msg" ] && echo "$msg" >&2
  exit "$rc";;
esac
exit 0
`

func runSSEScript(t *testing.T, script string, env map[string]string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the check scripts run in a Linux container")
	}
	dir := t.TempDir()
	//nolint:gosec // G306: the stub stands in for mc on PATH, so it has to be executable.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mc"), []byte(stubMC), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, sseKeyField), []byte("0123456789abcdef0123456789abcdef"), 0o600))
	script = strings.ReplaceAll(script, "/sse/", dir+"/")
	script = strings.ReplaceAll(script, "/tmp/", dir+"/")
	cmd := exec.Command("sh", "-c", script) //nolint:gosec // the script under test is what this test exists to run
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "BUCKET=b", "FIND_RC=0",
		"FIND_OUT=kipper/b/backups/daily-20261019/velero-backup.json\n")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

const (
	statOK      = "0|"
	statRefused = "1|mc: <ERROR> Unable to stat `kipper/b/backups/daily-20261019/velero-backup.json`. 400 Bad Request."
	statBroken  = "1|mc: <ERROR> Unable to stat `kipper/b/backups/daily-20261019/velero-backup.json`. 503 Service Unavailable."
)

func TestThePlainProbeAnswersOnlyWhatItKnows(t *testing.T) {
	assert.Contains(t, runSSEScript(t, ssePlainProbeScript, map[string]string{"STAT_PLAIN": statOK}), ssePlainMarker)
	assert.Contains(t, runSSEScript(t, ssePlainProbeScript, map[string]string{"STAT_PLAIN": statRefused}), sseEncryptedMarker)

	out := runSSEScript(t, ssePlainProbeScript, map[string]string{"STAT_PLAIN": statBroken})
	assert.Contains(t, out, sseFailedMarker, "a 503 proves nothing about encryption")
	assert.NotContains(t, out, ssePlainMarker)

	out = runSSEScript(t, ssePlainProbeScript, map[string]string{"FIND_RC": "1", "FIND_OUT": "", "FIND_ERR": "mc: <ERROR> Unable to stat `kipper/b/backups`. Object does not exist."})
	assert.Contains(t, out, sseNoBackupsMarker)

	out = runSSEScript(t, ssePlainProbeScript, map[string]string{"FIND_RC": "1", "FIND_OUT": "", "FIND_ERR": "mc: <ERROR> Access Denied."})
	assert.Contains(t, out, sseFailedMarker, "a failed listing is not an empty one")
}

func TestTheKeyCheckAnswersOnlyWhatItKnows(t *testing.T) {
	for _, tc := range []struct {
		plain, keyed, want string
	}{
		{statRefused, statOK, sseKeyMatchMarker},
		{statRefused, statRefused, sseKeyWrongMarker},
		{statOK, statOK, ssePlainMarker},
		{statBroken, statOK, sseFailedMarker},
		{statRefused, statBroken, sseFailedMarker},
	} {
		out := runSSEScript(t, sseKeyCheckScript, map[string]string{"STAT_PLAIN": tc.plain, "STAT_KEYED": tc.keyed})
		assert.Contains(t, out, tc.want, "plain %q, keyed %q", tc.plain, tc.keyed)
	}
}

func TestTheWriteTestPassesOnlyOnAnSSECRefusal(t *testing.T) {
	assert.Contains(t, runSSEScript(t, sseWriteTestScript, map[string]string{"STAT_PLAIN": statRefused}), sseWriteOKMarker)
	assert.NotContains(t, runSSEScript(t, sseWriteTestScript, map[string]string{"STAT_PLAIN": statOK}), sseWriteOKMarker,
		"a store that serves the object without the key ignored SSE-C")
	assert.NotContains(t, runSSEScript(t, sseWriteTestScript, map[string]string{"STAT_PLAIN": statBroken}), sseWriteOKMarker)
}
