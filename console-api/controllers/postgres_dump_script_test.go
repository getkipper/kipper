package controllers

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

// dumpStubs stand in for the Postgres tools. The psql session stub runs the
// script's \! line the way psql would, with the snapshot and output path it
// would have set, and writes the counts file.
var dumpStubs = map[string]string{
	"psql": `#!/bin/sh
case "$*" in *"count(*) FROM pg_database"*) echo "${ODD:-0}"; exit 0;; esac
case " $* " in *" -XAtc "*) printf '%s\n' $DBS; exit $LIST_RC;; esac
[ -n "${PGDATABASE:-}" ] || exit 8
out=; counts=
while [ $# -gt 0 ]; do case "$1" in -v) case "$2" in out=*) out=${2#out=};; counts=*) counts=${2#counts=};; esac; shift;; esac; shift; done
line=$(grep '^\\! ' | sed 's/^\\! //')
KIPPER_SNAPSHOT=snap-1 KIPPER_OUT="$out" sh -c "$line"
echo "public.orders|250" > "$counts"
exit "${PSQL_RC:-0}"
`,
	"pg_dump": `#!/bin/sh
while [ $# -gt 0 ]; do case "$1" in -f) f=$2; shift;; --snapshot=*) snap=${1#--snapshot=};; esac; shift; done
[ "$snap" = snap-1 ] || exit 9
[ -n "${PGDATABASE:-}" ] || exit 7
echo dump > "$f"; exit "${PGDUMP_RC:-0}"
`,
	"pg_dumpall": "#!/bin/sh\necho roles\n",
	"du":         "#!/bin/sh\ncase \"$2\" in */pg_wal) printf '%s\\t%s\\n' \"${WAL_KB:-0}\" \"$2\";; *) printf '%s\\t%s\\n' \"$DU_KB\" \"$2\";; esac\n",
	"df": "#!/bin/sh\nfor a; do p=$a; done\ndev=${DUMP_DEV:-shared}; case \"$p\" in */dump) ;; *) dev=${DATA_DEV:-shared};; esac\n" +
		"echo 'Filesystem 1024-blocks Used Available Capacity Mounted'\necho \"$dev ${DF_TOTAL_KB:-10000000} 0 $DF_KB 0% /\"\n",
}

func runDumpScript(t *testing.T, env map[string]string) (string, string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the hook runs in a Linux container")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o700))
	for name, body := range dumpStubs {
		//nolint:gosec // G306: the stubs stand in for tools on PATH, so they have to be executable.
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700))
	}
	dump := filepath.Join(dir, "dump")
	require.NoError(t, os.MkdirAll(dump, 0o700))
	script := strings.ReplaceAll(postgresDumpScript, dumpMountPath, dump)
	cmd := exec.Command("sh", "-c", script) //nolint:gosec // This test executes a fixed script with stubbed tools.
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "PGDATA="+dir, "POSTGRES_USER=kipper",
		"DBS=app postgres", "LIST_RC=0", "DU_KB=1000", "DF_KB=5000000")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	manifest, _ := os.ReadFile(filepath.Join(dump, "current", "manifest"))
	return string(manifest), string(out), err
}

func TestTheDumpScriptRecordsACompleteDump(t *testing.T) {
	manifest, out, err := runDumpScript(t, nil)

	require.NoError(t, err, out)
	assert.Contains(t, manifest, "format\tkipper-dump/1")
	assert.Contains(t, manifest, "database\tapp\t1.dump\t0\t5\t")
	assert.Contains(t, manifest, "database\tpostgres\t2.dump\t0\t")
	assert.True(t, strings.HasSuffix(manifest, "complete\ttrue\n"), manifest)
}

func TestAFailedDumpIsNeverRecordedAsComplete(t *testing.T) {
	manifest, _, err := runDumpScript(t, map[string]string{"PGDUMP_RC": "1"})

	require.Error(t, err, "Velero counts a nonzero exit in hooksFailed")
	assert.Contains(t, manifest, "database\tapp\t1.dump\t1\t")
	assert.Contains(t, manifest, "complete\tfalse")
	assert.NotContains(t, manifest, "complete\ttrue")
}

func TestAFailedSessionIsNeverRecordedAsComplete(t *testing.T) {
	manifest, _, err := runDumpScript(t, map[string]string{"PSQL_RC": "3"})

	require.Error(t, err, "counts that did not finish leave the dump unproven")
	assert.NotContains(t, manifest, "complete\ttrue")
}

func TestTheDumpRefusesToFillItsVolume(t *testing.T) {
	manifest, _, err := runDumpScript(t, map[string]string{"DF_KB": "500"})

	require.Error(t, err)
	assert.Contains(t, manifest, "complete\tfalse")
	assert.Contains(t, manifest, "500 KiB free")
}

func TestTheWriteAheadLogDoesNotCountTowardTheDump(t *testing.T) {
	_, _, err := runDumpScript(t, map[string]string{"DATA_DEV": "a", "DUMP_DEV": "b", "DU_KB": "3000000", "DF_KB": "2000000"})
	require.Error(t, err)

	_, out, err := runDumpScript(t, map[string]string{"DATA_DEV": "a", "DUMP_DEV": "b", "DU_KB": "3000000", "WAL_KB": "1500000", "DF_KB": "2000000"})
	require.NoError(t, err, "pg_dump never writes the WAL: %s", out)
}

func TestADedicatedDumpVolumeNeedsNoHeadroom(t *testing.T) {
	_, out, err := runDumpScript(t, map[string]string{"DATA_DEV": "/dev/longhorn/data", "DUMP_DEV": "/dev/longhorn/dump", "DF_KB": "2000000"})
	require.NoError(t, err, "a volume of its own cannot starve the database: %s", out)
}

func TestADatabaseNameTheManifestCannotHoldIsRefused(t *testing.T) {
	manifest, _, err := runDumpScript(t, map[string]string{"ODD": "1"})

	require.Error(t, err)
	assert.Contains(t, manifest, "line break or tab")
	assert.NotContains(t, manifest, "complete\ttrue")
}

func TestAnEarlyFailureStillNamesTheFormat(t *testing.T) {
	manifest, _, err := runDumpScript(t, map[string]string{"DF_KB": "500"})

	require.Error(t, err)
	assert.True(t, strings.HasPrefix(manifest, "format\tkipper-dump/1\n"), manifest)
}

func TestTheDumpKeepsHeadroomOnASharedDisk(t *testing.T) {
	// Room for the data, but not for the 2 GiB that stay free.
	_, _, err := runDumpScript(t, map[string]string{"DF_KB": "2000000"})
	require.Error(t, err)

	// A tenth of a large filesystem is more than 2 GiB.
	_, _, err = runDumpScript(t, map[string]string{"DF_TOTAL_KB": "100000000", "DF_KB": "5000000"})
	require.Error(t, err)

	_, out, err := runDumpScript(t, map[string]string{"DF_TOTAL_KB": "100000000", "DF_KB": "20000000"})
	require.NoError(t, err, out)
}
