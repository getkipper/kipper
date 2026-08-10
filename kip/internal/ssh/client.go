// Package ssh wraps OpenSSH (`ssh` on PATH) so kip behaves identically to the
// ssh command admins already configure for their workflow: ~/.ssh/config is
// honoured, agents work, ProxyJump works, passphrase-protected keys prompt or
// pull from the agent the way users expect, and any -o option that ssh accepts
// can be passed through.
//
// `ssh` is the only executable required. Upload streams into `cat > path` over
// the same session rather than calling scp, so the remote side needs no more
// than a POSIX shell and the local side needs no second binary.
//
// All of kip's installer code targets a small surface — open a
// connection, run a command, stream output, upload a file — so we
// shell out per command rather than holding a Go-side SSH connection.
//
// Those calls share one connection. OpenSSH's ControlMaster carries every
// command after the first over a socket, which turns an install's few hundred
// handshakes into one. That is not an optimisation: a `kip install` against a
// host being brute-forced on port 22 failed at cert-manager with
// `kex_exchange_identification: read: Connection reset by peer`, because sshd's
// MaxStartups randomly drops unauthenticated connections once ten are in flight
// and the installer was making hundreds of them into a flood.
package ssh

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	Host string
	Port string
	User string
	// KeyPath is the SSH private key the caller explicitly chose. When
	// non-empty, ssh is invoked with `-i <path> -o IdentitiesOnly=yes`,
	// forcing OpenSSH to use ONLY this key (the agent and other default
	// identities are ignored). Use this for "force key X" semantics.
	KeyPath string
	// FallbackKeyPath is a hint at a key file the caller would prefer
	// when KeyPath is empty. Unlike KeyPath this never forces
	// IdentitiesOnly: if the file exists ssh tries it; if not, ssh
	// falls through to its normal lookup (ssh-agent, ~/.ssh/config,
	// default identity files). Use this for the "default if I haven't
	// said otherwise" path so users with ssh-agent are not locked out
	// when the default file is absent.
	FallbackKeyPath string
	// Options is a list of additional `-o NAME=VALUE` strings forwarded
	// to ssh verbatim, for cases where the caller needs to override a
	// default (e.g. StrictHostKeyChecking, ProxyJump). Most users will
	// configure these in ~/.ssh/config instead.
	Options []string
}

// Client wraps an SSH-reachable remote host. Each method invocation spawns a
// fresh ssh process, but they share one connection through the control master
// established at Dial.
type Client struct {
	cfg Config
	// controlPath is the multiplexing socket, or "" when one could not be
	// set up. An install still works without it; it just makes a
	// handshake per command again.
	controlPath string
}

// Dial verifies that ssh can reach the host with the given config.
// Returns a Client on success.
func Dial(cfg Config) (*Client, error) {
	c := &Client{cfg: cfg, controlPath: controlSocket(cfg)}

	// The handshake is the only thing retried, and `true` is why that is safe.
	//
	// Every earlier attempt at this tried to work out, after the fact, whether
	// a command that failed had already run — from the exit status, from the
	// shape of the diagnostic, from a token the remote shell wrote. None of it
	// holds, because ssh carries stdout and stderr as separate streams and a
	// connection lost after the remote side wrote can deliver one without the
	// other. There is no way to read provenance out of a merged stream.
	//
	// So nothing that changes the host is ever repeated. What is repeated is a
	// command that does nothing, whose only job is to get the shared connection
	// up — and running `true` twice is the same as running it once whatever the
	// network did. Every command afterwards rides that connection and makes no
	// handshake, which is what the flood was dropping in the first place.
	var err error
	for attempt := 1; ; attempt++ {
		var out string
		out, err = c.attempt("true", nil, nil)
		if err == nil || attempt == dialAttempts || !neverReachedHost(err, out) {
			break
		}
		time.Sleep(time.Duration(attempt) * dialBackoff)
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", cfg.Host, err)
	}

	return c, nil
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
	return exec.Command("ssh", c.sshArgsWith("-O", "check")...).Run() == nil
}

// controlSocket returns the multiplexing socket path for this connection, or ""
// when the directory cannot be prepared.
//
// The name hashes everything that decides where the connection lands and who it
// authenticates as, not merely the address. Two clients addressing
// root@10.0.0.10 through different ProxyJump hosts reach different machines, and
// a socket keyed on the address alone would let the second reuse the first's
// authenticated connection and run an installer against the wrong host — with
// Dial's own `true` succeeding against it first. Options and key selection are
// in the digest for the same reason.
//
// It is a digest rather than OpenSSH's %C token because a Unix socket path is
// limited to about 104 characters on macOS and %C alone spends 64 of them; and
// it lives under the user's own kip directory rather than a shared temp
// directory, so no other local account can sit on the path.
func controlSocket(cfg Config) string {
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
	// The process id is part of the name, and it is what makes aliasing
	// impossible rather than merely unlikely.
	//
	// A digest can only cover what Config holds. OpenSSH also consults the
	// agent, ~/.ssh/config and the bytes of the key file itself, none of which
	// are visible here — so two processes with identical Config can still be
	// asking for different connections, and the loser of a ControlMaster race
	// would inherit the winner's authenticated session. Scoping the socket to
	// one process removes the question.
	//
	// It costs nothing: Close tears the master down when the command ends, so
	// nothing was being shared between invocations anyway. What multiplexing is
	// for is the hundreds of commands inside one install, and those are all in
	// this process.
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
	//nolint:gosec // ssh client is the operator's chosen execution surface; command is built by kip internals against the host the operator selected
	cmd := exec.Command("ssh", append(c.sshArgs(), command)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if stream != nil {
		cmd.Stdout = stream
		cmd.Stderr = stream
		return "", cmd.Run()
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// neverReachedHost reports whether a failure is the kind another attempt could
// clear.
//
// It guards only the `true` probe in Dial, so nothing here decides whether a
// command that changes the host gets repeated — that question no longer exists.
// What it decides is how quickly a hopeless connection gives up: a wrong key or
// a changed host key fails once rather than three times.
//
// Two things must hold, and neither is sufficient alone.
//
// The exit status must be 255, which is what ssh returns for its own failures;
// anything else is the remote command's own status and means it ran.
//
// And the output must carry a diagnostic in ssh's own message format. Bare
// substrings are not enough, because CombinedOutput merges ssh's stderr with
// the remote command's: a remote `curl` reporting "Connection refused" against
// some other service would otherwise look exactly like a connection that never
// opened, and the command it followed would be run again. Matching
// `ssh: connect to host ` rather than `Connection refused` is what separates
// them.
//
// Authentication and host-key failures are deliberately absent: they are
// configuration rather than weather, and retrying them only delays a clear
// error.
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

// Upload writes the contents of a reader to a remote file path. We use
// `cat > path` over an ssh session rather than scp so the only required
// remote-side tooling is a POSIX shell.
func (c *Client) Upload(content io.Reader, remotePath string, mode os.FileMode) error {
	// Set a restrictive umask and remove any pre-existing file/symlink before
	// writing, so the file is created 0600 from its first byte (never briefly
	// world-readable under the default umask) and a planted symlink at the
	// fixed path cannot redirect the write. chmod then applies the exact mode.
	remoteCmd := fmt.Sprintf("umask 077 && rm -f %s && cat > %s && chmod %o %s", remotePath, remotePath, mode, remotePath)
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

// socketPathFits reports whether OpenSSH can bind a multiplexing listener at
// this path, allowing for the temporary name it binds first.
func socketPathFits(path string) bool {
	return len(path) <= maxSocketPath
}

// Multiplexed reports whether commands are sharing one connection right now.
//
// It asks rather than remembers. The firewall step runs long after Dial — after
// hardening, after k3s — and a master killed in between leaves its socket file
// behind, at which point ssh quietly falls back to a connection per command. A
// remembered answer would then authorise a rule sized for one connection while
// the installer was making hundreds through it, and lock itself out of a
// half-built host. That is the failure this gate exists to prevent, so the gate
// cannot be reading a cached value.
func (c *Client) Multiplexed() bool {
	return c.masterIsServing()
}

// Close tears down the shared connection, so kip does not leave one open behind
// it. A failure here is not worth reporting: the master exits on its own when
// ControlPersist elapses.
func (c *Client) Close() error {
	if c.controlPath == "" {
		return nil
	}
	//nolint:gosec // the socket path is derived from the operator's own connection parameters
	_ = exec.Command("ssh", c.sshArgsWith("-O", "exit")...).Run()
	return nil
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

// sshArgs builds the OpenSSH argument list (everything before the
// command itself). Options precedence follows ssh's usual rules:
// -F / ~/.ssh/config first, then per-call flags. We add reasonable
// defaults (accept-new host keys, 10s connect timeout) that the user
// can override via cfg.Options.
func (c *Client) sshArgs() []string {
	return c.sshArgsWith()
}

// sshArgsWith builds the argument list with extra flags placed before the
// destination.
//
// Order is load-bearing for control commands: ssh treats anything after the
// destination as the remote command, so `ssh <opts> user@host -O exit` asks the
// remote shell to run "-O exit" instead of telling the master to close.
func (c *Client) sshArgsWith(extra ...string) []string {
	args := []string{
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
	}
	if c.controlPath != "" {
		// One connection carries every command. Without this an install
		// makes a handshake per command, which is what a flooded sshd
		// randomly drops.
		args = append(args,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+c.controlPath,
			"-o", "ControlPersist="+controlPersist,
			// A master whose TCP has died silently — a VPN reconnect, a NAT
			// table flush — would otherwise hang the install with no timeout
			// at all, where a connection per command failed in ten seconds.
			"-o", "ServerAliveInterval=15",
			"-o", "ServerAliveCountMax=4",
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
