package installer

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// localShell tests batch framing in a real Bash process.
type localShell struct{ calls int }

func (l *localShell) Run(command string) (string, error) {
	l.calls++
	out, err := exec.Command("bash", "-c", command).CombinedOutput()
	return string(out), err
}

func (l *localShell) RunStdin(command string, _ io.Reader) (string, error) {
	return l.Run(command)
}

// Batching limits new SSH connections against UFW's rate limit while preserving
// each command's exact output and exit status.
func TestCaptureCommandsReplaysEachCommandFromOneRun(t *testing.T) {
	shell := &localShell{}
	commands := []string{
		"printf 'no trailing newline'",
		"printf 'two\\nlines\\n'",
		"echo to stderr >&2; exit 3",
		"true",
	}

	results, err := captureCommands(shell, commands)
	if err != nil {
		t.Fatalf("captureCommands: %v", err)
	}
	if shell.calls != 1 {
		t.Fatalf("ran %d commands on the host, want 1", shell.calls)
	}

	for _, tc := range []struct {
		command string
		out     string
		fails   bool
	}{
		{commands[0], "no trailing newline", false},
		{commands[1], "two\nlines\n", false},
		{commands[2], "to stderr\n", true},
		{commands[3], "", false},
	} {
		out, err := (&capturedRunner{results: results}).Run(tc.command)
		if out != tc.out {
			t.Errorf("%q: output %q, want %q", tc.command, out, tc.out)
		}
		if (err != nil) != tc.fails {
			t.Errorf("%q: error %v, want failure %v", tc.command, err, tc.fails)
		}
	}
	if shell.calls != 1 {
		t.Errorf("replaying reached the host again: %d runs", shell.calls)
	}
}

// A check that asks for something outside the batch must fail, not open a
// connection of its own behind the caller's back.
func TestCapturedRunnerRefusesCommandsOutsideTheBatch(t *testing.T) {
	shell := &localShell{}
	results, err := captureCommands(shell, []string{"true"})
	if err != nil {
		t.Fatalf("captureCommands: %v", err)
	}
	replay := replayFrom(results)
	if _, err := replay.Run("uname -a"); err == nil {
		t.Error("a command outside the batch was answered")
	}
	if _, err := replay.RunStdin("true", strings.NewReader("")); err == nil {
		t.Error("RunStdin was answered from the batch")
	}
	if shell.calls != 1 {
		t.Errorf("ran %d commands on the host, want 1", shell.calls)
	}
}

// A transport failure is the host not being checked at all, which the caller
// has to be able to tell apart from a check that ran and failed.
func TestCaptureCommandsReturnsTheTransportError(t *testing.T) {
	_, err := captureCommands(&scriptedRunner{err: io.ErrUnexpectedEOF}, []string{"true"})
	if err == nil {
		t.Fatal("a failed batch returned a runner")
	}
}

// Every reader kip status uses on a worker must find its command in the batch.
// A reader whose command drifted would quietly turn into "not checked".
func TestHostCheckBatchCoversEveryWorkerCheck(t *testing.T) {
	for _, osRelease := range []string{"ID=debian\n", "ID=rocky\n"} {
		canned := map[string]string{
			osReleaseCommand:     osRelease,
			machineIDCommand:     "0123456789abcdef0123456789abcdef\n",
			k3sResolvConfCommand: "nameserver 192.0.2.53\nnameserver 198.51.100.53\n",
			liveDNSProbeCommand:  "192.0.2.53 ok\n198.51.100.53 unreachable\n",
		}
		results := map[string]commandResult{}
		for _, c := range hostCheckCommands {
			results[c] = commandResult{out: canned[c]}
		}
		replay := replayFrom(results)

		if _, err := PendingRestarts(replay); err != nil && strings.Contains(err.Error(), "not in the batch") {
			t.Errorf("%s: pending restarts: %v", strings.TrimSpace(osRelease), err)
		}
		if _, err := ReadOnlyVolumeMounts(replay); err != nil {
			t.Errorf("mounts: %v", err)
		}
		if _, err := UFWFailOpen(replay); err != nil {
			t.Errorf("ufw marker: %v", err)
		}
		if _, err := ReadMachineID(replay); err != nil {
			t.Errorf("machine id: %v", err)
		}
		if _, err := ReadAccessGuardState(replay); err != nil && strings.Contains(err.Error(), "not in the batch") {
			t.Errorf("access guard: %v", err)
		}
		if _, err := ReadOpsState(replay); err != nil {
			t.Errorf("operator interface: %v", err)
		}
		body, err := ReadResolvConf(replay)
		if err != nil {
			t.Fatalf("resolv.conf: %v", err)
		}
		live, err := CheckResolvConf(body)
		if err != nil {
			t.Fatalf("resolv.conf: %v", err)
		}
		unreachable, err := ProbeResolvers(replay, live)
		if err != nil {
			t.Errorf("resolver probe: %v", err)
		}
		if len(unreachable) != 1 || unreachable[0] != "198.51.100.53" {
			t.Errorf("unreachable = %v, want the live file's second resolver", unreachable)
		}
	}
}

// The probe covers the resolvers the worker actually uses, read from its own
// file in the same batch, whatever the operator's config says.
func TestLiveDNSProbeReadsTheWorkersOwnFile(t *testing.T) {
	dir := t.TempDir()
	file := dir + "/resolv.conf"
	if err := os.WriteFile(file, []byte("nameserver 127.0.0.1\nnameserver $(touch "+dir+"/pwned)\n# nameserver 127.0.0.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := (&localShell{}).Run(strings.ReplaceAll(liveDNSProbeCommand, k3sResolvConfPath, file))
	if err != nil {
		t.Fatalf("probe: %v: %s", err, out)
	}
	if !strings.HasPrefix(out, "127.0.0.1 ") || strings.Count(out, "\n") != 1 {
		t.Errorf("probe output %q, want one line for 127.0.0.1 only", out)
	}
	if _, err := os.Stat(dir + "/pwned"); err == nil {
		t.Error("a nameserver value was executed")
	}
}
