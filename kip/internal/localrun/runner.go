// Package localrun runs installer commands on the machine kip itself runs on,
// with the same contract the SSH client gives them on a remote server.
package localrun

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// RootPath includes the sbin directories needed by installer commands.
const RootPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Runner runs installer commands through bash with a fixed environment and
// working directory, independent of how kip was launched.
type Runner struct {
	Home string
	Dir  string
	Path string
}

// New returns a Runner with root's environment.
func New() *Runner {
	return &Runner{Home: "/root", Dir: "/root", Path: RootPath}
}

// Run returns combined stdout and stderr, with stdin at EOF. Command failures
// wrap *exec.ExitError so callers can inspect the exit code.
func (r *Runner) Run(command string) (string, error) {
	return r.run(command, nil)
}

// RunStdin runs command with stdin as its only input and returns its combined
// output. Input is passed through stdin; commands may echo it into the output.
func (r *Runner) RunStdin(command string, stdin io.Reader) (string, error) {
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("reading stdin for %q: %w", command, err)
	}
	return r.run(command, payload)
}

// Upload writes content to path with mode, using the same command and defences
// as the SSH client.
func (r *Runner) Upload(content io.Reader, path string, mode os.FileMode) error {
	payload, err := io.ReadAll(content)
	if err != nil {
		return fmt.Errorf("reading content for %s: %w", path, err)
	}
	if _, err := r.run(ssh.UploadCommand(path, mode), payload); err != nil {
		return fmt.Errorf("uploading to %s: %w", path, err)
	}
	return nil
}

// bash finds bash in the runner's PATH, so a parent started with a minimal
// PATH can still run commands.
func (r *Runner) bash() (string, error) {
	for _, dir := range filepath.SplitList(r.Path) {
		candidate := filepath.Join(dir, "bash")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("bash is not installed in %s; the installer's commands need it", r.Path)
}

// ErrNoConnection indicates that commands run locally, without an SSH connection.
var ErrNoConnection = errors.New("commands run on this machine, so there is no connection to confirm")

// ConfirmFreshConnection returns ErrNoConnection.
func (r *Runner) ConfirmFreshConnection() error {
	return ErrNoConnection
}

// ObservedAddress returns ErrNoConnection.
func (r *Runner) ObservedAddress(string) (string, error) {
	return "", ErrNoConnection
}

// SurvivesSSHRateLimit returns true because commands run locally.
func (r *Runner) SurvivesSSHRateLimit() bool {
	return true
}

func (r *Runner) run(command string, stdin []byte) (string, error) {
	bash, err := r.bash()
	if err != nil {
		return "", err
	}
	//nolint:gosec // bash runs installer commands kip builds itself, the same strings the SSH client sends
	cmd := exec.Command(bash, "-c", command)
	cmd.Dir = r.Dir
	// An explicit environment: an inherited KUBECONFIG would point kubectl at
	// another cluster, and BASH_ENV would run a startup file first.
	cmd.Env = []string{"PATH=" + r.Path, "HOME=" + r.Home}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if trimmed := bytes.TrimSpace(out); len(trimmed) > 0 {
			return string(out), fmt.Errorf("running %q on this machine: %w: %s", command, err, trimmed)
		}
		return string(out), fmt.Errorf("running %q on this machine: %w", command, err)
	}
	return string(out), nil
}
