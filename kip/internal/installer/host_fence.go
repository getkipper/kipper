package installer

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FenceDropInName is the systemd drop-in that keeps a fenced worker's k3s
// agent from starting. It sorts late so it lands after the usual drop-ins,
// and kip verifies the loaded unit anyway because a later one could reset it.
const FenceDropInName = "90-kipper-fence.conf"

// fenceUnit is the k3s unit of a worker, the only role kip fences.
func fenceUnit() string { return RoleWorker.k3sUnit() + ".service" }

// FenceDropInPath is where the fence interlock lives on a worker.
func FenceDropInPath() string {
	return "/etc/systemd/system/" + fenceUnit() + ".d/" + FenceDropInName
}

// fenceDropIn asserts that the drop-in itself does not exist, which fails for
// as long as it does. A failed assert makes every start of the unit fail with
// an error instead of being skipped silently, and it survives reboots, manual
// starts and the k3s install script, which never touches the .d directory.
func fenceDropIn() string {
	return "# kip node fence keeps the k3s agent stopped while this file exists.\n" +
		"# Remove it only by rejoining the node with 'kip node add'.\n" +
		"[Unit]\nAssertPathExists=!" + FenceDropInPath() + "\n"
}

// unitObjectPath returns the systemd D-Bus object path of unit, escaped the
// way sd-bus escapes object path labels: ASCII letters and digits stay, every
// other byte becomes _ and two lowercase hex digits.
func unitObjectPath(unit string) string {
	var b strings.Builder
	for i := 0; i < len(unit); i++ {
		c := unit[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "_%02x", c)
		}
	}
	return "/org/freedesktop/systemd1/unit/" + b.String()
}

// busctlFailed marks an answer in which busctl could not read the property.
const busctlFailed = "kip-busctl-failed"

// assertsCommand prints the unit's load state, then its effective assert list
// as JSON, or busctl's error followed by busctlFailed. systemctl show does not
// print the list, so it is read from the loaded unit's D-Bus Asserts
// property, which reflects every drop-in after systemd applied list resets.
func assertsCommand() string {
	return fmt.Sprintf("systemctl show -p LoadState --value %s && { busctl --json=short get-property org.freedesktop.systemd1 %s org.freedesktop.systemd1.Unit Asserts || echo %s; }",
		fenceUnit(), unitObjectPath(fenceUnit()), busctlFailed)
}

// readAsserts returns the loaded unit's assert list. It fails unless the unit
// is loaded.
func readAsserts(out string) ([][]any, error) {
	state, rest, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if strings.TrimSpace(state) != "loaded" {
		return nil, fmt.Errorf("systemd reports %s as %q, not loaded", fenceUnit(), strings.TrimSpace(state))
	}
	if before, _, failed := strings.Cut(rest, busctlFailed); failed {
		return nil, fmt.Errorf("busctl could not read the assert list of %s (%s). busctl talks to systemd through the system D-Bus, so check that dbus-daemon or dbus-broker runs on the host", fenceUnit(), strings.TrimSpace(before))
	}
	var prop struct {
		Type string  `json:"type"`
		Data [][]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(rest)), &prop); err != nil || prop.Type != "a(sbbsi)" {
		return nil, fmt.Errorf("reading the assert list of %s: unexpected answer %q", fenceUnit(), strings.TrimSpace(rest))
	}
	return prop.Data, nil
}

// hasFenceAssert reports whether asserts holds kip's negated, non-triggering
// AssertPathExists on the drop-in's own path.
func hasFenceAssert(asserts [][]any) bool {
	for _, a := range asserts {
		if len(a) != 5 {
			continue
		}
		kind, _ := a[0].(string)
		trigger, _ := a[1].(bool)
		negate, _ := a[2].(bool)
		param, _ := a[3].(string)
		if kind == "AssertPathExists" && !trigger && negate && param == FenceDropInPath() {
			return true
		}
	}
	return false
}

// checkFenceAsserts fails unless out shows kip's assert in the loaded unit.
func checkFenceAsserts(out string) error {
	asserts, err := readAsserts(out)
	if err != nil {
		return err
	}
	if !hasFenceAssert(asserts) {
		return fmt.Errorf("the loaded %s unit does not carry AssertPathExists=!%s. Another drop-in that sorts later may reset the assert list. Check 'systemctl cat %s'", fenceUnit(), FenceDropInPath(), fenceUnit())
	}
	return nil
}

// VerifyFenceInterlock checks that the fence drop-in exists and that the
// loaded k3s agent unit carries its assert, so no start of the agent can
// succeed. The assert blocks a start only while the file exists.
func VerifyFenceInterlock(host CommandRunner) error {
	present, err := HasFenceInterlock(host)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%s is gone, so nothing stops the k3s agent from starting", FenceDropInPath())
	}
	out, err := host.Run(assertsCommand())
	if err != nil {
		return fmt.Errorf("reading the assert list of %s: %w", fenceUnit(), err)
	}
	return checkFenceAsserts(out)
}

// InstallFenceInterlock writes the fence drop-in, reloads systemd and
// verifies that the loaded unit carries the assert. Writing an existing
// drop-in again is harmless.
func InstallFenceInterlock(host CommandRunner) error {
	path := FenceDropInPath()
	dir := path[:strings.LastIndex(path, "/")]
	cmd := fmt.Sprintf("mkdir -p %[1]s && cat > %[2]s.tmp && mv -f %[2]s.tmp %[2]s && systemctl daemon-reload", dir, path)
	if _, err := host.RunStdin(cmd, strings.NewReader(fenceDropIn())); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return VerifyFenceInterlock(host)
}

// HasFenceInterlock reports whether the fence drop-in file exists.
func HasFenceInterlock(host CommandRunner) (bool, error) {
	out, err := host.Run(fmt.Sprintf("if test -e %s; then echo present; else echo absent; fi", FenceDropInPath()))
	if err != nil {
		return false, fmt.Errorf("looking for %s: %w", FenceDropInPath(), err)
	}
	switch strings.TrimSpace(out) {
	case "present":
		return true, nil
	case "absent":
		return false, nil
	}
	return false, fmt.Errorf("looking for %s: unexpected answer %q", FenceDropInPath(), strings.TrimSpace(out))
}

// RemoveFenceInterlock deletes the fence drop-in, reloads systemd and checks
// that the loaded unit no longer carries the assert. Call it during a guarded
// rejoin or when clearing an interlock from an earlier cluster membership.
func RemoveFenceInterlock(host CommandRunner) error {
	if _, err := host.Run(fmt.Sprintf("rm -f %s && systemctl daemon-reload", FenceDropInPath())); err != nil {
		return fmt.Errorf("removing %s: %w", FenceDropInPath(), err)
	}
	out, err := host.Run(assertsCommand())
	if err != nil {
		return fmt.Errorf("reading the assert list of %s: %w", fenceUnit(), err)
	}
	asserts, err := readAsserts(out)
	if err != nil {
		return err
	}
	if hasFenceAssert(asserts) {
		return fmt.Errorf("the loaded %s unit still carries the fence assert after removing %s", fenceUnit(), FenceDropInPath())
	}
	return nil
}

// quiescenceScript reports the agent's state and anything a fenced host must
// no longer run: container shims, Longhorn processes and mounted Longhorn
// devices. The patterns are written so that the shell running the script
// does not match them itself.
func quiescenceScript() string {
	return `echo "agent=$(systemctl is-active ` + RoleWorker.k3sUnit() + ` 2>/dev/null)"
pgrep -a -f '[c]ontainerd-sh[i]m' | sed 's/^/S /'
pgrep -a -f '[l]onghor[n]' | sed 's/^/L /'
awk '$1 ~ "^/dev/[l]onghorn/" || $2 ~ "driver[.][l]onghorn[.]io" || $2 ~ "kubernetes[.]io~csi" { print "M " $1 " " $2 }' /proc/mounts
true`
}

// parseQuiescence fails unless the agent is inactive or failed and nothing
// else was reported, naming whatever is left.
func parseQuiescence(out string) error {
	agent := ""
	var left []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "agent="):
			agent = strings.TrimPrefix(line, "agent=")
		case strings.HasPrefix(line, "S "):
			left = append(left, "container shim "+strings.TrimPrefix(line, "S "))
		case strings.HasPrefix(line, "L "):
			left = append(left, "Longhorn process "+strings.TrimPrefix(line, "L "))
		case strings.HasPrefix(line, "M "):
			left = append(left, "Longhorn mount "+strings.TrimPrefix(line, "M "))
		default:
			left = append(left, "unexpected output "+line)
		}
	}
	if agent != "inactive" && agent != "failed" {
		left = append([]string{fmt.Sprintf("the k3s agent is %q", agent)}, left...)
	}
	if len(left) > 0 {
		return fmt.Errorf("the host is still running parts of the node: %s", strings.Join(left, "; "))
	}
	return nil
}

// VerifyQuiescent checks that the host's agent is stopped and that no
// container shim, Longhorn process or mounted Longhorn device remains.
func VerifyQuiescent(host CommandRunner) error {
	out, err := host.Run(quiescenceScript())
	if err != nil {
		return fmt.Errorf("checking that the node stopped: %w", err)
	}
	return parseQuiescence(out)
}

// StopFencedAgent runs k3s-killall.sh, which stops the agent and every
// container including Longhorn's, then verifies that the host is quiescent.
// Install the interlock first, or the agent can start again.
func StopFencedAgent(host CommandRunner) error {
	if _, err := host.Run("systemctl stop " + RoleWorker.k3sUnit() + " 2>/dev/null; if [ -x /usr/local/bin/k3s-killall.sh ]; then /usr/local/bin/k3s-killall.sh >/dev/null 2>&1; fi; true"); err != nil {
		return fmt.Errorf("running k3s-killall.sh: %w", err)
	}
	return VerifyQuiescent(host)
}

// ClearLeftoverInterlock removes a fence drop-in so the agent can start. The
// caller must first verify that the host has no fenced record, as after a
// forced removal. It reports whether the file existed, even if removal fails.
func ClearLeftoverInterlock(host CommandRunner) (bool, error) {
	present, err := HasFenceInterlock(host)
	if err != nil || !present {
		return false, err
	}
	return true, RemoveFenceInterlock(host)
}
