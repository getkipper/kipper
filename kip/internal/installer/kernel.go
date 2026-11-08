package installer

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	kernelPinFile   = "/etc/default/grub.d/99-kipper-kernel.cfg"
	kernelPinMarker = "# kipper-kernel-pin="
	// kernelHoldMarker tracks the hold created by the pin so cleanup preserves
	// holds created by the operator.
	kernelHoldMarker = "# kipper-kernel-hold="
	bootDir          = "/boot"
	grubConfig       = "/boot/grub/grub.cfg"
)

var kernelVersionPattern = regexp.MustCompile(`^[0-9][0-9A-Za-z.+~_-]*$`)

// kernelStateCheck is the command kip status runs on each node, part of the
// batch of host checks on workers.
var kernelStateCheck = kernelStateCommand(kernelPinFile, bootDir)

// KernelState describes the kernels on one host. Pinned is empty without a
// pin. SignalDenials counts AppArmor denials of signals sent inside containers
// since the host booted.
type KernelState struct {
	Running       string
	Pinned        string
	Installed     []string
	SignalDenials int
}

// BlocksContainerSignals reports whether the kernel log contains AppArmor
// signal denials between container profiles. These can block Go programs from
// signaling their own threads and hang the Postgres image's startup helper.
func (k KernelState) BlocksContainerSignals() bool {
	return k.SignalDenials > 0
}

// RunsPinned reports whether the host is running its pinned kernel. In that
// case, a newer installed kernel does not require a reboot.
func (k KernelState) RunsPinned() bool {
	return k.Pinned != "" && k.Pinned == k.Running
}

// kernelStateCommand prints one key=value line per fact. The kernel log comes
// from journald, or from dmesg when journald gives nothing. A booted kernel
// normally has messages, so an empty log is treated as unavailable and the
// denial count is "unknown". pinFile and boot must be safe to use unquoted
// in a shell.
func kernelStateCommand(pinFile, boot string) string {
	return fmt.Sprintf(`echo "running=$(uname -r)"
sed -n 's/^%[3]s//p' %[1]s 2>/dev/null | head -n1 | sed 's/^/pinned=/'
for f in %[2]s/vmlinuz-*; do case "$f" in *-rescue-*) continue ;; esac; [ -e "$f" ] && echo "installed=${f#%[2]s/vmlinuz-}"; done
denied='apparmor="DENIED" operation="signal".*profile="cri-containerd.apparmor.d".*peer="cri-containerd.apparmor.d'
if command -v journalctl >/dev/null 2>&1 && [ -n "$(journalctl -k -b 0 -q --no-pager -n 1 2>/dev/null)" ]; then
  echo "denials=$(journalctl -k -b 0 -q --no-pager 2>/dev/null | grep -c "$denied")"
elif [ -n "$(dmesg 2>/dev/null | head -n1)" ]; then
  echo "denials=$(dmesg 2>/dev/null | grep -c "$denied")"
else
  echo denials=unknown
fi`,
		pinFile, boot, kernelPinMarker)
}

// ReadKernelState reads the running, pinned, and installed kernels and counts
// container signal denials in the available kernel log since boot. It needs root
// to read the log and returns an error if the log is unreadable or empty, the
// denial count is invalid, or the running version is missing. Installed kernels
// exclude rescue images and follow shell glob order. Pinned is empty when
// no pin marker is found. Denials recorded only by an audit daemon are absent
// from this count.
func ReadKernelState(client CommandRunner) (KernelState, error) {
	out, err := client.Run(kernelStateCheck)
	if err != nil {
		return KernelState{}, fmt.Errorf("reading the kernel state: %w", err)
	}
	return parseKernelState(out)
}

func parseKernelState(out string) (KernelState, error) {
	var state KernelState
	for _, line := range strings.Split(out, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "running":
			state.Running = value
		case "pinned":
			state.Pinned = value
		case "installed":
			state.Installed = append(state.Installed, value)
		case "denials":
			if value == "unknown" {
				return KernelState{}, fmt.Errorf("the kernel log could not be read, so blocked container signals were not checked")
			}
			n, err := strconv.Atoi(value)
			if err != nil {
				return KernelState{}, fmt.Errorf("reading the kernel log: unexpected denial count %q", value)
			}
			state.SignalDenials = n
		}
	}
	if state.Running == "" {
		return KernelState{}, fmt.Errorf("reading the kernel state: the running kernel is missing from %q", strings.TrimSpace(out))
	}
	return state, nil
}

// ErrKernelNotInstalled is returned by PinKernel when the requested kernel
// image is missing from /boot.
var ErrKernelNotInstalled = errors.New("not installed")

// pinKernelCommand selects version through a GRUB drop-in and checks that the
// generated configuration uses it. It holds the installed linux-image package
// to protect it from automatic removal, then releases any hold the previous
// pin created. It attempts to restore the previous drop-in and configuration
// if writing or applying the pin fails. It clears one-time boot selections
// because GRUB gives them precedence over the default. Paths must be safe to
// use unquoted in a shell, and version must match kernelVersionPattern.
func pinKernelCommand(version, pinFile, boot, grubCfg string) string {
	re := regexp.QuoteMeta(version)
	return fmt.Sprintf(`v='%[1]s'
pin=%[5]s
`+releasePendingHolds+`
[ -e %[3]s/vmlinuz-$v ] || { echo KIPPER-KERNEL-MISSING; exit 0; }
command -v update-grub >/dev/null 2>&1 || { echo KIPPER-NO-UPDATE-GRUB; exit 0; }
update-grub >/dev/null || { echo KIPPER-GRUB-REFRESH-FAILED; exit 0; }
entry=$(sed -nE "s/^[[:space:]]*menuentry .*menuentry_id_option '(gnulinux-%[2]s-advanced-[0-9A-Za-z-]+)'.*/\1/p" %[4]s | head -n1)
sub=$(sed -nE "s/^[[:space:]]*submenu .*menuentry_id_option '(gnulinux-advanced-[0-9A-Za-z-]+)'.*/\1/p" %[4]s | head -n1)
[ -n "$entry" ] || { echo KIPPER-NO-GRUB-ENTRY; exit 0; }
default=$entry
[ -z "$sub" ] || default="$sub>$entry"
mkdir -p "$(dirname "$pin")" || { echo KIPPER-WRITE-FAILED; exit 0; }
rm -f "$pin.prev"
old_hold=$(sed -n 's/^%[7]s//p' "$pin" 2>/dev/null | head -n1)
if [ -e "$pin" ]; then cp -p "$pin" "$pin.prev" || { echo KIPPER-WRITE-FAILED; exit 0; }; fi
restore() {
  if [ -e "$pin.prev" ]; then mv -f "$pin.prev" "$pin"; else rm -f "$pin"; fi
  update-grub >/dev/null 2>&1 || echo KIPPER-RESTORE-FAILED
}
pkg="linux-image-$v"
hold=
held_pkg=
if dpkg -s "$pkg" >/dev/null 2>&1; then
  held_pkg=yes
  held=$(apt-mark showhold 2>/dev/null) || { rm -f "$pin.prev"; echo KIPPER-HOLDS-UNKNOWN; exit 0; }
  if [ "$old_hold" = "$pkg" ] || grep -qx "$pkg" "$pin.unhold" 2>/dev/null || ! printf '%%s\n' "$held" | grep -qx "$pkg"; then hold=$pkg; fi
fi
{
  printf '# Managed by Kipper: boots the kernel set with kip node kernel pin.\n%[6]s%%s\n' "$v"
  [ -z "$hold" ] || printf '%[7]s%%s\n' "$hold"
  printf 'GRUB_DEFAULT="%%s"\n' "$default"
} > "$pin.tmp" && mv -f "$pin.tmp" "$pin" || { rm -f "$pin.tmp"; restore; echo KIPPER-WRITE-FAILED; exit 0; }
if ! update-grub >/dev/null; then restore; echo KIPPER-GRUB-FAILED; exit 0; fi
if ! grep -qF "set default=\"$default\"" %[4]s; then restore; echo KIPPER-NOT-DEFAULT; exit 0; fi
rm -f "$pin.prev"
if [ -n "$old_hold" ] && [ "$old_hold" != "$hold" ]; then echo "$old_hold" >> "$pin.unhold"; fi
if [ -n "$hold" ] && ! apt-mark hold "$hold" >/dev/null 2>&1; then echo KIPPER-HOLD-FAILED; exit 0; fi
release_pending "$hold"
[ -n "$held_pkg" ] || echo KIPPER-NOT-HELD
if command -v grub-editenv >/dev/null 2>&1; then
  if grubenv=$(grub-editenv - list); then
    next=$(printf '%%s\n' "$grubenv" | sed -n 's/^next_entry=//p')
    if [ -n "$next" ] && ! grub-editenv - unset next_entry; then echo "KIPPER-NEXT-ENTRY=$next"; fi
  else
    echo KIPPER-GRUBENV-UNREADABLE
  fi
fi
echo KIPPER-PINNED`, version, re, boot, grubCfg, pinFile, kernelPinMarker, kernelHoldMarker)
}

// PinKernel selects version for subsequent boots through a GRUB drop-in and
// holds its installed linux-image package against automatic removal. The
// running kernel stays active until reboot. It requires Ubuntu or Debian with
// GRUB. Pinning again replaces the earlier pin and releases holds Kipper created
// once the new hold succeeds, preserving holds created by the operator.
//
// Errors include invalid versions, ErrKernelNotInstalled, and failures to apply
// the GRUB configuration or manage package holds and one-time boot selections.
// An error can leave the pin applied, and its message describes recovery steps.
func PinKernel(client CommandRunner, version string) error {
	if !kernelVersionPattern.MatchString(version) {
		return fmt.Errorf("%q is not a kernel version such as 7.0.0-38-generic", version)
	}
	out, err := client.Run(pinKernelCommand(version, kernelPinFile, bootDir, grubConfig))
	if err != nil {
		return fmt.Errorf("pinning kernel %s: %w", version, err)
	}
	restoreFailed := ""
	if strings.Contains(out, "KIPPER-RESTORE-FAILED") {
		restoreFailed = ". Restoring the previous GRUB configuration failed as well, so run update-grub on the host and check " + grubConfig + " before rebooting it"
	}
	switch {
	case strings.Contains(out, "KIPPER-PINNED"):
		if strings.Contains(out, "KIPPER-GRUBENV-UNREADABLE") {
			return fmt.Errorf("kernel %s is pinned, but reading GRUB's environment failed, so a one-time selection made with grub-reboot may still boot first. Run 'grub-editenv - list' on the host and, if it shows next_entry, 'grub-editenv - unset next_entry' before rebooting it", version)
		}
		if next := markerValue(out, "KIPPER-NEXT-ENTRY="); next != "" {
			return fmt.Errorf("kernel %s is pinned, but GRUB boots its one-time selection %s first, and clearing it failed. Run 'grub-editenv - unset next_entry' on the host before rebooting it", version, next)
		}
		if strings.Contains(out, "KIPPER-NOT-HELD") {
			return fmt.Errorf("kernel %s is pinned. No linux-image-%s package is registered with dpkg, so no package hold protects this kernel from removal by automatic updates", version, version)
		}
		if stuck := unreleasedHolds(out); stuck != "" {
			return fmt.Errorf("kernel %s is pinned, but apt-mark could not release the hold an earlier pin placed on %s. Run the same command again to retry", version, stuck)
		}
		return nil
	case strings.Contains(out, "KIPPER-KERNEL-MISSING"):
		return fmt.Errorf("kernel %s is %w on this host", version, ErrKernelNotInstalled)
	case strings.Contains(out, "KIPPER-NO-UPDATE-GRUB"):
		return fmt.Errorf("this host has no update-grub. Pinning a kernel needs Ubuntu or Debian with GRUB")
	case strings.Contains(out, "KIPPER-NO-GRUB-ENTRY"):
		return fmt.Errorf("GRUB has no boot entry for kernel %s in %s", version, grubConfig)
	case strings.Contains(out, "KIPPER-WRITE-FAILED"):
		return fmt.Errorf("writing %s failed, so the host boots as before%s", kernelPinFile, restoreFailed)
	case strings.Contains(out, "KIPPER-GRUB-REFRESH-FAILED"):
		return fmt.Errorf("update-grub failed%s before the kernel was pinned. A failed update-grub can leave %s incomplete, so run update-grub on the host and check %s before rebooting it", grubSaid(out), grubConfig, grubConfig)
	case strings.Contains(out, "KIPPER-HOLDS-UNKNOWN"):
		return fmt.Errorf("apt-mark could not list the held packages, so nothing was changed and the host boots as before")
	case strings.Contains(out, "KIPPER-GRUB-FAILED"):
		return fmt.Errorf("update-grub failed%s, so the previous pin was put back and the host boots as before%s", grubSaid(out), restoreFailed)
	case strings.Contains(out, "KIPPER-NOT-DEFAULT"):
		return fmt.Errorf("GRUB did not make kernel %s the default, so the previous pin was put back. Another file in /etc/default/grub.d may set GRUB_DEFAULT%s", version, restoreFailed)
	case strings.Contains(out, "KIPPER-HOLD-FAILED"):
		return fmt.Errorf("kernel %s is pinned, but apt-mark could not hold linux-image-%s, so automatic updates may remove it. Run the same command again to retry", version, version)
	}
	return fmt.Errorf("pinning kernel %s: unexpected output %q", version, strings.TrimSpace(out))
}

// releasePendingHolds defines release_pending, which releases the holds listed
// in $pin.unhold except the package given as its argument. A hold apt-mark
// could not release stays listed for the next pin or unpin and is reported as
// KIPPER-UNHOLD-FAILED. The list is not a .cfg file, so GRUB never reads it.
const releasePendingHolds = `release_pending() {
  [ -e "$pin.unhold" ] || return 0
  left=
  for p in $(cat "$pin.unhold"); do
    [ "$p" = "$1" ] && continue
    apt-mark unhold "$p" >/dev/null 2>&1 || left="$left $p"
  done
  if [ -n "$left" ]; then
    for p in $left; do echo "$p"; done > "$pin.unhold"
    echo "KIPPER-UNHOLD-FAILED="$left
  else
    rm -f "$pin.unhold"
  fi
}`

// unpinKernelCommand removes the pin, regenerates the GRUB configuration and
// releases the hold the pin placed, along with any an earlier pin or unpin
// could not release. If update-grub fails, it attempts to restore the pin and
// regenerate the previous configuration. pinFile must be safe to use unquoted
// in a shell.
func unpinKernelCommand(pinFile string) string {
	return fmt.Sprintf(`pin=%[1]s
`+releasePendingHolds+`
[ -e "$pin" ] || { release_pending ""; echo KIPPER-NOT-PINNED; exit 0; }
v=$(sed -n 's/^%[2]s//p' "$pin" | head -n1)
hold=$(sed -n 's/^%[3]s//p' "$pin" | head -n1)
mv -f "$pin" "$pin.unpinning" || { echo KIPPER-WRITE-FAILED; exit 0; }
if ! update-grub >/dev/null; then
  mv -f "$pin.unpinning" "$pin"
  update-grub >/dev/null 2>&1 || echo KIPPER-RESTORE-FAILED
  echo KIPPER-GRUB-FAILED
  exit 0
fi
rm -f "$pin.unpinning"
if [ -n "$hold" ]; then echo "$hold" >> "$pin.unhold"; fi
release_pending ""
echo "KIPPER-UNPINNED=$v"`, pinFile, kernelPinMarker, kernelHoldMarker)
}

// UnpinKernel removes a pin set by PinKernel, so the host boots GRUB's
// default kernel again starting with its next reboot. It releases holds Kipper
// created, including pending releases from earlier attempts, and preserves
// operator-created holds. It returns the previously pinned version, or an empty
// string if no pin existed. Errors return an empty string and can occur after
// the pin is removed, such as when a package hold cannot be released.
func UnpinKernel(client CommandRunner) (string, error) {
	out, err := client.Run(unpinKernelCommand(kernelPinFile))
	if err != nil {
		return "", fmt.Errorf("removing the kernel pin: %w", err)
	}
	if stuck := unreleasedHolds(out); stuck != "" && strings.Contains(out, "KIPPER-NOT-PINNED") {
		return "", fmt.Errorf("no kernel is pinned, but apt-mark could not release the hold an earlier pin placed on %s. Run the same command again to retry", stuck)
	}
	if stuck := unreleasedHolds(out); stuck != "" {
		return "", fmt.Errorf("the kernel pin is removed, but apt-mark could not release the hold on %s. Run the same command again to retry", stuck)
	}
	switch {
	case strings.Contains(out, "KIPPER-NOT-PINNED"):
		return "", nil
	case strings.Contains(out, "KIPPER-WRITE-FAILED"):
		return "", fmt.Errorf("removing %s failed, so the pin stays", kernelPinFile)
	case strings.Contains(out, "KIPPER-RESTORE-FAILED"):
		return "", fmt.Errorf("update-grub failed%s, and so did regenerating the GRUB configuration after putting the pin back. Run update-grub on the host and check %s before rebooting it", grubSaid(out), grubConfig)
	case strings.Contains(out, "KIPPER-GRUB-FAILED"):
		return "", fmt.Errorf("update-grub failed%s, so the pin was put back and the host boots as before", grubSaid(out))
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "KIPPER-UNPINNED="); ok {
			return v, nil
		}
	}
	return "", fmt.Errorf("removing the kernel pin: unexpected output %q", strings.TrimSpace(out))
}

// unreleasedHolds returns the packages, separated by commas, whose hold a pin
// or unpin could not release, or "" when every release succeeded.
func unreleasedHolds(out string) string {
	return strings.Join(strings.Fields(markerValue(out, "KIPPER-UNHOLD-FAILED=")), ", ")
}

// markerValue returns what follows marker on its line of out, or "".
func markerValue(out, marker string) string {
	for _, line := range strings.Split(out, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), marker); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// grubSaid returns up to three trailing non-marker output lines as a
// parenthesized diagnostic, or "" if none remain.
func grubSaid(out string) string {
	var said []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "KIPPER-") {
			said = append(said, line)
		}
	}
	if len(said) == 0 {
		return ""
	}
	if len(said) > 3 {
		said = said[len(said)-3:]
	}
	return " (" + strings.Join(said, ". ") + ")"
}
