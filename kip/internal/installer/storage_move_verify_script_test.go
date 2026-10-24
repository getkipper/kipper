package installer

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verifyTools returns a PATH that puts GNU or BusyBox stat first, since the
// verify pod's stat takes -c formats. Homebrew's coreutils provide one on macOS.
func verifyTools(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the verify script runs in a Linux container")
	}
	path := os.Getenv("PATH")
	for _, dir := range []string{"/opt/homebrew/opt/coreutils/libexec/gnubin", "/usr/local/opt/coreutils/libexec/gnubin"} {
		if _, err := os.Stat(filepath.Join(dir, "stat")); err == nil {
			path = dir + string(os.PathListSeparator) + path
			break
		}
	}
	probe := exec.Command("sh", "-c", "stat -c %F /")
	probe.Env = append(os.Environ(), "PATH="+path)
	if err := probe.Run(); err != nil {
		t.Skip("the verify script needs a stat that takes -c, from GNU coreutils or BusyBox")
	}
	return path
}

// toolOnPath returns where the named tool is found on path.
func toolOnPath(t *testing.T, path, tool string) string {
	t.Helper()
	lookup := exec.Command("sh", "-c", "command -v "+tool) //nolint:gosec // a fixed lookup of the tools on PATH
	lookup.Env = append(os.Environ(), "PATH="+path)
	real, err := lookup.Output()
	require.NoError(t, err, tool)
	return strings.TrimSpace(string(real))
}

// wrapTools puts scripts in front of path that stand in for the named tools.
// Each script is a body run by bash, with $real set to the tool it replaces.
func wrapTools(t *testing.T, path string, bodies map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for tool, body := range bodies {
		wrapper := "#!/bin/bash\nreal=" + toolOnPath(t, path, tool) + "\n" + body + "\n"
		//nolint:gosec // G306: the wrapper stands in for the tool on PATH, so it has to be executable.
		require.NoError(t, os.WriteFile(filepath.Join(dir, tool), []byte(wrapper), 0o700))
	}
	return dir + string(os.PathListSeparator) + path
}

// countingTools wraps stat and sha256sum so each run appends a line to the
// returned count file.
func countingTools(t *testing.T, path string) (string, string) {
	t.Helper()
	count := filepath.Join(t.TempDir(), "count")
	body := func(tool string) string { return "echo " + tool + " >> " + count + `; exec "$real" "$@"` }
	return wrapTools(t, path, map[string]string{"stat": body("stat"), "sha256sum": body("sha256sum")}), count
}

// runVerifyScript runs moveVerifyScript against src and dst in place of the
// pod's read-only mounts and returns its output and exit status.
func runVerifyScript(t *testing.T, path, src, dst string) (string, int) {
	t.Helper()
	script := strings.NewReplacer("/src", src, "/dst", dst).Replace(moveVerifyScript)
	cmd := exec.Command("bash", "-c", script) //nolint:gosec // the script under test is what this test exists to run
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	require.NoError(t, err, string(out))
	return string(out), 0
}

// oddDir is a directory name with a space and both kinds of quote.
const oddDir = `odd 'name" dir`

// backslashFile is a file name with a backslash, which Linux allows in a name.
const backslashFile = `back\slash`

// writeVerifyTree builds the same tree each time it is called: nested and
// empty directories, regular and empty files, names with spaces, quotes, a
// backslash and a percent sign, a symlink, a FIFO and a lost+found the
// comparison ignores.
func writeVerifyTree(t *testing.T, root string) {
	t.Helper()
	for _, d := range []string{"base/16384", oddDir, "lost+found/junk"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o750))
	}
	require.NoError(t, os.Mkdir(filepath.Join(root, "empty"), 0o700))
	for name, body := range map[string]string{
		"PG_VERSION":                "16\n",
		"base/16384/1259":           strings.Repeat("page", 2048),
		"base/16384/empty":          "",
		oddDir + "/a b.txt":         "spaces",
		"lost+found/junk/leftover":  "ignored",
		"postmaster.opts":           "postgres -D /var/lib/postgresql/data\n",
		"base/16384/-starts-a-flag": "dash",
		"base/" + backslashFile:     "backslash",
		"base/100% done %s":         "percent",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(body), 0o600))
	}
	require.NoError(t, os.Symlink("../PG_VERSION", filepath.Join(root, "base", "version-link")))
	require.NoError(t, syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600))
}

func verifyTrees(t *testing.T) (string, string) {
	t.Helper()
	src, dst := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "dst")
	writeVerifyTree(t, src)
	writeVerifyTree(t, dst)
	return src, dst
}

func TestMoveVerifyScriptPassesAnIdenticalCopy(t *testing.T) {
	path := verifyTools(t)
	src, dst := verifyTrees(t)
	require.NoError(t, os.RemoveAll(filepath.Join(dst, "lost+found", "junk")))

	out, code := runVerifyScript(t, path, src, dst)
	assert.Equal(t, 0, code, out)
	assert.Contains(t, out, "kipper-move-verified 15 entries", "every entry outside lost+found counts, the top directory included")
}

func TestMoveVerifyScriptFindsEachKindOfDifference(t *testing.T) {
	path := verifyTools(t)
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, dst string)
		want   string
	}{
		{"same-size content", func(t *testing.T, dst string) {
			require.NoError(t, os.WriteFile(filepath.Join(dst, "PG_VERSION"), []byte("17\n"), 0o600))
		}, "./PG_VERSION"},
		{"file mode", func(t *testing.T, dst string) {
			require.NoError(t, os.Chmod(filepath.Join(dst, "postmaster.opts"), 0o400))
		}, "./postmaster.opts"},
		{"directory mode", func(t *testing.T, dst string) {
			require.NoError(t, os.Remove(filepath.Join(dst, "empty")))
			require.NoError(t, os.Mkdir(filepath.Join(dst, "empty"), 0o750))
		}, "./empty"},
		{"top directory mode", func(t *testing.T, dst string) {
			require.NoError(t, os.Chmod(dst, 0o700)) //nolint:gosec // G302: a directory needs its execute bit, and the test needs a mode other than 0750
		}, "at .:"},
		{"missing empty directory", func(t *testing.T, dst string) {
			require.NoError(t, os.Remove(filepath.Join(dst, "empty")))
		}, "./empty"},
		{"missing file", func(t *testing.T, dst string) {
			require.NoError(t, os.Remove(filepath.Join(dst, "base", "16384", "empty")))
		}, "./base/16384/empty"},
		{"extra file", func(t *testing.T, dst string) {
			require.NoError(t, os.WriteFile(filepath.Join(dst, "base", "extra"), nil, 0o600))
		}, "the copy holds 16 entries and the source 15"},
		{"symlink target", func(t *testing.T, dst string) {
			link := filepath.Join(dst, "base", "version-link")
			require.NoError(t, os.Remove(link))
			require.NoError(t, os.Symlink("../postmaster.opts", link))
		}, "./base/version-link"},
		{"symlink target with a line break", func(t *testing.T, dst string) {
			link := filepath.Join(dst, "base", "version-link")
			require.NoError(t, os.Remove(link))
			require.NoError(t, os.Symlink("../PG\nVERSION", link))
		}, "./base/version-link"},
		{"symlink copied as its target", func(t *testing.T, dst string) {
			link := filepath.Join(dst, "base", "version-link")
			require.NoError(t, os.Remove(link))
			require.NoError(t, os.WriteFile(link, []byte("16\n"), 0o600))
		}, "./base/version-link"},
		{"FIFO copied as a file", func(t *testing.T, dst string) {
			require.NoError(t, os.Remove(filepath.Join(dst, "pipe")))
			require.NoError(t, os.WriteFile(filepath.Join(dst, "pipe"), nil, 0o600))
		}, "./pipe"},
		{"name with quotes", func(t *testing.T, dst string) {
			require.NoError(t, os.WriteFile(filepath.Join(dst, oddDir, "a b.txt"), []byte("SPACES"), 0o600))
		}, "./" + oddDir + "/a b.txt"},
		{"name with a backslash", func(t *testing.T, dst string) {
			require.NoError(t, os.WriteFile(filepath.Join(dst, "base", backslashFile), []byte("BACKSLASH"), 0o600))
		}, "./base/" + backslashFile},
		{"name with a percent sign", func(t *testing.T, dst string) {
			require.NoError(t, os.WriteFile(filepath.Join(dst, "base", "100% done %s"), []byte("PERCENT"), 0o600))
		}, "./base/100% done %s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := verifyTrees(t)
			tc.change(t, dst)
			out, code := runVerifyScript(t, path, src, dst)
			assert.Equal(t, 3, code, out)
			assert.Contains(t, out, "kipper-move-mismatch")
			assert.Contains(t, out, tc.want)
			assert.NotContains(t, out, "kipper-move-verified")
		})
	}
}

func TestMoveVerifyScriptFindsAChangedOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing a file's owner needs root")
	}
	path := verifyTools(t)
	src, dst := verifyTrees(t)
	require.NoError(t, os.Lchown(filepath.Join(dst, "base", "version-link"), 70, 70))
	out, code := runVerifyScript(t, path, src, dst)
	assert.Equal(t, 3, code, out)
	assert.Contains(t, out, "./base/version-link")
}

func TestMoveVerifyScriptStopsAtAnUnreadableSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	path := verifyTools(t)
	src, dst := verifyTrees(t)
	require.NoError(t, os.Chmod(filepath.Join(src, "PG_VERSION"), 0))
	require.NoError(t, os.Chmod(filepath.Join(dst, "PG_VERSION"), 0))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(src, "PG_VERSION"), 0o600) })

	out, code := runVerifyScript(t, path, src, dst)
	assert.Equal(t, 1, code, out)
	assert.Contains(t, out, "the source entry ./PG_VERSION cannot be read")
	assert.NotContains(t, out, "kipper-move-mismatch", "an unreadable source says nothing about the copy")
}

// A verify pod that started stat and sha256sum for every entry spent about
// 19 ms per file under its CPU limit, so 150,000 small files took 48 minutes.
func TestMoveVerifyScriptReadsInBatches(t *testing.T) {
	path := verifyTools(t)
	src, dst := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "dst")
	for _, root := range []string{src, dst} {
		for d := 0; d < 3; d++ {
			dir := filepath.Join(root, "d", string(rune('a'+d)))
			require.NoError(t, os.MkdirAll(dir, 0o750))
			for f := 0; f < 100; f++ {
				require.NoError(t, os.WriteFile(filepath.Join(dir, strings.Repeat("f", f%7+1)+string(rune('0'+f%10))+string(rune('a'+f/10))), []byte{byte(f)}, 0o600))
			}
		}
	}
	path, count := countingTools(t, path)

	out, code := runVerifyScript(t, path, src, dst)
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "kipper-move-verified 305 entries")
	runs, err := os.ReadFile(count)
	require.NoError(t, err)
	assert.LessOrEqual(t, strings.Count(string(runs), "\n"), 12, "stat and sha256sum run once per side and batch, not once per entry")
}

// readlink fails on both sides here, for two different targets of the same
// length. The stat wrapper acts as BusyBox stat does when it cannot read a
// target, leaving %N out and still exiting 0, so the test also catches a
// script that takes symlink targets from stat %N.
func TestMoveVerifyScriptStopsWhenASymlinkTargetCannotBeRead(t *testing.T) {
	path := verifyTools(t)
	src, dst := verifyTrees(t)
	relink(t, dst, "../pg_version")
	path = wrapTools(t, path, map[string]string{
		"stat": `args=("$@")
for i in "${!args[@]}"; do
  if [ "${args[$i]}" = -c ]; then args[$((i+1))]=${args[$((i+1))]//%N/}; fi
done
exec "$real" "${args[@]}"`,
		"readlink": "exit 1",
	})

	out, code := runVerifyScript(t, path, src, dst)
	assert.Equal(t, 1, code, out)
	assert.Contains(t, out, "the source entry ./base/version-link cannot be read")
	assert.NotContains(t, out, "kipper-move-verified")
}

// A read that fails for a whole batch and then succeeds entry by entry says
// nothing about the copy, so it stops the comparison without calling the copy
// different.
func TestMoveVerifyScriptStopsAtAReadThatFailsOnlyInABatch(t *testing.T) {
	path := verifyTools(t)
	src, dst := verifyTrees(t)
	path = wrapTools(t, path, map[string]string{"sha256sum": `[ $# -gt 1 ] && exit 1
exec "$real" "$@"`})

	out, code := runVerifyScript(t, path, src, dst)
	assert.Equal(t, 1, code, out)
	assert.Contains(t, out, "could not be compared consistently")
	assert.NotContains(t, out, "kipper-move-mismatch")
	assert.NotContains(t, out, "kipper-move-verified")
}

// writeBatchTree writes 2500 four-byte files under root. Each relative path is
// 12 bytes plus its terminator, so 1000 paths fit within BusyBox xargs' 30 KiB
// argument buffer. The regular-file pass uses batches of 1000, 1000 and 500.
func writeBatchTree(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "base"), 0o750))
	for i := 0; i < 2500; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(root, "base", fmt.Sprintf("f%04d", i)), []byte(fmt.Sprintf("%04d", i)), 0o600))
	}
}

func TestMoveVerifyScriptComparesEveryBatch(t *testing.T) {
	path := verifyTools(t)
	src, dst := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "dst")
	writeBatchTree(t, src)
	writeBatchTree(t, dst)
	list := exec.Command("find", ".", "-type", "f")
	list.Dir = src
	found, err := list.Output()
	require.NoError(t, err)
	files := strings.Split(strings.TrimSpace(string(found)), "\n")
	require.Len(t, files, 2500)

	counted, count := countingTools(t, path)
	out, code := runVerifyScript(t, counted, src, dst)
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "kipper-move-verified 2502 entries")
	runs, err := os.ReadFile(count)
	require.NoError(t, err)
	assert.Equal(t, 6, strings.Count(string(runs), "sha256sum"), "three batches of regular files, each read on both sides")

	for _, at := range []int{999, 1000, 1999, 2000, 2499} {
		t.Run(fmt.Sprintf("batch %d entry %d", at/1000+1, at%1000+1), func(t *testing.T) {
			changed := filepath.Join(dst, files[at])
			original, err := os.ReadFile(changed)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(changed, []byte("XXXX"), 0o600))
			//nolint:gosec // G703: the path comes from find over this test's own temporary tree.
			t.Cleanup(func() { require.NoError(t, os.WriteFile(changed, original, 0o600)) })

			out, code := runVerifyScript(t, path, src, dst)
			assert.Equal(t, 3, code, out)
			assert.Contains(t, out, "kipper-move-mismatch: the copy differs from the source at "+files[at])
		})
	}
}

// failReadlinkIn makes readlink fail for the entry rel when it is read from
// root, and work everywhere else.
func failReadlinkIn(t *testing.T, path, root, rel string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	return wrapTools(t, path, map[string]string{"readlink": `if [ "$(pwd -P)" = '` + real + `' ] && [ "$1" = './` + rel + `' ]; then exit 1; fi
exec "$real" "$@"`})
}

// versionLink is the one symlink in writeVerifyTree's tree.
const versionLink = "base/version-link"

// relink points versionLink under root at target.
func relink(t *testing.T, root, target string) {
	t.Helper()
	require.NoError(t, os.Remove(filepath.Join(root, versionLink)))
	require.NoError(t, os.Symlink(target, filepath.Join(root, versionLink)))
}

// A failed target read never compares equal to a target that was read, even
// one that spells out what a failed read used to stand for.
func TestMoveVerifyScriptTellsAFailedTargetReadFromATarget(t *testing.T) {
	path := verifyTools(t)
	for _, tc := range []struct {
		name                 string
		srcTarget, dstTarget string
		unreadable           string
		extraLinks           bool
		code                 int
		want                 string
	}{
		{"copy unreadable, source target missing", "missing", "MISSING", "dst", false, 3, "kipper-move-mismatch: the copy differs from the source at ./" + versionLink},
		{"source unreadable, copy target unreadable", "UNREADABLE", "unreadable", "src", false, 1, "the source entry ./" + versionLink + " cannot be read"},
		{"copy unreadable among other links", "missing", "MISSING", "dst", true, 3, "kipper-move-mismatch: the copy differs from the source at ./" + versionLink},
		{"source unreadable among other links", "UNREADABLE", "unreadable", "src", true, 1, "the source entry ./" + versionLink + " cannot be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := verifyTrees(t)
			relink(t, src, tc.srcTarget)
			relink(t, dst, tc.dstTarget)
			if tc.extraLinks {
				for _, root := range []string{src, dst} {
					require.NoError(t, os.Symlink("../postmaster.opts", filepath.Join(root, "base", "opts-link")))
					require.NoError(t, os.Symlink("16384", filepath.Join(root, "base", "data-link")))
				}
			}
			side := map[string]string{"src": src, "dst": dst}[tc.unreadable]

			out, code := runVerifyScript(t, failReadlinkIn(t, path, side, versionLink), src, dst)
			assert.Equal(t, tc.code, code, out)
			assert.Contains(t, out, tc.want)
			assert.NotContains(t, out, "kipper-move-verified")
			if tc.unreadable == "dst" {
				assert.Contains(t, out, "and the copy has no readable entry there")
			}
			assert.NotContains(t, out, "could not be compared consistently", "a read that fails every time is no inconsistency")
		})
	}
}

// readlink ends each target with a line break, and command substitution
// drops trailing line breaks, so targets that end in one still have to
// compare byte for byte.
func TestMoveVerifyScriptComparesTargetsEndingInALineBreak(t *testing.T) {
	path := verifyTools(t)
	for _, tc := range []struct {
		name                 string
		srcTarget, dstTarget string
		code                 int
	}{
		{"same target", "x\n", "x\n", 0},
		{"a trailing line break against none", "x\n", "x", 3},
		{"line break against a letter", "x\n", "xy", 3},
		{"two line breaks against one", "x\n\n", "xy\n", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := verifyTrees(t)
			relink(t, src, tc.srcTarget)
			relink(t, dst, tc.dstTarget)
			out, code := runVerifyScript(t, path, src, dst)
			assert.Equal(t, tc.code, code, out)
		})
	}
}
