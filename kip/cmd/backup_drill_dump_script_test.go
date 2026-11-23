//go:build !windows

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePostgresTools writes psql, pg_restore, df and sha256sum stand-ins into
// a directory and returns its path and the call log path. The stand-ins report
// a database size of sizeBytes bytes, free space of freeKiB KiB, and a
// max_wal_size of walBytes bytes. An empty walBytes simulates an unreadable setting.
func fakePostgresTools(t *testing.T, sizeBytes, freeKiB, walBytes string) (bin, calls string) {
	t.Helper()
	bin = t.TempDir()
	calls = filepath.Join(bin, "calls")
	tools := map[string]string{
		"psql": `case "$*" in *" -c "* | *" -f "*) input= ;; *) input=$(cat) ;; esac
echo "psql $* $input" >> "` + calls + `"
case "$*$input" in
  *"SHOW data_directory"*) echo /var/lib/postgresql/data ;;
  *max_wal_size*) echo "` + walBytes + `" ;;
  *pg_database_size*) echo ` + sizeBytes + ` ;;
  *"count(*) FROM pg_database"*) echo 0 ;;
  *count.sql*) echo 'public.fill|3' ;;
esac`,
		"pg_restore": `echo "pg_restore $*" >> "` + calls + `"`,
		"df":         `printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda 996352 855040 ` + freeKiB + ` 86%% /var/lib/postgresql/data\n'`,
		"sha256sum":  `echo "abc  $1"`,
	}
	for name, body := range tools {
		//nolint:gosec // G306: the stand-in replaces the tool on PATH, so it has to be executable.
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	}
	return bin, calls
}

// runDumpScript runs drillDumpScript with a dump for database "app". The
// recorded row count is "public.fill|3", and the manifest records the dump's
// size as dumpBytes bytes.
func runDumpScript(t *testing.T, bin, dumpBytes string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := "database\tapp\t1.dump\t0\t" + dumpBytes + "\tabc\ncomplete\ttrue\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest"), []byte(manifest), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "1.dump"), []byte("dump"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "1.counts"), []byte("public.fill|3\n"), 0o600))
	script := strings.Replace(drillDumpScript, "D="+drillDumpDir, "D="+dir, 1)
	cmd := exec.Command("sh", "-c", script) //nolint:gosec // The command runs the fixed script under test.
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

func TestDumpScriptRestoresWhenTheVolumeHasRoom(t *testing.T) {
	bin, calls := fakePostgresTools(t, "1048576", "1000000", "1073741824")

	out := runDumpScript(t, bin, "4")

	assert.Equal(t, "KIPPER_DUMP_OK\tapp\t1\n", out)
	log, err := os.ReadFile(calls)
	require.NoError(t, err)
	assert.Contains(t, string(log), "CREATE DATABASE")
	assert.Contains(t, string(log), "DROP DATABASE")
}

func TestDumpScriptSkipsTheRestoreWhenTheVolumeLacksRoom(t *testing.T) {
	bin, calls := fakePostgresTools(t, "875560960", "141312", "1073741824")

	out := runDumpScript(t, bin, "4")

	assert.Equal(t, "KIPPER_DUMP_SKIPPED\tapp\t1818230784 144703488\n", out)
	log, err := os.ReadFile(calls)
	require.NoError(t, err)
	assert.Contains(t, string(log), "pg_restore --list")
	assert.NotContains(t, string(log), "CREATE DATABASE", "a restore that cannot fit must not start")
}

func TestDumpScriptSizesTheRestoreByTheLargerOfDumpAndDatabase(t *testing.T) {
	bin, calls := fakePostgresTools(t, "8388608", "204800", "")

	out := runDumpScript(t, bin, "300000000")

	assert.Equal(t, "KIPPER_DUMP_SKIPPED\tapp\t667108864 209715200\n", out, "a table emptied after the dump leaves the restored database small")
	log, err := os.ReadFile(calls)
	require.NoError(t, err)
	assert.NotContains(t, string(log), "CREATE DATABASE")
}

func TestDumpScriptCapsTheWALEstimateForALargeDatabase(t *testing.T) {
	const twentyGiB = "21474836480"
	for name, tc := range map[string]struct{ freeKiB, want string }{
		"room for the database and twice max_wal_size": {freeKiB: "30408704", want: "KIPPER_DUMP_OK\tapp\t1\n"},
		"too little room even with the cap":            {freeKiB: "23068672", want: "KIPPER_DUMP_SKIPPED\tapp\t23689428992 23622320128\n"},
	} {
		t.Run(name, func(t *testing.T) {
			bin, _ := fakePostgresTools(t, twentyGiB, tc.freeKiB, "1073741824")

			out := runDumpScript(t, bin, "4")

			assert.Equal(t, tc.want, out, "Postgres recycles WAL past max_wal_size, so a restore needs about the database plus at most twice that")
		})
	}
}
