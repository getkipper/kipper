package installer

import (
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fixtures in testdata/fio are reconstructed, not captured: the layout
// follows fio 3.36's JSON writer in stat.c (show_run_stats' top-level object,
// show_thread_status_json, add_ddir_status_json and add_ddir_lat_json, with
// percentile keys printed "%f" from the default percentile_list in options.c),
// and the figures are those of two calibration runs: a local SSD that finished
// all 10029 blocks, and a network block volume stopped by the 60 second cap.
func readFioFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "fio", name))
	require.NoError(t, err)
	return raw
}

func TestParseFioSyncReadsACompletedRun(t *testing.T) {
	run, err := ParseFioSync(readFioFixture(t, "etcd-complete.json"))
	require.NoError(t, err)
	assert.InDelta(t, 4.42368, run.P99Ms, 1e-9)
	assert.InDelta(t, 21.092242, run.MaxMs, 1e-9)
	assert.EqualValues(t, 23066700, run.IOBytes)
	assert.False(t, run.CapHit, "all 10029 whole blocks were written")
}

func TestParseFioSyncSeesTheRuntimeCap(t *testing.T) {
	run, err := ParseFioSync(readFioFixture(t, "etcd-capped.json"))
	require.NoError(t, err)
	assert.InDelta(t, 20.31616, run.P99Ms, 1e-9)
	assert.InDelta(t, 848.287737, run.MaxMs, 1e-9)
	assert.True(t, run.CapHit)
}

// One block short of the complete total is a run the cap stopped, even though
// it is within a block of 22 MiB.
func TestParseFioSyncCountsOneMissingBlockAsTheCap(t *testing.T) {
	raw := strings.Replace(string(readFioFixture(t, "etcd-complete.json")), `"io_bytes": 23066700`, `"io_bytes": 23064400`, 1)
	run, err := ParseFioSync([]byte(raw))
	require.NoError(t, err)
	assert.True(t, run.CapHit)
}

// fio prints notes on the stream that carries its JSON.
func TestParseFioSyncSkipsANoteBeforeTheJSON(t *testing.T) {
	raw := append([]byte("note: a fio informational line\n"), readFioFixture(t, "etcd-complete.json")...)
	run, err := ParseFioSync(raw)
	require.NoError(t, err)
	assert.InDelta(t, 4.42368, run.P99Ms, 1e-9)
}

func TestParseFioSyncFailsClosed(t *testing.T) {
	cases := map[string]string{
		"not JSON":           "fio: engine sync not loadable",
		"no jobs":            `{"fio version": "fio-3.36", "jobs": []}`,
		"no sync block":      `{"jobs": [{"write": {"io_bytes": 23066700}}]}`,
		"no percentiles":     `{"jobs": [{"write": {"io_bytes": 23066700}, "sync": {"total_ios": 0, "lat_ns": {"min": 0, "max": 0, "N": 0}}}]}`,
		"no 99th percentile": `{"jobs": [{"write": {"io_bytes": 23066700}, "sync": {"lat_ns": {"max": 5, "percentile": {"50.000000": 1}}}}]}`,
		"no write block":     `{"jobs": [{"sync": {"lat_ns": {"max": 5, "percentile": {"99.000000": 4}}}}]}`,
		"no max":             `{"jobs": [{"write": {"io_bytes": 23066700}, "sync": {"lat_ns": {"percentile": {"99.000000": 4}}}}]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseFioSync([]byte(raw))
			assert.Error(t, err)
		})
	}
}

func fastRun() FioSyncRun { return FioSyncRun{P99Ms: 4.4, MaxMs: 21, IOBytes: fioCompleteBytes} }

func fastUnderlay() *RTTSample { return &RTTSample{P99Ms: 1.6, MaxMs: 12.5, Sent: 200} }

func TestDiskGatePassesALocalSSD(t *testing.T) {
	r := EvaluateDiskGate([]FioSyncRun{fastRun(), fastRun()}, DefaultEngineReplicaTimeout, fastUnderlay(), false)
	assert.True(t, r.Passed)
	assert.False(t, r.Overridden)
	assert.Empty(t, r.Refusals)
	assert.Empty(t, r.Warnings)
	require.NotNil(t, r.UnderlayRTTP99Ms)
	assert.InDelta(t, 1.6, *r.UnderlayRTTP99Ms, 1e-9)
}

func TestDiskGateTakesTheWorstOfBothRuns(t *testing.T) {
	slow := FioSyncRun{P99Ms: 25, MaxMs: 300, IOBytes: fioCompleteBytes}
	r := EvaluateDiskGate([]FioSyncRun{fastRun(), slow}, DefaultEngineReplicaTimeout, fastUnderlay(), false)
	assert.False(t, r.Passed)
	assert.InDelta(t, 25, r.P99Ms, 1e-9)
	assert.InDelta(t, 300, r.MaxMs, 1e-9)
	assert.Len(t, r.Refusals, 1)
}

func TestDiskGateNeedsTwoRuns(t *testing.T) {
	r := EvaluateDiskGate([]FioSyncRun{fastRun()}, DefaultEngineReplicaTimeout, fastUnderlay(), true)
	assert.False(t, r.Passed, "a missing run is never overridden")
	assert.True(t, r.Hard)
}

func TestDiskGateOverridableRefusals(t *testing.T) {
	cases := map[string]FioSyncRun{
		"p99 over 20 ms":     {P99Ms: 20.4, MaxMs: 200, IOBytes: fioCompleteBytes},
		"a sync over 1 s":    {P99Ms: 5, MaxMs: 1040, IOBytes: fioCompleteBytes},
		"the 60 s cap reach": {P99Ms: 5, MaxMs: 30, IOBytes: 8997600, CapHit: true},
	}
	for name, slow := range cases {
		t.Run(name, func(t *testing.T) {
			refused := EvaluateDiskGate([]FioSyncRun{fastRun(), slow}, DefaultEngineReplicaTimeout, fastUnderlay(), false)
			assert.False(t, refused.Passed)
			assert.False(t, refused.Hard)
			assert.Len(t, refused.Refusals, 1)

			overridden := EvaluateDiskGate([]FioSyncRun{fastRun(), slow}, DefaultEngineReplicaTimeout, fastUnderlay(), true)
			assert.True(t, overridden.Passed)
			assert.True(t, overridden.Overridden)
			assert.Equal(t, refused.Refusals, overridden.Refusals, "the override keeps what it overrode")
		})
	}
}

func TestDiskGateHardRefusalAtHalfTheEngineTimeout(t *testing.T) {
	stall := FioSyncRun{P99Ms: 5, MaxMs: 4000, IOBytes: fioCompleteBytes}
	r := EvaluateDiskGate([]FioSyncRun{fastRun(), stall}, DefaultEngineReplicaTimeout, fastUnderlay(), true)
	assert.False(t, r.Passed, "--ignore-slow-disk never overrides a stall Longhorn would fail a replica for")
	assert.True(t, r.Hard)
	assert.False(t, r.Overridden)

	under := FioSyncRun{P99Ms: 5, MaxMs: 3999, IOBytes: fioCompleteBytes}
	r = EvaluateDiskGate([]FioSyncRun{fastRun(), under}, DefaultEngineReplicaTimeout, fastUnderlay(), true)
	assert.False(t, r.Hard)
	assert.True(t, r.Passed)
}

func TestDiskGateFollowsTheLiveEngineTimeout(t *testing.T) {
	stall := FioSyncRun{P99Ms: 5, MaxMs: 2600, IOBytes: fioCompleteBytes}
	r := EvaluateDiskGate([]FioSyncRun{fastRun(), stall}, 5*time.Second, fastUnderlay(), true)
	assert.True(t, r.Hard)
}

func TestDiskGateWarnings(t *testing.T) {
	warm := FioSyncRun{P99Ms: 12, MaxMs: 40, IOBytes: fioCompleteBytes}
	r := EvaluateDiskGate([]FioSyncRun{fastRun(), warm}, DefaultEngineReplicaTimeout, &RTTSample{P99Ms: 26.8, MaxMs: 29.2, Sent: 200}, false)
	assert.True(t, r.Passed)
	assert.Len(t, r.Warnings, 2, "slow-ish syncs and a slow underlay each warn")

	r = EvaluateDiskGate([]FioSyncRun{fastRun(), fastRun()}, DefaultEngineReplicaTimeout, nil, false)
	assert.True(t, r.Passed, "a missing RTT measurement never refuses")
	assert.Len(t, r.Warnings, 1)
	assert.Nil(t, r.UnderlayRTTP99Ms)

	r = EvaluateDiskGate([]FioSyncRun{fastRun(), fastRun()}, DefaultEngineReplicaTimeout, &RTTSample{Sent: 200, Lost: 200}, false)
	assert.True(t, r.Passed)
	assert.Nil(t, r.UnderlayRTTP99Ms, "no connect succeeded, so there is no figure")
	assert.Len(t, r.Warnings, 1)
}

func TestSplitDiskGateOutput(t *testing.T) {
	complete := string(readFioFixture(t, "etcd-complete.json"))
	out := "\n" + diskGateMarker + "\n" + complete + diskGateMarker + "\n" + complete
	runs, err := SplitDiskGateOutput(out)
	require.NoError(t, err)
	require.Len(t, runs, 2)
	for _, raw := range runs {
		_, err := ParseFioSync(raw)
		require.NoError(t, err)
	}

	_, err = SplitDiskGateOutput(diskGateMarker + "\n" + complete)
	assert.Error(t, err, "one run is not enough")
}

type gateHost struct {
	out  string
	err  error
	cmds []string
}

func (h *gateHost) Run(command string) (string, error) {
	h.cmds = append(h.cmds, command)
	return h.out, h.err
}

func (h *gateHost) RunStdin(command string, _ io.Reader) (string, error) { return h.Run(command) }

func TestRunDiskGateReturnsBothRuns(t *testing.T) {
	complete := string(readFioFixture(t, "etcd-complete.json"))
	capped := string(readFioFixture(t, "etcd-capped.json"))
	host := &gateHost{out: diskGateMarker + "\n" + complete + diskGateMarker + "\n" + capped}
	runs, err := RunDiskGate(host)
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.False(t, runs[0].CapHit)
	assert.True(t, runs[1].CapHit)
	require.Len(t, host.cmds, 1)
	assert.Equal(t, DiskGateCommand(), host.cmds[0])
}

func TestRunDiskGateFailsClosed(t *testing.T) {
	_, err := RunDiskGate(&gateHost{out: "fio: command not found", err: errors.New("exit status 127")})
	assert.Error(t, err)

	_, err = RunDiskGate(&gateHost{out: diskGateMarker + "\n{}\n" + diskGateMarker + "\n{}\n"})
	assert.Error(t, err, "output without figures is a failure, not a pass")
}

func TestDiskGateCommandShape(t *testing.T) {
	cmd := DiskGateCommand()
	for _, want := range []string{
		"dir=/var/lib/longhorn\n",
		`mkdir -p "$dir"`,
		"trap ",
		"--name=kipper-disk-gate --directory=/var/lib/longhorn --rw=write --ioengine=sync --fdatasync=1 --size=22m --bs=2300 --runtime=60 --unlink=1 --output-format=json",
		"timeout ",
		diskGateMarker,
	} {
		assert.Contains(t, cmd, want)
	}
}

// The command runs as written: a stub fio stands in for the real one, and
// the script runs it twice, separates the runs and cleans up after itself.
func TestDiskGateCommandRunsTwiceAndRemovesItsFile(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	data := filepath.Join(dir, "longhorn")
	require.NoError(t, os.MkdirAll(bin, 0o700))
	fixture := filepath.Join("testdata", "fio", "etcd-complete.json")
	abs, err := filepath.Abs(fixture)
	require.NoError(t, err)
	stub := "#!/bin/sh\necho fio-stub-warning >&2\ntouch " + data + "/kipper-disk-gate.0.0\ncat " + abs + "\n"
	//nolint:gosec // G306: the stub stands in for fio on PATH, so it has to be executable.
	require.NoError(t, os.WriteFile(filepath.Join(bin, "fio"), []byte(stub), 0o700))

	script := strings.ReplaceAll(DiskGateCommand(), diskGateDir, data)
	cmd := exec.Command(bash, "-c", script) //nolint:gosec // This test runs a fixed script with a stubbed fio.
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	runs, err := SplitDiskGateOutput(string(out))
	require.NoError(t, err, string(out))
	require.Len(t, runs, 2)
	assert.NotContains(t, string(out), "fio-stub-warning", "fio's stderr stays out of a passing run's output")
	left, err := filepath.Glob(filepath.Join(data, "kipper-disk-gate*"))
	require.NoError(t, err)
	assert.Empty(t, left, "the test file is removed")
}

func TestDiskGateCommandReportsAFailedRun(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o700))
	//nolint:gosec // G306: the stub stands in for fio on PATH, so it has to be executable.
	require.NoError(t, os.WriteFile(filepath.Join(bin, "fio"), []byte("#!/bin/sh\necho 'fio: no space left' >&2\nexit 1\n"), 0o700))

	script := strings.ReplaceAll(DiskGateCommand(), diskGateDir, filepath.Join(dir, "longhorn"))
	cmd := exec.Command(bash, "-c", script) //nolint:gosec // This test runs a fixed script with a stubbed fio.
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(out), "fio: no space left", "the failure says why")
}

func TestUnderlayRTTCommandRefusesANonAddress(t *testing.T) {
	for _, addr := range []string{"", "server-1", "10.0.0.1; reboot", "$(id)"} {
		_, err := UnderlayRTTCommand(addr, 10250)
		assert.Error(t, err, addr)
	}
	_, err := UnderlayRTTCommand("192.0.2.10", 0)
	assert.Error(t, err)

	cmd, err := UnderlayRTTCommand("192.0.2.10", 10250)
	require.NoError(t, err)
	assert.Contains(t, cmd, "/dev/tcp/192.0.2.10/10250")
	assert.Contains(t, cmd, "timeout ")

	_, err = UnderlayRTTCommand("2001:db8::1", 10250)
	assert.NoError(t, err)
}

func TestParseRTT(t *testing.T) {
	var lines []string
	for i := 1; i <= 197; i++ {
		lines = append(lines, "700")
	}
	lines = append(lines, "1650", "1650", "12500")
	s, err := ParseRTT(strings.Join(lines, "\n") + "\n")
	require.NoError(t, err)
	assert.Equal(t, 200, s.Sent)
	assert.Zero(t, s.Lost)
	assert.InDelta(t, 1.65, s.P99Ms, 1e-9)
	assert.InDelta(t, 12.5, s.MaxMs, 1e-9)

	s, err = ParseRTT("700\nlost\n900\n")
	require.NoError(t, err)
	assert.Equal(t, rttConnects, s.Sent, "connects the timeout cut off count as lost")
	assert.Equal(t, rttConnects-2, s.Lost)

	_, err = ParseRTT("bash: /dev/tcp: not supported\n")
	assert.Error(t, err)
}

// The probe runs against a listener on this machine and reports each connect.
func TestUnderlayRTTCommandMeasuresALocalListener(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	if out, err := exec.Command(bash, "-c", `[ -n "${EPOCHREALTIME:-}" ] && echo yes`).Output(); err != nil || strings.TrimSpace(string(out)) != "yes" {
		t.Skip("this bash has no EPOCHREALTIME")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	cmd, err := UnderlayRTTCommand("127.0.0.1", ln.Addr().(*net.TCPAddr).Port)
	require.NoError(t, err)
	out, err := exec.Command(bash, "-c", cmd).CombinedOutput() //nolint:gosec // The command is built from a validated loopback address.
	require.NoError(t, err, string(out))
	s, err := ParseRTT(string(out))
	require.NoError(t, err, string(out))
	assert.Equal(t, rttConnects, s.Sent)
	assert.Zero(t, s.Lost)
	assert.Positive(t, s.MaxMs)
}
