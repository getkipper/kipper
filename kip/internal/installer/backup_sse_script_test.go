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

// stubMC simulates mc using environment variables. CP_KEYED_RC and CP_PLAIN_RC
// control cp exit codes with and without --enc-c. Keyed failures print
// CP_KEYED_ERR, defaulting to HTTP 501. Plain failures print HTTP 503.
// CAT_KEYED and CAT_PLAIN control cat output, defaulting to the test payload.
// CAT_KEYED_RC and CAT_PLAIN_RC cause failures when nonzero. FIND_OUT, FIND_ERR,
// and FIND_RC control find stdout, stderr, and exit status. STAT_KEYED and
// STAT_PLAIN provide "rc|stderr" for stat requests with and without --enc-c.
const stubMC = `#!/bin/sh
cmd=$1; shift
keyed=
case " $* " in *" --enc-c "*) keyed=1;; esac
case "$cmd" in
find) printf '%s' "$FIND_OUT"; printf '%s' "$FIND_ERR" >&2; exit "$FIND_RC";;
cp)
  rc=${CP_PLAIN_RC:-0}; err="mc: <ERROR> Failed to copy. 503 Service Unavailable."
  [ -n "$keyed" ] && { rc=${CP_KEYED_RC:-0}; err=${CP_KEYED_ERR-"mc: <ERROR> Failed to copy. 501 Not Implemented."}; }
  [ "$rc" = 0 ] || echo "$err" >&2
  exit "$rc";;
cat)
  if [ -n "$keyed" ]; then rc=${CAT_KEYED_RC:-0}; out=${CAT_KEYED-kipper}; else rc=${CAT_PLAIN_RC:-0}; out=${CAT_PLAIN-kipper}; fi
  [ "$rc" = 0 ] || { echo "mc: <ERROR> Unable to read. 400 Bad Request." >&2; exit "$rc"; }
  echo "$out";;
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

// TestTheWriteTestAnswersOnlyWhatItKnows requires demonstrated incompatibility
// for fallback, so inconclusive failures cannot leave backups without SSE-C.
func TestTheWriteTestAnswersOnlyWhatItKnows(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"refused without the key", map[string]string{"STAT_PLAIN": statRefused}, sseWriteOKMarker},
		{"served without the key", map[string]string{"STAT_PLAIN": statOK}, sseUnsupportedMarker},
		{"changed with the key", map[string]string{"STAT_PLAIN": statRefused, "CAT_KEYED": "other"}, sseUnsupportedMarker},
		{"keyed write not implemented, plain write accepted", map[string]string{"CP_KEYED_RC": "1"}, sseUnsupportedMarker},
		{"keyed write header not implemented, plain write accepted", map[string]string{"CP_KEYED_RC": "1",
			"CP_KEYED_ERR": "mc: <ERROR> Failed to copy. A header you provided implies functionality that is not implemented."}, sseUnsupportedMarker},
		{"keyed write unavailable, plain write accepted", map[string]string{"CP_KEYED_RC": "1",
			"CP_KEYED_ERR": "mc: <ERROR> Failed to copy. 503 Service Unavailable."}, ""},
		{"keyed write timed out, plain write accepted", map[string]string{"CP_KEYED_RC": "1",
			"CP_KEYED_ERR": "mc: <ERROR> Failed to copy. context deadline exceeded"}, ""},
		{"keyed write throttled, plain write accepted", map[string]string{"CP_KEYED_RC": "1",
			"CP_KEYED_ERR": "mc: <ERROR> Failed to copy. Please reduce your request rate."}, ""},
		{"keyed read refused, plain read served", map[string]string{"CAT_KEYED_RC": "1"}, sseUnsupportedMarker},
		{"stat failed", map[string]string{"STAT_PLAIN": statBroken}, ""},
		{"every write failed", map[string]string{"CP_KEYED_RC": "1", "CP_PLAIN_RC": "1"}, ""},
		{"every read failed", map[string]string{"CAT_KEYED_RC": "1", "CAT_PLAIN_RC": "1"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runSSEScript(t, sseWriteTestScript, tc.env)
			for _, marker := range []string{sseWriteOKMarker, sseUnsupportedMarker} {
				if marker == tc.want {
					assert.Contains(t, out, marker)
				} else {
					assert.NotContains(t, out, marker)
				}
			}
		})
	}
}
