package installer

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const k3sResolvConfCommand = "cat " + k3sResolvConfPath

const (
	checkBeginMarker = "KIP-HOST-CHECK-BEGIN"
	checkEndMarker   = "KIP-HOST-CHECK-END"
)

// ReadResolvConf reads the host's curated resolv.conf used by CoreDNS.
// It returns the raw command output and any runner error without validating the file.
func ReadResolvConf(client CommandRunner) (string, error) {
	return client.Run(k3sResolvConfCommand)
}

// liveDNSProbeCommand probes the resolvers in the host's own curated
// resolv.conf, in file order, printing the same lines as dnsProbeCommand.
// Values other than digits and dots are skipped, so nothing read from the file
// reaches the shell as code.
var liveDNSProbeCommand = fmt.Sprintf(
	`for r in $(awk '$1=="nameserver"{print $2}' %s 2>/dev/null); do `+
		`case "$r" in ''|*[!0-9.]*) continue;; esac; `+
		`if timeout %d bash -c ":< /dev/tcp/$r/53" 2>/dev/null; then echo "$r ok"; else echo "$r unreachable"; fi; done`,
	k3sResolvConfPath, dnsProbeTimeoutSeconds)

// hostCheckCommands lists the read-only commands behind kip status's host checks.
var hostCheckCommands = []string{
	machineIDCommand,
	osReleaseCommand,
	needrestartCommand,
	dnfNeedsRestartingCommand,
	mountsCommand,
	ufwFailOpenCommand,
	k3sResolvConfCommand,
	liveDNSProbeCommand,
	accessStateCommand(""),
	opsStateCommand(""),
}

// CaptureHostChecks runs every host check command on client as one remote
// command and returns a runner that answers those commands from the captured
// results, including combined stdout and stderr and each command's exit status.
// Run accepts captured commands, including the ProbeResolvers command when the
// captured resolver file is valid.
// Other commands and all RunStdin calls return errors without contacting the host.
// A batch execution failure returns an error here. Individual command failures
// or malformed results are returned when the captured command is replayed.
func CaptureHostChecks(client CommandRunner) (CommandRunner, error) {
	results, err := captureCommands(client, hostCheckCommands)
	if err != nil {
		return nil, err
	}
	return replayFrom(results), nil
}

// replayFrom answers ProbeResolvers for the captured resolv.conf from the live
// probe, because ProbeResolvers names the resolvers it read in its command.
func replayFrom(results map[string]commandResult) *capturedRunner {
	body, probe := results[k3sResolvConfCommand], results[liveDNSProbeCommand]
	if body.err == nil {
		if live, err := CheckResolvConf(body.out); err == nil {
			results[dnsProbeCommand(live)] = probe
		}
	}
	return &capturedRunner{results: results}
}

type commandResult struct {
	out string
	err error
}

// capturedRunner replays the results of a batch run by captureCommands.
type capturedRunner struct {
	results map[string]commandResult
}

func (c *capturedRunner) Run(command string) (string, error) {
	r, ok := c.results[command]
	if !ok {
		return "", fmt.Errorf("%q is not in the batch of host checks", command)
	}
	return r.out, r.err
}

func (c *capturedRunner) RunStdin(command string, _ io.Reader) (string, error) {
	return "", fmt.Errorf("%q is not in the batch of host checks", command)
}

// captureCommands runs commands in one remote shell, each in a subshell with
// stderr merged into stdout, and frames each output with markers that carry
// its exit status.
func captureCommands(client CommandRunner, commands []string) (map[string]commandResult, error) {
	var script strings.Builder
	for i, c := range commands {
		fmt.Fprintf(&script, "printf '%s %d\\n'; ( %s\n) 2>&1; printf '\\n%s %d %%s\\n' \"$?\"; ", checkBeginMarker, i, c, checkEndMarker, i)
	}
	script.WriteString("true")
	out, err := client.Run(script.String())
	if err != nil {
		return nil, err
	}

	results := make(map[string]commandResult, len(commands))
	for i, c := range commands {
		results[c] = parseCapturedCommand(out, i)
	}
	return results, nil
}

// parseCapturedCommand extracts command i's output and exit status from a
// batch's output. A missing or malformed frame is reported as an error.
func parseCapturedCommand(out string, i int) commandResult {
	begin := fmt.Sprintf("%s %d\n", checkBeginMarker, i)
	start := strings.Index(out, begin)
	if start < 0 {
		return commandResult{err: errors.New("the host returned no result for this check")}
	}
	rest := out[start+len(begin):]
	end := strings.Index(rest, fmt.Sprintf("\n%s %d ", checkEndMarker, i))
	if end < 0 {
		return commandResult{err: errors.New("the host returned an incomplete result for this check")}
	}
	body := rest[:end]
	statusLine, _, _ := strings.Cut(rest[end+1:], "\n")
	status, err := strconv.Atoi(strings.TrimPrefix(statusLine, fmt.Sprintf("%s %d ", checkEndMarker, i)))
	switch {
	case err != nil:
		return commandResult{out: body, err: errors.New("the host returned no exit status for this check")}
	case status != 0:
		return commandResult{out: body, err: fmt.Errorf("exit status %d", status)}
	}
	return commandResult{out: body}
}
