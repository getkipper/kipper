package installer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultEngineReplicaTimeout is Longhorn's engine-replica-timeout when the
// cluster's setting cannot be read.
const DefaultEngineReplicaTimeout = 8 * time.Second

const (
	diskGateDir    = "/var/lib/longhorn"
	diskGateMarker = "--- kipper-disk-gate run ---"
	diskGateRuns   = 2

	// fio writes whole 2300-byte blocks and never a short last one, so a run
	// that finishes its 22 MiB writes 10029 blocks.
	fioCompleteBytes = 10029 * 2300

	diskGateP99RefuseMs  = 20.0
	diskGateP99WarnMs    = 10.0
	diskGateStallRefuse  = time.Second
	underlayRTTWarnMs    = 2.0
	rttConnects          = 200
	rttTimeoutSeconds    = 60
	fioRunTimeoutSeconds = 150
)

// FioSyncRun contains one fio run's sync latencies in milliseconds and bytes
// written. CapHit indicates fewer than 10029 complete 2300-byte blocks, the
// expected output of the 22 MiB test within its 60 second runtime cap.
type FioSyncRun struct {
	P99Ms   float64
	MaxMs   float64
	IOBytes int64
	CapHit  bool
}

// RTTSample is a round-trip measurement in milliseconds over Sent attempts,
// Lost of which failed or never ran.
type RTTSample struct {
	P99Ms float64
	MaxMs float64
	Sent  int
	Lost  int
}

// DiskGateResult contains the disk-check verdict and measurements. Refusals
// contains the limits reported before evaluation stopped, including those
// accepted by an override. UnderlayRTTP99Ms is nil without a successful probe.
type DiskGateResult struct {
	Passed           bool
	Hard             bool
	Overridden       bool
	Refusals         []string
	Warnings         []string
	P99Ms            float64
	MaxMs            float64
	CapHit           bool
	UnderlayRTTP99Ms *float64
}

type fioLat struct {
	Max        *int64           `json:"max"`
	Percentile map[string]int64 `json:"percentile"`
}

type fioOutput struct {
	Jobs []struct {
		Write *struct {
			IOBytes *int64 `json:"io_bytes"`
		} `json:"write"`
		Sync *struct {
			LatNs *fioLat `json:"lat_ns"`
		} `json:"sync"`
	} `json:"jobs"`
}

// ParseFioSync reads the first job in fio's JSON output and converts sync
// latencies from nanoseconds to milliseconds. It skips fio notes before the
// JSON object and returns an error for invalid JSON or missing measurements.
// CapHit is inferred from the number of complete blocks written.
func ParseFioSync(raw []byte) (FioSyncRun, error) {
	start := bytes.Index(raw, []byte("\n{"))
	switch {
	case bytes.HasPrefix(raw, []byte("{")):
		start = 0
	case start >= 0:
		start++
	default:
		return FioSyncRun{}, fmt.Errorf("fio printed no JSON: %s", firstLine(string(raw)))
	}
	var out fioOutput
	if err := json.NewDecoder(bytes.NewReader(raw[start:])).Decode(&out); err != nil {
		return FioSyncRun{}, fmt.Errorf("reading fio's JSON: %w", err)
	}
	if len(out.Jobs) == 0 {
		return FioSyncRun{}, errors.New("fio reported no jobs")
	}
	job := out.Jobs[0]
	if job.Sync == nil || job.Sync.LatNs == nil {
		return FioSyncRun{}, errors.New("fio reported no sync latency")
	}
	p99, ok := job.Sync.LatNs.Percentile["99.000000"]
	if !ok {
		return FioSyncRun{}, errors.New("fio reported no 99th percentile sync latency")
	}
	if job.Sync.LatNs.Max == nil {
		return FioSyncRun{}, errors.New("fio reported no maximum sync latency")
	}
	if job.Write == nil || job.Write.IOBytes == nil {
		return FioSyncRun{}, errors.New("fio reported no bytes written")
	}
	return FioSyncRun{
		P99Ms:   float64(p99) / 1e6,
		MaxMs:   float64(*job.Sync.LatNs.Max) / 1e6,
		IOBytes: *job.Write.IOBytes,
		CapHit:  *job.Write.IOBytes < fioCompleteBytes,
	}, nil
}

// EvaluateDiskGate evaluates the worst measurements across the fio runs.
// Fewer than two runs or a sync stall of at least half engineTimeout causes
// an immediate refusal that ignoreSlowDisk cannot override. A non-positive
// engineTimeout uses DefaultEngineReplicaTimeout. Other disk limits may be
// overridden when ignoreSlowDisk is true, which sets Overridden. A nil or
// failed underlay measurement only warns after the checks that cause an
// immediate refusal.
func EvaluateDiskGate(runs []FioSyncRun, engineTimeout time.Duration, underlay *RTTSample, ignoreSlowDisk bool) DiskGateResult {
	if engineTimeout <= 0 {
		engineTimeout = DefaultEngineReplicaTimeout
	}
	var r DiskGateResult
	for _, run := range runs {
		r.P99Ms = math.Max(r.P99Ms, run.P99Ms)
		r.MaxMs = math.Max(r.MaxMs, run.MaxMs)
		r.CapHit = r.CapHit || run.CapHit
	}
	if underlay != nil && underlay.Lost < underlay.Sent {
		p99 := underlay.P99Ms
		r.UnderlayRTTP99Ms = &p99
	}

	if len(runs) < diskGateRuns {
		r.Hard = true
		r.Refusals = append(r.Refusals, fmt.Sprintf("The disk check needs %d fio runs and got %d.", diskGateRuns, len(runs)))
		return r
	}
	stall := time.Duration(r.MaxMs * float64(time.Millisecond))
	if stall >= engineTimeout/2 {
		r.Hard = true
		r.Refusals = append(r.Refusals, fmt.Sprintf("The slowest sync took %.0f ms, at least half of Longhorn's %s engine replica timeout, so Longhorn would fail a copy on this disk during a stall.", r.MaxMs, engineTimeout))
		return r
	}

	if r.P99Ms > diskGateP99RefuseMs {
		r.Refusals = append(r.Refusals, fmt.Sprintf("1 in 100 syncs took %.1f ms or longer, above the %.0f ms limit.", r.P99Ms, diskGateP99RefuseMs))
	} else if r.P99Ms > diskGateP99WarnMs {
		r.Warnings = append(r.Warnings, fmt.Sprintf("1 in 100 syncs took %.1f ms or longer, above the %.0f ms Kipper recommends.", r.P99Ms, diskGateP99WarnMs))
	}
	if stall > diskGateStallRefuse {
		r.Refusals = append(r.Refusals, fmt.Sprintf("The slowest sync took %.0f ms, above the %s limit.", r.MaxMs, diskGateStallRefuse))
	}
	if r.CapHit {
		r.Refusals = append(r.Refusals, "fio did not finish writing 22 MiB within its 60 second limit.")
	}

	switch {
	case r.UnderlayRTTP99Ms == nil:
		r.Warnings = append(r.Warnings, "The network round trip to the server could not be measured.")
	case *r.UnderlayRTTP99Ms > underlayRTTWarnMs:
		r.Warnings = append(r.Warnings, fmt.Sprintf("1 in 100 connections to the server took %.1f ms or longer, above the %.0f ms Kipper recommends for nodes that hold copies.", *r.UnderlayRTTP99Ms, underlayRTTWarnMs))
	}
	if r.UnderlayRTTP99Ms != nil && underlay.Lost > 0 {
		r.Warnings = append(r.Warnings, fmt.Sprintf("%d of %d connections to the server failed.", underlay.Lost, underlay.Sent))
	}

	r.Passed = len(r.Refusals) == 0 || ignoreSlowDisk
	r.Overridden = len(r.Refusals) > 0 && ignoreSlowDisk
	return r
}

// DiskGateCommand returns a shell script that runs fio's etcd recipe twice in
// /var/lib/longhorn, with each run's JSON following a marker line. It prints
// fio's stderr on failure and uses an exit trap to remove the test and stderr
// files. Each run has a 60 second fio limit and a 150 second command timeout.
func DiskGateCommand() string {
	fio := "fio --name=kipper-disk-gate --directory=" + diskGateDir +
		" --rw=write --ioengine=sync --fdatasync=1 --size=22m --bs=2300 --runtime=60 --unlink=1 --output-format=json"
	return fmt.Sprintf(`dir=%s
mkdir -p "$dir" || exit 1
err=$(mktemp) || exit 1
trap 'rm -f "$err" "$dir"/kipper-disk-gate.*' EXIT
for run in 1 2; do
  echo '%s'
  timeout %d %s 2>"$err" || { rc=$?; cat "$err"; exit "$rc"; }
done
`, diskGateDir, diskGateMarker, fioRunTimeoutSeconds, fio)
}

// SplitDiskGateOutput returns the fio output of each run in DiskGateCommand's
// output, and an error unless there are exactly two.
func SplitDiskGateOutput(out string) ([][]byte, error) {
	parts := strings.Split(out, diskGateMarker+"\n")
	runs := make([][]byte, 0, len(parts))
	for _, p := range parts[1:] {
		runs = append(runs, []byte(p))
	}
	if len(runs) != diskGateRuns {
		return nil, fmt.Errorf("the disk check printed %d fio runs, expected %d", len(runs), diskGateRuns)
	}
	return runs, nil
}

// RunDiskGate runs the disk check on the host behind client and returns both
// runs. Any failure to run or read fio is an error.
func RunDiskGate(client CommandRunner) ([]FioSyncRun, error) {
	out, err := client.Run(DiskGateCommand())
	if err != nil {
		return nil, fmt.Errorf("running the disk check: %w", err)
	}
	parts, err := SplitDiskGateOutput(out)
	if err != nil {
		return nil, err
	}
	runs := make([]FioSyncRun, 0, len(parts))
	for _, p := range parts {
		run, err := ParseFioSync(p)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// UnderlayRTTCommand returns a Bash command that attempts 200 TCP connections
// to addr:port within 60 seconds. It prints each duration in microseconds or
// "lost". Invalid IP addresses and ports outside 1 to 65535 return an error.
func UnderlayRTTCommand(addr string, port int) (string, error) {
	ip := net.ParseIP(addr)
	if ip == nil {
		return "", fmt.Errorf("%q is not an IP address", addr)
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("port %d is out of range", port)
	}
	loop := fmt.Sprintf(`for ((i = 0; i < %d; i++)); do s=$EPOCHREALTIME; if : 2>/dev/null <>/dev/tcp/%s/%d; then e=$EPOCHREALTIME; echo $(( ${e/[.,]/} - ${s/[.,]/} )); else echo lost; fi; done`,
		rttConnects, ip.String(), port)
	return fmt.Sprintf("timeout %d bash -c %s", rttTimeoutSeconds, shellQuote(loop)), nil
}

// ParseRTT reads UnderlayRTTCommand's output and returns the nearest-rank p99
// and maximum successful connection time in milliseconds. Missing attempts,
// including those cut off by the timeout, count as lost out of at least 200.
// Output without a non-negative duration or "lost" line returns an error.
func ParseRTT(out string) (RTTSample, error) {
	var micros []int64
	lost, lines := 0, 0
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "lost" {
			lost++
			lines++
			continue
		}
		if v, err := strconv.ParseInt(line, 10, 64); err == nil && v >= 0 {
			micros = append(micros, v)
			lines++
		}
	}
	if lines == 0 {
		return RTTSample{}, fmt.Errorf("the round-trip probe printed no measurement: %s", firstLine(out))
	}
	s := RTTSample{Sent: max(rttConnects, lines)}
	s.Lost = s.Sent - len(micros)
	if len(micros) > 0 {
		slices.Sort(micros)
		rank := int(math.Ceil(0.99*float64(len(micros)))) - 1
		s.P99Ms = float64(micros[rank]) / 1000
		s.MaxMs = float64(micros[len(micros)-1]) / 1000
	}
	return s, nil
}

// MeasureUnderlayRTT runs UnderlayRTTCommand on the host behind client.
// It returns an error for invalid arguments or output without a measurement.
// Partial measurements are accepted even when the command fails, including
// when the probe's timeout expires.
func MeasureUnderlayRTT(client CommandRunner, addr string, port int) (RTTSample, error) {
	cmd, err := UnderlayRTTCommand(addr, port)
	if err != nil {
		return RTTSample{}, err
	}
	out, runErr := client.Run(cmd)
	s, err := ParseRTT(out)
	if err != nil {
		if runErr != nil {
			return RTTSample{}, fmt.Errorf("measuring the round trip to %s: %w", addr, runErr)
		}
		return RTTSample{}, err
	}
	return s, nil
}
