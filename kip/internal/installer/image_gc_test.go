package installer

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestImageGCDropInText(t *testing.T) {
	assert.Equal(t, "# Managed by Kipper. The kubelet removes unused images from 70% disk use,\n"+
		"# before Longhorn stops placing replicas on a disk that is 75% full.\n"+
		"kubelet-arg+:\n"+
		"  - \"image-gc-high-threshold=70\"\n"+
		"  - \"image-gc-low-threshold=60\"\n", imageGCConfig)
	assert.Equal(t, "kubelet-arg+:\n  - \"protect-kernel-defaults=true\"\n", agentKubeletConfig)
}

func runImageGCCommand(t *testing.T, dir string) {
	t.Helper()
	out, err := exec.Command("sh", "-c", imageGCCommand(dir)).CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestImageGCCommandWritesTheDropIn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config.yaml.d")

	runImageGCCommand(t, dir)

	saved, err := os.ReadFile(filepath.Join(dir, imageGCDropIn))
	require.NoError(t, err)
	assert.Equal(t, imageGCConfig, string(saved))
	assert.NoFileExists(t, filepath.Join(dir, agentKubeletDropIn), "a server has no worker kubelet drop-in to rewrite")
}

// A worker joined by an older kip has kipper-kubelet.yaml with a plain
// kubelet-arg key. It sorts after kipper-image-gc.yaml, so k3s would let it
// replace the thresholds.
func TestImageGCCommandMovesAnOlderWorkerKubeletDropInToAppend(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, agentKubeletDropIn)
	require.NoError(t, os.WriteFile(older, []byte("kubelet-arg:\n  - \"protect-kernel-defaults=true\"\n"), 0o600))

	runImageGCCommand(t, dir)

	saved, err := os.ReadFile(older)
	require.NoError(t, err)
	assert.Equal(t, agentKubeletConfig, string(saved))
}

// The restart check compares file times with when k3s started, so a file
// that already holds the right content must keep its time.
func TestImageGCCommandLeavesCurrentFilesUntouched(t *testing.T) {
	dir := t.TempDir()
	runImageGCCommand(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, agentKubeletDropIn), []byte(agentKubeletConfig), 0o600))
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, name := range []string{imageGCDropIn, agentKubeletDropIn} {
		require.NoError(t, os.Chtimes(filepath.Join(dir, name), old, old))
	}

	runImageGCCommand(t, dir)

	for _, name := range []string{imageGCDropIn, agentKubeletDropIn} {
		info, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.True(t, info.ModTime().Equal(old), "%s was rewritten", name)
	}
}

// mergedKubeletArgs follows k3s's documented config merge: config.yaml first,
// then config.yaml.d in name order. A later plain key replaces the value and a
// key ending in + appends to it.
func mergedKubeletArgs(t *testing.T, configYAML string, dropIns map[string]string) []string {
	t.Helper()
	docs := []string{configYAML}
	names := make([]string, 0, len(dropIns))
	for name := range dropIns {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		docs = append(docs, dropIns[name])
	}
	var args []string
	for _, doc := range docs {
		var parsed struct {
			Set    []string `yaml:"kubelet-arg"`
			Append []string `yaml:"kubelet-arg+"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(doc), &parsed))
		if parsed.Set != nil {
			args = parsed.Set
		}
		args = append(args, parsed.Append...)
	}
	return args
}

func TestEveryNodeKeepsItsKubeletArgsAlongsideTheThresholds(t *testing.T) {
	thresholds := []string{"image-gc-high-threshold=70", "image-gc-low-threshold=60"}

	server := mergedKubeletArgs(t, "kubelet-arg:\n  - \"protect-kernel-defaults=true\"\n",
		map[string]string{imageGCDropIn: imageGCConfig})
	assert.ElementsMatch(t, append([]string{"protect-kernel-defaults=true"}, thresholds...), server)

	worker := mergedKubeletArgs(t, "", map[string]string{
		imageGCDropIn:      imageGCConfig,
		agentKubeletDropIn: agentKubeletConfig,
		"kipper-dns.yaml":  "resolv-conf: " + k3sResolvConfPath + "\n",
	})
	assert.ElementsMatch(t, append([]string{"protect-kernel-defaults=true"}, thresholds...), worker)
}

func TestPrepareNodeOSSetsImageGCOnlyForLonghorn(t *testing.T) {
	debian := []route{{contains: "cat /etc/os-release", out: "ID=debian\nVERSION_ID=\"13\"\n"}}

	longhorn := &routedRunner{routes: debian}
	require.NoError(t, prepareNodeOS(longhorn, false, true))
	assert.True(t, longhorn.ran(imageGCDropIn), "Longhorn stops placing replicas at 75%% disk use\n%s", strings.Join(longhorn.commands, "\n"))

	local := &routedRunner{routes: debian}
	require.NoError(t, prepareNodeOS(local, false, false))
	assert.False(t, local.ran(imageGCDropIn), "local storage keeps the kubelet's defaults")
}

func TestInstallK3sWritesImageGCBeforeK3sStarts(t *testing.T) {
	h := &k3sInstallHost{}

	require.NoError(t, InstallK3s(h, "203.0.113.10", nil, false, false, StorageLonghorn))

	written, installed := h.index(imageGCDropIn), h.index("get.k3s.io")
	require.NotEqual(t, -1, written, "the image GC drop-in was never written")
	assert.Less(t, written, installed, "the kubelet reads its flags only when k3s starts")
}

func TestApplyImageGCRestartsOnlyAStaleK3s(t *testing.T) {
	tests := []struct {
		name     string
		stale    string
		restarts string
	}{
		{name: "the kubelet already started with the drop-in", stale: ""},
		{name: "server", stale: "k3s\n", restarts: "systemctl restart k3s"},
		{name: "worker", stale: "k3s-agent\n", restarts: "systemctl restart k3s-agent"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host := &routedRunner{routes: []route{{contains: "ExecMainStartTimestamp", out: tc.stale}}}
			var notices []string

			restarted, err := ApplyImageGC(host, func(msg string) { notices = append(notices, msg) })

			require.NoError(t, err)
			assert.True(t, host.ran(imageGCDropIn))
			assert.Equal(t, strings.TrimSpace(tc.stale), restarted, "the restarted unit is named for the operator")
			if tc.restarts == "" {
				assert.False(t, host.ran("systemctl restart"), "nothing changed since k3s started")
				assert.Empty(t, notices)
				return
			}
			assert.True(t, host.ran(tc.restarts), strings.Join(host.commands, "\n"))
			assert.Len(t, notices, 1, "the operator hears about the restart before it happens")
		})
	}
}

func TestApplyImageGCWaitsForTheAPIAfterRestartingAServer(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "ExecMainStartTimestamp", out: "k3s\n"}}}

	_, err := ApplyImageGC(host, func(string) {})

	require.NoError(t, err)
	assert.True(t, host.ran("/readyz"), "the next step needs the API server")
}

func TestApplyImageGCReportsAFailedRestart(t *testing.T) {
	for unit, fail := range map[string]string{"k3s": "systemctl restart k3s", "k3s-agent": "systemctl restart k3s-agent"} {
		host := &routedRunner{routes: []route{
			{contains: "ExecMainStartTimestamp", out: unit + "\n"},
			{contains: fail, err: errors.New("exit status 1")},
		}}

		_, err := ApplyImageGC(host, func(string) {})

		require.Error(t, err, unit)
		assert.Contains(t, err.Error(), "restarting "+unit)
		assert.Contains(t, err.Error(), "journalctl -u "+unit)
	}
}

func TestApplyImageGCRefusesUnexpectedOutput(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "ExecMainStartTimestamp", out: "k3s-server\n"}}}

	_, err := ApplyImageGC(host, func(string) {})

	assert.Error(t, err)
	assert.False(t, host.ran("systemctl restart"))
}

// gnuDate returns the path of a GNU date, or skips the test when the machine
// has none.
func gnuDate(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"date", "gdate"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if out, err := exec.Command(path, "--version").CombinedOutput(); err == nil && strings.Contains(string(out), "GNU") {
			return path
		}
	}
	t.Skip("needs GNU date")
	return ""
}

// staleUnit runs the restart check against a drop-in written at written, with
// k3s active and reporting started as its start time in systemd's format.
func staleUnit(t *testing.T, written time.Time, started string) string {
	t.Helper()
	return staleUnitOn(t, written, started, "--no-such-option")
}

// staleUnitOn is staleUnit on a systemd that rejects the option named by
// unsupported, as systemd before 248 rejects --timestamp.
func staleUnitOn(t *testing.T, written time.Time, started, unsupported string) string {
	t.Helper()
	conf := t.TempDir()
	dropIn := filepath.Join(conf, imageGCDropIn)
	require.NoError(t, os.WriteFile(dropIn, []byte(imageGCConfig), 0o600))
	require.NoError(t, os.Chtimes(dropIn, written, written))
	stubs := "date() { " + gnuDate(t) + " \"$@\"; }\n" +
		"systemctl() {\n  case \"$*\" in\n    *is-active*k3s-agent) return 3 ;;\n    *is-active*) return 0 ;;\n" +
		"    *'" + unsupported + "'*) echo unknown option >&2; return 1 ;;\n    *show*) echo '" + started + "' ;;\n  esac\n}\n"

	out, err := exec.Command("sh", "-c", stubs+imageGCStaleUnitCommand(conf)).CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func TestImageGCStaleUnitCommandComparesWithTheK3sStart(t *testing.T) {
	written := time.Date(2026, 11, 7, 12, 0, 0, 100_000_000, time.UTC)

	assert.Empty(t, staleUnit(t, written, "Sat 2026-11-07 12:00:00.900000 UTC"),
		"k3s restarted in the same second, after the write, and loaded the drop-in")
	assert.Empty(t, staleUnit(t, written, "Sat 2026-11-07 12:00:01.000000 UTC"))
	assert.Equal(t, "k3s", staleUnit(t, written, "Sat 2026-11-07 12:00:00.050000 UTC"),
		"k3s started earlier in the same second, before the write")
	assert.Equal(t, "k3s", staleUnit(t, written, "Sat 2026-11-07 11:59:59.000000 UTC"))
	assert.Equal(t, "k3s", staleUnit(t, written, ""), "an unknown start time restarts rather than guesses")
}

// systemd before 248 reports whole seconds. A start in the same second as the
// write is ambiguous there and counts as stale. ApplyImageGC waits a second
// before restarting so its own restart lands in a later second.
func TestImageGCStaleUnitCommandOnOlderSystemd(t *testing.T) {
	written := time.Date(2026, 11, 7, 12, 0, 0, 100_000_000, time.UTC)

	assert.Empty(t, staleUnitOn(t, written, "Sat 2026-11-07 12:00:01 UTC", "--timestamp"))
	assert.Equal(t, "k3s", staleUnitOn(t, written, "Sat 2026-11-07 12:00:00 UTC", "--timestamp"))
	assert.Equal(t, "k3s", staleUnitOn(t, written, "Sat 2026-11-07 11:59:59 UTC", "--timestamp"))
}

func TestApplyImageGCRestartsInALaterSecondThanTheWrite(t *testing.T) {
	host := &routedRunner{routes: []route{{contains: "ExecMainStartTimestamp", out: "k3s-agent\n"}}}

	_, err := ApplyImageGC(host, func(string) {})

	require.NoError(t, err)
	wait, restart := -1, -1
	for i, c := range host.commands {
		if c == "sleep 1" && wait < 0 {
			wait = i
		}
		if strings.Contains(c, "systemctl restart") {
			restart = i
		}
	}
	assert.True(t, wait >= 0 && wait < restart, "a restart in the write's second reads as stale on systemd before 248\n%s", strings.Join(host.commands, "\n"))
}
