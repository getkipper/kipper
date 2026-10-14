package installjob

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// HostSystemd drives the server's systemd through systemd-run and systemctl.
type HostSystemd struct{}

// systemdRunArgs delegates log redirection to the unit's shell. With SELinux enforcing, systemd
// (init_t) may not open the job's admin_home_t log for append, so StandardOutput=append: fails.
// Passing logPath as $0 keeps argv in "$@" and preserves argument boundaries.
func systemdRunArgs(unit string, argv []string, logPath string) []string {
	args := []string{
		"--unit", unit, "--collect", "--quiet",
		"-p", "User=root", "-p", "WorkingDirectory=/root", "--setenv=HOME=/root",
		"--", "/bin/sh", "-c", `exec "$@" >>"$0" 2>&1`, logPath,
	}
	return append(args, argv...)
}

// Start runs argv as a transient unit that systemd removes when it ends.
func (HostSystemd) Start(unit string, argv []string, logPath string) error {
	out, err := exec.Command("systemd-run", systemdRunArgs(unit, argv, logPath)...).CombinedOutput() //nolint:gosec // the unit, the job's log path and kip's own argv, all built by kip
	if err != nil {
		if strings.Contains(string(out), "already exists") {
			return fmt.Errorf("unit %s: %w", unit, os.ErrExist)
		}
		return fmt.Errorf("systemd-run: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Active treats every non-empty state except inactive and failed as busy.
func (HostSystemd) Active(unit string) (bool, error) {
	// Read the printed state so transitions such as activating and
	// deactivating count as busy regardless of the exit status.
	out, err := exec.Command("systemctl", "is-active", unit).Output() //nolint:gosec // a unit name built from a validated run id
	busy, parseErr := unitBusy(string(out))
	if parseErr != nil {
		if err != nil {
			return false, fmt.Errorf("systemctl is-active %s: %w", unit, err)
		}
		return false, parseErr
	}
	return busy, nil
}

// unitBusy treats unknown non-empty states as busy to avoid starting
// a concurrent install.
func unitBusy(state string) (bool, error) {
	switch s := strings.TrimSpace(state); s {
	case "inactive", "failed":
		return false, nil
	case "":
		return false, errors.New("systemctl is-active returned no state")
	default:
		return true, nil
	}
}

func (HostSystemd) Stop(unit string) error {
	out, err := exec.Command("systemctl", "stop", unit).CombinedOutput() //nolint:gosec // a unit name built from a validated run id
	if err != nil {
		return fmt.Errorf("systemctl stop %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}
