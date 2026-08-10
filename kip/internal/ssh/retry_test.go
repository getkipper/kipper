package ssh

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The whole safety of the retry is this distinction. A connection that never
// completed its handshake ran nothing, so running it again is the same as
// running it once. A connection lost midway may have run half of the command,
// and an installer that repeats those corrupts hosts.
func TestNeverReachedHost(t *testing.T) {
	// ssh returns 255 for its own failures; anything else is the remote
	// command's own status.
	transport := &exec.ExitError{ProcessState: exitStatus(255)}
	remote := &exec.ExitError{ProcessState: exitStatus(1)}

	cases := []struct {
		name   string
		err    error
		output string
		want   bool
	}{
		{
			// The failure that started this: sshd's MaxStartups randomly
			// dropping an unauthenticated connection during a brute-force flood.
			name: "the handshake was dropped",
			err:  transport,
			output: "kex_exchange_identification: read: Connection reset by peer" +
				"\nConnection reset by 203.0.113.10 port 22",
			want: true,
		},
		{
			name:   "nothing listening",
			err:    transport,
			output: "ssh: connect to host 203.0.113.10 port 22: Connection refused",
			want:   true,
		},
		{
			name:   "the host did not answer",
			err:    transport,
			output: "ssh: connect to host 203.0.113.10 port 22: Operation timed out",
			want:   true,
		},
		{
			name:   "dns is down",
			err:    transport,
			output: "ssh: Could not resolve hostname demo: Temporary failure in name resolution",
			want:   true,
		},

		{
			// The one that matters most. CombinedOutput merges ssh's stderr
			// with the remote command's, so a remote tool reporting a
			// connection failure of its own reads exactly like a connection
			// that never opened — and the command it followed has already run.
			// Matching ssh's message format rather than the bare phrase is
			// what separates them.
			name:   "a remote command reports its own connection failure",
			err:    remote,
			output: "curl: (7) Failed to connect to registry.example.com port 443: Connection refused",
			want:   false,
		},
		{
			// Same text, and the remote command happened to exit 255 too. The
			// format check is the only thing left standing between this and a
			// repeat.
			name:   "a remote command fails with ssh's own exit status",
			err:    transport,
			output: "curl: (28) Failed to connect: Connection timed out",
			want:   false,
		},
		{
			// Configuration, not weather. Retrying only delays a clear error.
			name:   "the key is wrong",
			err:    transport,
			output: "root@203.0.113.10: Permission denied (publickey).",
			want:   false,
		},
		{
			name:   "the host key changed",
			err:    transport,
			output: "@@@@ WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED! @@@@\nHost key verification failed.",
			want:   false,
		},
		{
			name:   "the remote command failed",
			err:    remote,
			output: "Error from server (NotFound): namespaces \"cert-manager\" not found",
			want:   false,
		},
		{
			// The connection died after the command started, so it may have
			// done half its work.
			name:   "the connection dropped mid-command",
			err:    transport,
			output: "Connection to 203.0.113.10 closed by remote host.",
			want:   false,
		},
		{name: "success", err: nil, output: "", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, neverReachedHost(tc.err, tc.output))
		})
	}
}

// exitStatus builds a ProcessState reporting the given code, so the classifier
// can be driven without spawning anything.
func exitStatus(code int) *os.ProcessState {
	//nolint:gosec // the code is a test constant, not input
	cmd := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code))
	_ = cmd.Run()
	return cmd.ProcessState
}

// A Unix socket path is limited to about 104 characters on macOS, and OpenSSH's
// own %C token spends 64 of them on a hash. A path that overruns produces a
// confusing failure at connect time rather than an obvious one here.
func TestControlSocket_IsShortEnoughForAUnixSocket(t *testing.T) {
	path := controlSocket(Config{Host: "a-rather-long-hostname.demo.getkipper.com", User: "root"})
	require.NotEmpty(t, path)
	assert.LessOrEqual(t, len(path), maxSocketPath, "a longer path cannot be bound as a Unix socket on macOS")

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".kip", "ssh"), filepath.Dir(path),
		"the socket belongs under the user's own directory, where no other local account can sit on the path")
}

// Two hosts must not share a socket, or a command meant for one would run on
// the other.
func TestControlSocket_DiffersPerConnection(t *testing.T) {
	a := controlSocket(Config{Host: "one.example.com", User: "root"})
	b := controlSocket(Config{Host: "two.example.com", User: "root"})
	c := controlSocket(Config{Host: "one.example.com", User: "deploy"})
	d := controlSocket(Config{Host: "one.example.com", User: "root", Port: "2222"})

	assert.NotEqual(t, a, b, "a different host is a different connection")
	assert.NotEqual(t, a, c, "so is a different user")
	assert.NotEqual(t, a, d, "so is a different port")

	assert.Equal(t, a, controlSocket(Config{Host: "one.example.com", User: "root", Port: "22"}),
		"the default port and an explicit 22 are the same connection")
}

// Two clients addressing the same address through different jump hosts reach
// different machines. Sharing a socket would let the second reuse the first's
// authenticated connection and run an installer against the wrong host, with
// Dial's own connectivity check succeeding against it first.
func TestControlSocket_SeparatesConnectionsThatDifferOnlyByRouting(t *testing.T) {
	base := Config{Host: "10.0.0.10", User: "root"}

	viaA := base
	viaA.Options = []string{"ProxyJump=bastion-a"}
	viaB := base
	viaB.Options = []string{"ProxyJump=bastion-b"}

	assert.NotEqual(t, controlSocket(viaA), controlSocket(viaB),
		"a different jump host is a different machine at the same address")

	withKey := base
	withKey.KeyPath = "/home/ops/.ssh/id_a"
	otherKey := base
	otherKey.KeyPath = "/home/ops/.ssh/id_b"

	assert.NotEqual(t, controlSocket(withKey), controlSocket(otherKey),
		"reusing a master authenticated by another key ignores this call's own identity")
	assert.NotEqual(t, controlSocket(base), controlSocket(withKey),
		"and so does reusing one established without a forced key")
}

// A collision means commands running on the wrong machine, so the digest keeps
// enough bytes that finding one is not worth attempting.
func TestControlSocket_KeepsEnoughDigest(t *testing.T) {
	path := controlSocket(Config{Host: "one.example.com", User: "root"})
	assert.GreaterOrEqual(t, len(filepath.Base(path)), 16,
		"four bytes collide by accident at tens of thousands of connections")
}

// A digest can only cover what Config holds; OpenSSH also consults the agent,
// ~/.ssh/config and the key file's own bytes. Two processes with identical
// Config can therefore be asking for different connections, and the loser of a
// ControlMaster race would inherit the winner's authenticated session. Scoping
// the socket to one process removes the question rather than narrowing it.
func TestControlSocket_IsNotSharedBetweenProcesses(t *testing.T) {
	path := controlSocket(Config{Host: "one.example.com", User: "root"})
	assert.Contains(t, filepath.Base(path), fmt.Sprintf("-%d", os.Getpid()),
		"a socket another process can find is a socket another process can be aliased onto")
}

// ssh treats anything after the destination as the remote command, so a control
// command placed there asks the remote shell to run "-O exit" instead of
// telling the master to close.
func TestSSHArgsWith_PlacesControlCommandsBeforeTheDestination(t *testing.T) {
	c := &Client{cfg: Config{Host: "demo.example.com"}, controlPath: "/tmp/kip-abcd"}
	args := c.sshArgsWith("-O", "exit")

	dest := -1
	control := -1
	for i, a := range args {
		if a == "root@demo.example.com" {
			dest = i
		}
		if a == "-O" {
			control = i
		}
	}
	require.NotEqual(t, -1, dest, "the destination must be present")
	require.NotEqual(t, -1, control, "the control flag must be present")
	assert.Less(t, control, dest, "after the destination it becomes a remote command")
	assert.Equal(t, len(args)-1, dest, "the destination is last")
}

// Every command has to carry the multiplexing options, or the ones that do not
// go back to a handshake apiece.
func TestSSHArgs_CarryTheSharedConnection(t *testing.T) {
	c := &Client{cfg: Config{Host: "demo.example.com"}, controlPath: "/tmp/kip-abcd1234"}
	args := strings.Join(c.sshArgs(), " ")

	assert.Contains(t, args, "ControlMaster=auto")
	assert.Contains(t, args, "ControlPath=/tmp/kip-abcd1234")
	assert.Contains(t, args, "ControlPersist="+controlPersist)
}

// Without a socket kip still works; it just makes a handshake per command.
func TestSSHArgs_OmitTheOptionsWhenThereIsNoSocket(t *testing.T) {
	c := &Client{cfg: Config{Host: "demo.example.com"}}
	args := strings.Join(c.sshArgs(), " ")

	assert.NotContains(t, args, "ControlMaster")
	assert.NotContains(t, args, "ControlPath")
}

// The seam the safety story rides on, driven through a stub ssh rather than a
// helper. Every earlier version of these tests pinned a classifier, which would
// have stayed green if the caller used it backwards.
func TestDial_RetriesTheProbeAndNeverTheCommands(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")

	// A stub ssh that fails the first two invocations the way a flooded sshd
	// does, then succeeds, recording every call.
	stub := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + calls + "\n" +
		"n=$(wc -l < " + calls + ")\n" +
		"if [ \"$n\" -le 2 ]; then\n" +
		"  echo 'kex_exchange_identification: read: Connection reset by peer' >&2\n" +
		"  exit 255\n" +
		"fi\n" +
		"exit 0\n"
	//nolint:gosec // a stub in the test's own temp dir has to be executable
	require.NoError(t, os.WriteFile(stub, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	c, err := Dial(Config{Host: "demo.example.com", User: "root"})
	require.NoError(t, err, "two dropped handshakes must not end the connection attempt")
	require.NotNil(t, c)

	recorded, err := os.ReadFile(calls)
	require.NoError(t, err)
	probes := strings.Count(string(recorded), " true")
	assert.GreaterOrEqual(t, probes, 3, "the probe is what gets retried")
}

// Nothing that changes the host is ever repeated, however the connection fails.
func TestRun_IsNeverRetried(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")

	stub := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + calls + "\n" +
		"case \"$*\" in\n" +
		"  *' true') exit 0 ;;\n" +
		"esac\n" +
		"echo 'kex_exchange_identification: read: Connection reset by peer' >&2\n" +
		"exit 255\n"
	//nolint:gosec // a stub in the test's own temp dir has to be executable
	require.NoError(t, os.WriteFile(stub, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	c, err := Dial(Config{Host: "demo.example.com", User: "root"})
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(calls, nil, 0o600))
	_, runErr := c.Run("rm -rf /var/lib/something && touch /var/lib/marker")
	require.Error(t, runErr, "the connection failed, so the command failed")

	recorded, err := os.ReadFile(calls)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(recorded), "/var/lib/marker"),
		"a command that changes the host runs once, whatever the connection did")
}

// OpenSSH does not bind the path it is given. muxserver_listen binds
// `<ControlPath>.<16 random characters>` and links that into place afterwards,
// so a path that fits by itself can still be seventeen bytes too long — and ssh
// treats a listener it cannot bind as fatal rather than falling back.
func TestSocketPathFits_LeavesRoomForTheListenerOpenSSHActuallyBinds(t *testing.T) {
	longest := strings.Repeat("a", maxSocketPath)
	assert.True(t, socketPathFits(longest))
	assert.LessOrEqual(t, len(longest)+muxListenerSuffix, unixSocketPathMax-1,
		"the name ssh binds first has to fit too, or it fails fatally on a path we accepted")

	assert.False(t, socketPathFits(strings.Repeat("a", maxSocketPath+1)))

	// The bound in the previous version of this guard.
	assert.False(t, socketPathFits(strings.Repeat("a", 99)),
		"99 bytes leaves no room for the suffix and was accepted before")
}
