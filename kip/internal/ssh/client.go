// Package ssh wraps OpenSSH, honoring SSH configuration, agents and options.
// Commands use ControlMaster multiplexing when available. Uploads send data
// through remote cat over the same transport.
package ssh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const defaultPort = "22"

const (
	// dialAttempts is how many times a command whose connection never
	// reached the remote side is tried. Enough to ride out a MaxStartups
	// drop, few enough that a genuinely unreachable host fails promptly.
	dialAttempts = 3
	// dialBackoff is the wait before the second attempt; the third waits
	// twice as long. A MaxStartups drop clears in well under this.
	dialBackoff = time.Second
	// sshTransportExit is the status ssh returns when it fails on its own
	// rather than relaying a remote command's. A remote command can also
	// return it, which is why it is necessary and not sufficient.
	sshTransportExit = 255
	// unixSocketPathMax is the smaller of the two platform limits on a Unix
	// socket path, including its terminating NUL: macOS allows 104 bytes,
	// Linux 108.
	unixSocketPathMax = 104
	// muxListenerSuffix is what OpenSSH appends before it has a socket at all.
	// muxserver_listen binds `<ControlPath>.<16 random characters>` and links
	// that into place afterwards, so the path that has to fit is 17 bytes
	// longer than the one we choose. Checking only the final path lets ssh
	// fail fatally on a path we accepted.
	muxListenerSuffix = 17
	// maxSocketPath is what is left for the path itself.
	maxSocketPath = unixSocketPathMax - 1 - muxListenerSuffix
	// controlPersist is how long the master outlives the command that opened
	// it. Nothing reuses it: the socket is scoped to one process and Close
	// shuts it down. It exists so an unclean exit does not strand a connection
	// on the host for long.
	controlPersist = "30s"
)

// Config holds the parameters needed to invoke ssh.
type Config struct {
	Host            string
	Port            string
	User            string
	KeyPath         string
	FallbackKeyPath string
	Options         []string
	CommandTimeout  time.Duration
}

// Client runs commands as root on a remote host using a fresh SSH process per
// invocation. Non-root logins use passwordless sudo with root's home directory.
// Captured output combines command stdout and stderr, with sudo diagnostics
// included only on failure. RunStreamTo keeps stdout and stderr separate.
type Client struct {
	cfg Config
	// controlPath is the multiplexing socket, or "" when one could not be
	// set up. An install still works without it; it just makes a
	// handshake per command again.
	controlPath string
}

// Dial verifies SSH access and, for non-root users, passwordless sudo access
// to a root Bash shell. An empty User defaults to root. Connection probes retry
// transient transport failures. Authentication and sudo failures return an error.
func Dial(cfg Config) (*Client, error) {
	c := &Client{cfg: cfg, controlPath: controlSocket(cfg)}

	// Retry only the harmless connection probe. A failed SSH command may
	// already have changed the host, so subsequent commands run once.
	probe := "true"
	if c.viaSudo() {
		probe = sudoProbe
	}
	var err error
	var out string
	for attempt := 1; ; attempt++ {
		out, err = c.attemptRaw(probe, nil, nil)
		if err == nil || attempt == dialAttempts || !neverReachedHost(err, out) {
			break
		}
		time.Sleep(time.Duration(attempt) * dialBackoff)
	}
	if err != nil {
		if c.viaSudo() && strings.Contains(out, "sudo") {
			return nil, fmt.Errorf("connecting to %s: %s needs passwordless sudo, because kip runs its commands as root: %s", cfg.Host, cfg.User, strings.TrimSpace(out))
		}
		if user, ok := CloudImageUser(out); ok && !c.viaSudo() {
			return nil, fmt.Errorf("connecting to %s: %s", cfg.Host, rootRefusedFor(user))
		}
		if trimmed := strings.TrimSpace(out); trimmed != "" {
			return nil, fmt.Errorf("connecting to %s: %w: %s", cfg.Host, err, trimmed)
		}
		return nil, fmt.Errorf("connecting to %s: %w", cfg.Host, err)
	}

	return c, nil
}

// sudoPath includes /usr/local/bin so sudo commands can find k3s and kubectl
// even when sudo's secure_path omits that directory.
const sudoPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func (c *Client) viaSudo() bool {
	return c.cfg.User != "" && c.cfg.User != "root"
}

// cloudImageUserPattern matches the message cloud images print to a root
// login they refuse, which names the image's own login user.
var cloudImageUserPattern = regexp.MustCompile(`Please login as the user "([a-z_][a-z0-9_-]*)" rather than the user "root"`)

// CloudImageUser extracts the suggested user from a cloud image's root-login
// refusal message. It returns an empty string and false when no message matches.
func CloudImageUser(output string) (string, bool) {
	m := cloudImageUserPattern.FindStringSubmatch(output)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// RootRefused returns an error explaining a cloud image's root-login refusal
// and instructing the caller to retry with --ssh-user set to user.
func RootRefused(user string) error {
	return errors.New(rootRefusedFor(user))
}

func rootRefusedFor(user string) string {
	return fmt.Sprintf("the server refuses root logins and specifies %s as its login user. Run the command again with --ssh-user %s", user, user)
}

// sudoProbe checks passwordless access to the root shell used for commands.
// Running it directly preserves sudo diagnostics for Dial.
const sudoProbe = "LC_ALL=C sudo -n -H -- /bin/bash -c true"

// remoteCommand prepares command to run as root. Non-root logins use
// non-interactive sudo with root's home directory and combined command output.
// A temporary file captures sudo diagnostics and is removed on shell exit.
// Diagnostics are printed only on failure, preserving refusal details while
// keeping warnings such as "unable to resolve host" out of successful output
// that kip parses.
func (c *Client) remoteCommand(command string) string {
	if !c.viaSudo() {
		return command
	}
	inner := "sudo -n -H -- /bin/bash -c " + Quote("exec 2>&1; PATH="+sudoPath+"; export PATH\n"+command)
	return `e=$(mktemp) || exit 1; trap 'rm -f "$e"' EXIT; trap 'exit 129' HUP; trap 'exit 130' INT; trap 'exit 143' TERM; ` +
		inner + ` 2>"$e"; rc=$?; [ "$rc" -eq 0 ] || cat "$e" >&2; exit "$rc"`
}

// remoteStreamCommand is remoteCommand for RunStreamTo, whose callers keep
// stdout and stderr apart. sudo's messages stay on stderr there.
func (c *Client) remoteStreamCommand(command string) string {
	if !c.viaSudo() {
		return command
	}
	return "sudo -n -H -- /bin/bash -c " + Quote("PATH="+sudoPath+"; export PATH\n"+command)
}

// masterIsServing asks OpenSSH whether a master is actually accepting commands
// on the socket.
//
// The socket path having been prepared says nothing. A file left by an unclean
// exit — and the process id in the name recycles — makes ssh quietly fall back
// to a connection per command, and a firewall rule sized for one connection
// would then lock the installer out of a host it had half built. This is the
// difference between believing the connection is shared and knowing it.
func (c *Client) masterIsServing() bool {
	if c.controlPath == "" {
		return false
	}
	//nolint:gosec // the socket path is derived from the operator's own connection parameters
	return withoutLocale(exec.Command("ssh", c.sshArgsWith("-O", "check")...)).Run() == nil
}

// controlSocket hashes connection options and key selection as well as the
// destination, preventing reuse across distinct routes or identities. It uses
// the private kip directory and returns empty when multiplexing is unsupported,
// the directory is unavailable, or the socket path would exceed the limit.
func controlSocket(cfg Config) string {
	// Windows OpenSSH has no connection multiplexing, and asking for it there
	// is worse than going without: ssh treats a ControlPath it cannot bind as
	// fatal, so every command fails rather than falling back. An empty path is
	// the documented "no master" answer this file already handles, which costs
	// a handshake per command and works.
	if runtime.GOOS == "windows" {
		return ""
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".kip", "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}

	user := cfg.User
	if user == "" {
		user = "root"
	}
	port := cfg.Port
	if port == "" {
		port = defaultPort
	}

	// Length-prefixed, so no pair of configurations can serialise to the same
	// bytes by shifting a boundary — the same reason the environment digest
	// does it.
	h := sha256.New()
	for _, part := range append([]string{user, cfg.Host, port, cfg.KeyPath, cfg.FallbackKeyPath}, cfg.Options...) {
		// A hash.Hash never reports a write error, per its own contract.
		_, _ = fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	// Scope sockets to this process: SSH configuration, agent state and key
	// contents can differ between invocations with identical Config values.
	path := filepath.Join(dir, fmt.Sprintf("%s-%d", hex.EncodeToString(h.Sum(nil))[:16], os.Getpid()))
	// A deep home directory is unusual, but it has to degrade to a connection
	// per command rather than to no connection: ssh treats a path it cannot
	// bind as fatal rather than falling back.
	if !socketPathFits(path) {
		return ""
	}
	return path
}

// Run executes a command on the remote host and returns its combined
// stdout+stderr output.
func (c *Client) Run(command string) (string, error) {
	out, err := c.attempt(command, nil, nil)
	if err != nil {
		trimmed := strings.TrimSpace(out)
		if trimmed == "" {
			return out, fmt.Errorf("running %q on %s: %w", command, c.cfg.Host, err)
		}
		return out, fmt.Errorf("running %q on %s: %w: %s", command, c.cfg.Host, err, trimmed)
	}
	return out, nil
}

// attempt runs ssh exactly once. stdin, when non-nil, is fed to the remote
// command; stream, when non-nil, receives stdout and stderr as they arrive
// instead of them being captured.
func (c *Client) attempt(command string, stdin []byte, stream io.Writer) (string, error) {
	return c.attemptRaw(c.remoteCommand(command), stdin, stream)
}

// maxInlineCommand is the byte threshold for staging commands with nil stdin.
// OpenSSH sends shared-connection commands through a local socket. Staying below
// macOS's default 8 KiB socket buffer size leaves room for protocol overhead to
// avoid "mm_send_fd: sendmsg(1): Message too long" failures.
const maxInlineCommand = 4096

// stagedCommand evaluates a command from stdin in the remote user's shell,
// keeping it in memory. Redirecting stdin from /dev/null preserves the empty
// input used for inline commands with nil stdin. The x sentinel preserves
// trailing newlines that command substitution would otherwise strip.
const stagedCommand = `c=$(cat && printf x) || exit 1; c=${c%x}; eval "$c" </dev/null`

// stage moves commands exceeding maxInlineCommand bytes onto stdin when stdin
// is nil. A non-nil stdin, even if empty, preserves the caller's input and keeps
// the command inline. Commands at or below the threshold also stay inline.
func stage(command string, stdin []byte) (string, []byte) {
	if stdin != nil || len(command) <= maxInlineCommand {
		return command, stdin
	}
	return stagedCommand, []byte(command)
}

// attemptRaw runs command without the sudo wrapper so connection probes retain
// their diagnostics. Long commands with nil stdin are staged through stdin.
func (c *Client) attemptRaw(command string, stdin []byte, stream io.Writer) (string, error) {
	command, stdin = stage(command, stdin)
	cmd, ctx, cancel := c.command(append(c.sshArgs(), command)...)
	defer cancel()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out []byte
	var err error
	if stream != nil {
		cmd.Stdout = stream
		cmd.Stderr = stream
		err = cmd.Run()
	} else {
		out, err = cmd.CombinedOutput()
	}
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("ssh command did not finish within %s: %w", c.cfg.CommandTimeout, err)
	}
	return string(out), err
}

// neverReachedHost recognizes retryable Dial-probe failures by SSH's exit
// status and diagnostic format. Authentication and host-key failures return
// immediately. Only the harmless connection probe uses this retry decision.
func neverReachedHost(err error, output string) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != sshTransportExit {
		return false
	}
	for _, diagnostic := range []string{
		// ssh: connect to host X port 22: Connection refused / timed out /
		// No route to host / Network is unreachable
		"ssh: connect to host ",
		"ssh: Could not resolve hostname ",
		// The handshake itself, which is what a MaxStartups drop produces.
		"kex_exchange_identification:",
		"ssh_exchange_identification:",
	} {
		if strings.Contains(output, diagnostic) {
			return true
		}
	}
	return false
}

// RunStdin executes a command on the remote host, feeding the remote
// command's stdin from the given reader, and returns its combined
// stdout+stderr output. Because the payload travels over the ssh
// process's stdin rather than the command string, secret material never
// appears in the process table or in the error message (which quotes
// only the command).
func (c *Client) RunStdin(command string, stdin io.Reader) (string, error) {
	payload, readErr := io.ReadAll(stdin)
	if readErr != nil {
		return "", fmt.Errorf("reading stdin for %q: %w", command, readErr)
	}

	out, err := c.attempt(command, payload, nil)
	if err != nil {
		trimmed := strings.TrimSpace(out)
		if trimmed == "" {
			return out, fmt.Errorf("running %q on %s: %w", command, c.cfg.Host, err)
		}
		return out, fmt.Errorf("running %q on %s: %w: %s", command, c.cfg.Host, err, trimmed)
	}
	return out, nil
}

// Held provides input, output and completion for an SSH command. Closing
// Stdin sends EOF to the command. Drain Stdout before calling Wait, which
// waits for the SSH process to exit and returns its exit error, if any.
type Held struct {
	Stdin  io.WriteCloser
	Stdout io.Reader
	Wait   func() error
}

// Start starts the local SSH process without a command time limit and
// discards stderr. Like Run, it runs the command as root, through sudo for a
// non-root login. A successful return means SSH started, so connection or
// remote command failures can still be reported by Held.Wait. The caller
// must close Stdin, drain Stdout and call Wait. Closing Stdin sends EOF,
// which ends commands that exit on EOF. SSH shares kip's process group and
// receives interrupts sent to that group.
func (c *Client) Start(command string) (*Held, error) {
	//nolint:gosec // ssh client is the operator's chosen execution surface; arguments are built by kip internals against the host the operator selected
	cmd := withoutLocale(exec.Command("ssh", append(c.sshArgs(), c.remoteStreamCommand(command))...))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %q on %s: %w", command, c.cfg.Host, err)
	}
	return &Held{Stdin: stdin, Stdout: stdout, Wait: cmd.Wait}, nil
}

// RunStream executes a command and streams stdout and stderr to the
// given writer as the remote command produces them.
// It is not retried. Whatever the remote side produced before the connection
// dropped has already been written to stdout, and running the command again
// would append a second copy to the first.
func (c *Client) RunStream(command string, stdout io.Writer) error {
	if _, err := c.attempt(command, nil, stdout); err != nil {
		return fmt.Errorf("running %q on %s: %w", command, c.cfg.Host, err)
	}
	return nil
}

// RunStreamTo separates remote stdout from stderr and SSH diagnostics so callers
// can track log offsets. It leaves retries to the caller to avoid duplicate output.
func (c *Client) RunStreamTo(command string, stdout, stderr io.Writer) error {
	remote, stdin := stage(c.remoteStreamCommand(command), nil)
	cmd, ctx, cancel := c.command(append(c.sshArgs(), remote)...)
	defer cancel()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("ssh command did not finish within %s: %w", c.cfg.CommandTimeout, err)
	}
	if err != nil {
		return fmt.Errorf("running %q on %s: %w", command, c.cfg.Host, err)
	}
	return nil
}

// Upload writes the contents of a reader to a remote file path. We use
// `cat > path` over an ssh session rather than scp so the only required
// remote-side tooling is a POSIX shell.
func (c *Client) Upload(content io.Reader, remotePath string, mode os.FileMode) error {
	remoteCmd := UploadCommand(remotePath, mode)
	payload, readErr := io.ReadAll(content)
	if readErr != nil {
		return fmt.Errorf("reading content for %s: %w", remotePath, readErr)
	}

	out, err := c.attempt(remoteCmd, payload, nil)
	if err != nil {
		trimmed := strings.TrimSpace(out)
		if trimmed == "" {
			return fmt.Errorf("uploading to %s on %s: %w", remotePath, c.cfg.Host, err)
		}
		return fmt.Errorf("uploading to %s on %s: %w: %s", remotePath, c.cfg.Host, err, trimmed)
	}
	return nil
}

// UploadCommand is the shell command that writes stdin to path with mode. A
// restrictive umask keeps the new file private until chmod applies mode.
// Removing the existing path avoids following a pre-existing symlink; callers
// must use a directory protected from concurrent untrusted writes.
func UploadCommand(path string, mode os.FileMode) string {
	q := Quote(path)
	return fmt.Sprintf("umask 077 && rm -f %s && cat > %s && chmod %o %s", q, q, mode, q)
}

// Quote makes s a single shell word.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// socketPathFits reports whether OpenSSH can bind a multiplexing listener at
// this path, allowing for the temporary name it binds first.
func socketPathFits(path string) bool {
	return len(path) <= maxSocketPath
}

// SurvivesSSHRateLimit checks that the shared SSH connection is still serving.
// Rechecking before the firewall step prevents a stale socket from enabling a
// rate limit that would block an installer opening one connection per command.
func (c *Client) SurvivesSSHRateLimit() bool {
	return c.masterIsServing()
}

// ConfirmFreshConnection runs true over a new SSH connection to verify that
// the host accepts new sessions independently of the shared connection.
// Retries tolerate transient handshake failures after a firewall reload.
func (c *Client) ConfirmFreshConnection() error {
	var err error
	for attempt := 1; attempt <= freshAttempts; attempt++ {
		if _, err = c.RunFresh("true"); err == nil {
			return nil
		}
		if attempt < freshAttempts {
			time.Sleep(freshRetryDelay)
		}
	}
	return err
}

// ConfirmFreshKeyOnlyConnection runs true as root over a new connection using
// only public-key authentication. Non-root users need passwordless sudo. It
// makes up to three attempts, waiting two seconds between failures, and returns
// the last error if all fail. A positive CommandTimeout bounds each attempt.
func (c *Client) ConfirmFreshKeyOnlyConnection() error {
	keyOnly := []string{
		"-o", "PreferredAuthentications=publickey",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-o", "BatchMode=yes",
	}
	var err error
	for attempt := 1; attempt <= freshAttempts; attempt++ {
		// OpenSSH keeps the first value of each option, so these go ahead of
		// the operator's own options.
		args := append(append(keyOnly[:len(keyOnly):len(keyOnly)], c.connectionArgs(false, true)...), c.remoteCommand("true"))
		cmd, ctx, cancel := c.command(args...)
		out, runErr := cmd.CombinedOutput()
		cancel()
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			err = fmt.Errorf("a new key-only SSH connection to %s did not finish within %s", c.cfg.Host, c.cfg.CommandTimeout)
		case runErr != nil:
			err = fmt.Errorf("opening a new key-only SSH connection to %s: %w: %s", c.cfg.Host, runErr, strings.TrimSpace(string(out)))
		default:
			return nil
		}
		if attempt < freshAttempts {
			time.Sleep(freshRetryDelay)
		}
	}
	return err
}

// ConfirmPasswordRefused makes one fresh connection attempt for the configured
// user with public keys disabled and no interactive prompts. It returns nil
// only when OpenSSH reports a refusal offering neither password nor
// keyboard-interactive authentication. An accepted login, an inconclusive
// response or timeout returns an error. A positive CommandTimeout bounds the
// attempt.
func (c *Client) ConfirmPasswordRefused() error {
	noKeys := []string{
		"-o", "PubkeyAuthentication=no",
		"-o", "PreferredAuthentications=password,keyboard-interactive",
		"-o", "BatchMode=yes",
	}
	args := append(append(noKeys[:len(noKeys):len(noKeys)], c.connectionArgs(false, true)...), c.remoteCommand("true"))
	cmd, ctx, cancel := c.command(args...)
	defer cancel()
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("a new SSH connection to %s did not finish within %s", c.cfg.Host, c.cfg.CommandTimeout)
	}
	if err == nil {
		return fmt.Errorf("%s accepted a login without a key", c.cfg.Host)
	}
	return passwordRefused(string(out))
}

// passwordRefused reads the methods sshd still offers from OpenSSH's
// "Permission denied (...)" message.
func passwordRefused(out string) error {
	i := strings.LastIndex(out, "Permission denied (")
	if i < 0 {
		return fmt.Errorf("could not tell which login methods the server offers: %s", strings.TrimSpace(out))
	}
	methods, _, ok := strings.Cut(out[i+len("Permission denied ("):], ")")
	if !ok {
		return fmt.Errorf("could not tell which login methods the server offers: %s", strings.TrimSpace(out))
	}
	for _, m := range strings.Split(methods, ",") {
		if m == "password" || m == "keyboard-interactive" {
			return fmt.Errorf("the server still offers %s login (%s)", m, methods)
		}
	}
	return nil
}

const freshAttempts = 3

var freshRetryDelay = 2 * time.Second

// RunFresh runs command over a new SSH connection and returns combined stdout
// and stderr. Bypassing multiplexing verifies that new connections are accepted.
func (c *Client) RunFresh(command string) (string, error) {
	cmd, ctx, cancel := c.command(c.freshConnectionArgs(command)...)
	defer cancel()
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("a new SSH connection to %s did not finish within %s", c.cfg.Host, c.cfg.CommandTimeout)
	}
	if err != nil {
		return string(out), fmt.Errorf("opening a new SSH connection to %s: %w: %s", c.cfg.Host, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// freshConnectionArgs disables multiplexing for command.
func (c *Client) freshConnectionArgs(command string) []string {
	return append(c.connectionArgs(false, true), c.remoteCommand(command))
}

// Close tears down the shared connection, so kip does not leave one open behind
// it. A failure here is not worth reporting: the master exits on its own when
// ControlPersist elapses.
func (c *Client) Close() error {
	if c.controlPath == "" {
		return nil
	}
	cmd, _, cancel := c.command(c.sshArgsWith("-O", "exit")...)
	defer cancel()
	_ = cmd.Run()
	return nil
}

// command bounds execution and output-pipe waits when CommandTimeout is positive.
// On Unix, cancellation kills the process group. Normal SSH exit preserves
// helpers that may serve the shared connection. Untimed commands retain
// terminal access for interactive SSH prompts.
func (c *Client) command(args ...string) (*exec.Cmd, context.Context, context.CancelFunc) {
	if c.cfg.CommandTimeout <= 0 {
		//nolint:gosec // ssh client is the operator's chosen execution surface; arguments are built by kip internals against the host the operator selected
		return withoutLocale(exec.Command("ssh", args...)), context.Background(), func() {}
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.CommandTimeout)
	//nolint:gosec // ssh client is the operator's chosen execution surface; arguments are built by kip internals against the host the operator selected
	cmd := withoutLocale(exec.CommandContext(ctx, "ssh", args...))
	ownProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	return cmd, ctx, cancel
}

// withoutLocale sets cmd's environment to EnvironWithoutLocale and returns cmd.
// SSH configurations can forward locale variables through SendEnv, as macOS does
// with "SendEnv LANG LC_*". An unavailable locale, for example on a Debian cloud
// image with only C.UTF-8, can cause shell warnings in output kip parses.
// Removing these variables lets the host choose its locale unless SSH
// configuration supplies one explicitly.
func withoutLocale(cmd *exec.Cmd) *exec.Cmd {
	cmd.Env = EnvironWithoutLocale()
	return cmd
}

// EnvironWithoutLocale returns a new environment slice for SSH subprocesses,
// excluding LANG, LANGUAGE and names beginning with LC_, matched case-insensitively.
// It preserves the order and contents of other entries and leaves kip's environment
// unchanged.
func EnvironWithoutLocale() []string {
	env := []string{}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		// Windows matches variable names case-insensitively.
		name = strings.ToUpper(name)
		if name == "LANG" || name == "LANGUAGE" || strings.HasPrefix(name, "LC_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// fileExists returns true when the path exists and is not a directory.
// Used by sshArgs to decide whether to pass `-i` for a fallback key.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// sshArgs builds the connection arguments, including multiplexing when available.
func (c *Client) sshArgs() []string {
	return c.sshArgsWith()
}

// sshArgsWith places extra flags before the destination so OpenSSH interprets
// control options such as -O exit locally instead of as a remote command.
func (c *Client) sshArgsWith(extra ...string) []string {
	return c.connectionArgs(c.controlPath != "", false, extra...)
}

// connectionArgs puts fresh-connection overrides before Config.Options because
// OpenSSH uses the first value supplied for each option.
func (c *Client) connectionArgs(multiplex, fresh bool, extra ...string) []string {
	args := []string{
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
		// Detect stalled connections during long commands and log streams.
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
	}
	if fresh {
		args = append(args, "-o", "ControlMaster=no", "-o", "ControlPath=none")
	}
	if multiplex {
		// One connection carries every command. Without this an install
		// makes a handshake per command, which is what a flooded sshd
		// randomly drops.
		args = append(args,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+c.controlPath,
			"-o", "ControlPersist="+controlPersist,
		)
	}
	if c.cfg.Port != "" && c.cfg.Port != defaultPort {
		args = append(args, "-p", c.cfg.Port)
	}
	if c.cfg.KeyPath != "" {
		// Explicit: force this and only this key.
		args = append(args, "-i", c.cfg.KeyPath, "-o", "IdentitiesOnly=yes")
	} else if c.cfg.FallbackKeyPath != "" && fileExists(c.cfg.FallbackKeyPath) {
		// Fallback: hint at the key but do not block ssh-agent or
		// ~/.ssh/config. No IdentitiesOnly.
		args = append(args, "-i", c.cfg.FallbackKeyPath)
	}
	for _, opt := range c.cfg.Options {
		args = append(args, "-o", opt)
	}
	user := c.cfg.User
	if user == "" {
		user = "root"
	}
	args = append(args, extra...)
	args = append(args, fmt.Sprintf("%s@%s", user, c.cfg.Host))
	return args
}
