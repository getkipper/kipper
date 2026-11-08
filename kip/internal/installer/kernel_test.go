package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// kernelHost is a fake host in a temporary directory: kernels in boot, a GRUB
// configuration, and update-grub, dpkg, apt-mark, uname and journalctl stubs.
// holds logs every hold and unhold, and held lists the packages on hold.
// aptFails creates a flag file that makes apt-mark fail the chosen subcommand.
// update-grub fails while the pin file matches a pattern in failOn, and while
// failWithoutPin exists and there is no pin.
type kernelHost struct {
	dir, boot, grubCfg, pinFile, bin, holds, held, failOn, failWithoutPin string
}

const grubEntries = `menuentry 'Ubuntu' --class ubuntu $menuentry_id_option 'gnulinux-simple-5781b336' {
submenu 'Advanced options for Ubuntu' $menuentry_id_option 'gnulinux-advanced-5781b336' {
	menuentry 'Ubuntu, with Linux 7.0.0-42-generic' --class ubuntu $menuentry_id_option 'gnulinux-7.0.0-42-generic-advanced-5781b336' {
	menuentry 'Ubuntu, with Linux 7.0.0-38-generic' --class ubuntu $menuentry_id_option 'gnulinux-7.0.0-38-generic-advanced-5781b336' {
`

// flatGrubEntries is the menu GRUB generates with GRUB_DISABLE_SUBMENU=y.
const flatGrubEntries = `menuentry 'Ubuntu' --class ubuntu $menuentry_id_option 'gnulinux-simple-5781b336' {
menuentry 'Ubuntu, with Linux 7.0.0-42-generic' --class ubuntu $menuentry_id_option 'gnulinux-7.0.0-42-generic-advanced-5781b336' {
menuentry 'Ubuntu, with Linux 7.0.0-38-generic' --class ubuntu $menuentry_id_option 'gnulinux-7.0.0-38-generic-advanced-5781b336' {
`

func newKernelHost(t *testing.T, kernels ...string) kernelHost {
	t.Helper()
	dir := t.TempDir()
	h := kernelHost{
		dir:     dir,
		boot:    filepath.Join(dir, "boot"),
		grubCfg: filepath.Join(dir, "boot", "grub.cfg"),
		pinFile: filepath.Join(dir, "grub.d", "99-kipper-kernel.cfg"),
		bin:     filepath.Join(dir, "bin"),
		holds:   filepath.Join(dir, "holds"),
		held:    filepath.Join(dir, "held"),
		failOn:  filepath.Join(dir, "grub-fails-on"),

		failWithoutPin: filepath.Join(dir, "grub-fails-without-pin"),
	}
	require.NoError(t, os.MkdirAll(h.boot, 0o750))
	require.NoError(t, os.MkdirAll(h.bin, 0o750))
	for _, k := range kernels {
		require.NoError(t, os.WriteFile(filepath.Join(h.boot, "vmlinuz-"+k), nil, 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "menu"), []byte(grubEntries), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "menu-flat"), []byte(flatGrubEntries), 0o600))
	stub := func(name, body string) { h.stub(t, name, body) }
	stub("update-grub", `[ -e `+h.failOn+` ] && grep -qf `+h.failOn+` `+h.pinFile+` 2>/dev/null && { echo "/usr/sbin/grub-mkconfig: No space left on device" >&2; exit 1; }
[ -e `+h.failWithoutPin+` ] && [ ! -e `+h.pinFile+` ] && { echo "/usr/sbin/grub-mkconfig: No space left on device" >&2; exit 1; }
GRUB_DEFAULT=0
for f in `+filepath.Join(dir, "grub.d")+`/*.cfg; do [ -e "$f" ] && . "$f"; done
menu=`+filepath.Join(dir, "menu")+`
[ -e `+filepath.Join(dir, "flat-menu")+` ] && menu=`+filepath.Join(dir, "menu-flat")+`
{ printf '   set default="%s"\n' "$GRUB_DEFAULT"; cat "$menu"; } > `+h.grubCfg)
	stub("dpkg", "exit 0")
	stub("apt-mark", `[ -e `+filepath.Join(dir, "apt-fails-")+`"$1" ] && exit 1
case "$1" in
showhold) cat `+h.held+` 2>/dev/null; true ;;
hold) echo "hold $2" >> `+h.holds+`; grep -qx "$2" `+h.held+` 2>/dev/null || echo "$2" >> `+h.held+` ;;
unhold) echo "unhold $2" >> `+h.holds+`; grep -vx "$2" `+h.held+` > `+h.held+`.tmp; mv `+h.held+`.tmp `+h.held+` ;;
esac`)
	env := filepath.Join(dir, "grubenv")
	stub("grub-editenv", `[ -e `+filepath.Join(dir, "editenv-fails")+` ] && [ "$2" = unset ] && exit 1
case "$2" in
list) cat `+env+` 2>/dev/null; true ;;
unset) grep -v "^$3=" `+env+` > `+env+`.tmp; mv `+env+`.tmp `+env+` ;;
esac`)
	stub("uname", "echo 7.0.0-42-generic")
	stub("journalctl", "echo 'kernel: Linux version 7.0.0-42-generic'")
	return h
}

// stub puts an executable shell script named name on the fake host's PATH.
func (h kernelHost) stub(t *testing.T, name, body string) {
	t.Helper()
	//nolint:gosec // G306: the stub stands in for a host command on PATH, so it has to be executable.
	require.NoError(t, os.WriteFile(filepath.Join(h.bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755))
}

func (h kernelHost) run(t *testing.T, command string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = append(os.Environ(), "PATH="+h.bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

// pinOlder pins 7.0.0-38-generic, the older of the two kernels in grubEntries.
func (h kernelHost) pinOlder(t *testing.T) string {
	t.Helper()
	return h.pinVersion(t, "7.0.0-38-generic")
}

func (h kernelHost) pinVersion(t *testing.T, version string) string {
	t.Helper()
	return h.run(t, pinKernelCommand(version, h.pinFile, h.boot, h.grubCfg))
}

// aptFails makes apt-mark fail subcommand until the returned function runs.
func (h kernelHost) aptFails(t *testing.T, subcommand string) func() {
	t.Helper()
	flag := filepath.Join(h.dir, "apt-fails-"+subcommand)
	require.NoError(t, os.WriteFile(flag, nil, 0o600))
	return func() { require.NoError(t, os.Remove(flag)) }
}

func (h kernelHost) file(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(t, err)
	return string(b)
}

const (
	defaultOlder = `set default="gnulinux-advanced-5781b336>gnulinux-7.0.0-38-generic-advanced-5781b336"`
	defaultNewer = `set default="gnulinux-advanced-5781b336>gnulinux-7.0.0-42-generic-advanced-5781b336"`
)

func (h kernelHost) state(t *testing.T) KernelState {
	t.Helper()
	state, err := parseKernelState(h.run(t, kernelStateCommand(h.pinFile, h.boot)))
	require.NoError(t, err)
	return state
}

func TestPinKernelMakesGRUBBootTheVersionAndHoldsItsPackage(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	h.run(t, "update-grub")

	out := h.pinOlder(t)

	assert.Contains(t, out, "KIPPER-PINNED")
	cfg, err := os.ReadFile(h.grubCfg)
	require.NoError(t, err)
	assert.Contains(t, string(cfg), `set default="gnulinux-advanced-5781b336>gnulinux-7.0.0-38-generic-advanced-5781b336"`,
		"the next boot must use the pinned kernel")
	holds, err := os.ReadFile(h.holds)
	require.NoError(t, err)
	assert.Equal(t, "hold linux-image-7.0.0-38-generic\n", string(holds), "automatic updates must keep the pinned kernel installed")
	assert.Equal(t, "7.0.0-38-generic", h.state(t).Pinned)
}

func TestPinKernelRefusesAKernelThatIsNotInstalled(t *testing.T) {
	h := newKernelHost(t, "7.0.0-42-generic")
	h.run(t, "update-grub")

	out := h.pinOlder(t)

	assert.Contains(t, out, "KIPPER-KERNEL-MISSING")
	assert.NoFileExists(t, h.pinFile)
}

func TestPinKernelRemovesAPinThatGRUBDoesNotUse(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	// A later drop-in overrides Kipper's choice.
	require.NoError(t, os.MkdirAll(filepath.Dir(h.pinFile), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(h.pinFile), "zz-local.cfg"), []byte("GRUB_DEFAULT=0\n"), 0o600))
	h.run(t, "update-grub")

	out := h.pinOlder(t)

	assert.Contains(t, out, "KIPPER-NOT-DEFAULT")
	assert.NoFileExists(t, h.pinFile, "a pin GRUB ignores must not stay behind looking active")
	assert.NoFileExists(t, h.holds)
}

func TestUnpinKernelRestoresGRUBsDefaultAndReleasesTheHold(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	h.run(t, "update-grub")
	h.pinOlder(t)

	out := h.run(t, unpinKernelCommand(h.pinFile))

	assert.Contains(t, out, "KIPPER-UNPINNED=7.0.0-38-generic")
	assert.NoFileExists(t, h.pinFile)
	cfg, err := os.ReadFile(h.grubCfg)
	require.NoError(t, err)
	assert.Contains(t, string(cfg), `set default="0"`)
	holds, err := os.ReadFile(h.holds)
	require.NoError(t, err)
	assert.Equal(t, "hold linux-image-7.0.0-38-generic\nunhold linux-image-7.0.0-38-generic\n", string(holds))
}

func TestUnpinKernelWithoutAPinChangesNothing(t *testing.T) {
	h := newKernelHost(t, "7.0.0-42-generic")

	assert.Contains(t, h.run(t, unpinKernelCommand(h.pinFile)), "KIPPER-NOT-PINNED")
}

func TestKernelStateCountsOnlyContainerSignalDenials(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	log := strings.Join([]string{
		`audit: type=1400 apparmor="DENIED" operation="signal" class="signal" profile="cri-containerd.apparmor.d" pid=1 comm="gosu" requested_mask="send" denied_mask="send" signal=urg peer="cri-containerd.apparmor.d//&unconfined"`,
		`audit: type=1400 apparmor="DENIED" operation="signal" class="signal" profile="cri-containerd.apparmor.d" pid=2 comm="velero" signal=urg peer="cri-containerd.apparmor.d//&unconfined"`,
		`audit: type=1400 apparmor="DENIED" operation="open" class="file" profile="snap.who" name="/proc/1/stat" comm="who"`,
		`audit: type=1400 apparmor="DENIED" operation="signal" class="signal" profile="cri-containerd.apparmor.d" pid=3 comm="node-exporter" requested_mask="send" denied_mask="send" signal=term peer="unconfined"`,
	}, "\n")
	h.stub(t, "journalctl", "cat <<'EOF'\n"+log+"\nEOF")

	state := h.state(t)

	assert.Equal(t, "7.0.0-42-generic", state.Running)
	assert.Equal(t, []string{"7.0.0-38-generic", "7.0.0-42-generic"}, state.Installed)
	assert.Equal(t, 2, state.SignalDenials, "file denials and a container signaling a host process say nothing about signals inside containers")
	assert.True(t, state.BlocksContainerSignals())
	assert.Empty(t, state.Pinned)
}

func TestKernelStateWithACleanLog(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic")
	h.stub(t, "journalctl", "echo 'kernel: Linux version 7.0.0-38-generic'")

	assert.False(t, h.state(t).BlocksContainerSignals())
}

func TestPinKernelRejectsAVersionThatIsNotOne(t *testing.T) {
	runner := &routedRunner{}
	for _, bad := range []string{"", "7.0.0-38-generic; reboot", "../../etc/passwd", "$(id)", "linux-7"} {
		assert.Error(t, PinKernel(runner, bad), bad)
	}
	assert.Empty(t, runner.commands, "nothing reaches the host for a malformed version")
}

// A failed update-grub must not leave a pin file that disagrees with the
// generated configuration, or status would report a kernel the host does not boot.
func TestPinKernelKeepsThePreviousPinWhenUpdateGrubFails(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	h.run(t, "update-grub")
	h.pinOlder(t)
	require.NoError(t, os.WriteFile(h.failOn, []byte("7.0.0-42\n"), 0o600))

	out := h.pinVersion(t, "7.0.0-42-generic")

	assert.Contains(t, out, "KIPPER-GRUB-FAILED")
	assert.NotContains(t, out, "KIPPER-RESTORE-FAILED")
	assert.Contains(t, out, "No space left on device", "update-grub's own message names the cause")
	assert.Equal(t, "7.0.0-38-generic", h.state(t).Pinned)
	assert.Contains(t, h.file(t, h.grubCfg), defaultOlder)
	assert.Equal(t, "hold linux-image-7.0.0-38-generic\n", h.file(t, h.holds), "a failed pin changes no hold")
	assert.NoFileExists(t, h.pinFile+".prev")
}

func TestUnpinKernelPutsThePinBackWhenUpdateGrubFails(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	h.run(t, "update-grub")
	h.pinOlder(t)
	require.NoError(t, os.WriteFile(h.failWithoutPin, nil, 0o600))

	out := h.run(t, unpinKernelCommand(h.pinFile))

	assert.Contains(t, out, "KIPPER-GRUB-FAILED")
	assert.NotContains(t, out, "KIPPER-RESTORE-FAILED")
	assert.Equal(t, "7.0.0-38-generic", h.state(t).Pinned, "the pin stays, so a retry finds it")
	assert.Contains(t, h.file(t, h.grubCfg), defaultOlder)
	assert.Equal(t, "hold linux-image-7.0.0-38-generic\n", h.file(t, h.holds))
}

func TestPinKernelReplacingAPinReleasesItsHold(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	h.run(t, "update-grub")
	h.pinOlder(t)

	assert.Contains(t, h.pinVersion(t, "7.0.0-42-generic"), "KIPPER-PINNED")

	assert.Contains(t, h.file(t, h.grubCfg), defaultNewer)
	assert.Equal(t, "hold linux-image-7.0.0-38-generic\nhold linux-image-7.0.0-42-generic\nunhold linux-image-7.0.0-38-generic\n", h.file(t, h.holds),
		"the old kernel stays held until the new one is")
	assert.Equal(t, "linux-image-7.0.0-42-generic\n", h.file(t, h.held))
}

func TestPinKernelAgainKeepsItsOwnHold(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	h.run(t, "update-grub")
	h.pinOlder(t)
	h.pinOlder(t)

	h.run(t, unpinKernelCommand(h.pinFile))

	assert.Empty(t, h.file(t, h.held), "the hold the pin placed goes with it")
}

func TestKernelPinLeavesAnOperatorsHoldAlone(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	require.NoError(t, os.WriteFile(h.held, []byte("linux-image-7.0.0-38-generic\n"), 0o600))
	h.run(t, "update-grub")

	h.pinOlder(t)
	h.run(t, unpinKernelCommand(h.pinFile))

	assert.Empty(t, h.file(t, h.holds), "the operator held the package before the pin")
	assert.Equal(t, "linux-image-7.0.0-38-generic\n", h.file(t, h.held))
}

// An unreadable log leaves the signal check inconclusive.
func TestKernelStateWithAnUnreadableLogIsNotChecked(t *testing.T) {
	h := newKernelHost(t, "7.0.0-42-generic")
	h.stub(t, "journalctl", "exit 1")
	h.stub(t, "dmesg", "exit 1")

	_, err := parseKernelState(h.run(t, kernelStateCommand(h.pinFile, h.boot)))

	assert.ErrorContains(t, err, "could not be read")
}

func TestKernelStateFallsBackToDmesgWhenJournaldFails(t *testing.T) {
	h := newKernelHost(t, "7.0.0-42-generic")
	h.stub(t, "journalctl", "exit 1")
	h.stub(t, "dmesg", `echo 'audit: apparmor="DENIED" operation="signal" class="signal" profile="cri-containerd.apparmor.d" comm="gosu" signal=urg peer="cri-containerd.apparmor.d//&unconfined"'`)

	assert.Equal(t, 1, h.state(t).SignalDenials)
}

func TestPinKernelNamesAFailedRestore(t *testing.T) {
	runner := &routedRunner{routes: []route{{contains: "update-grub", out: "/usr/sbin/grub-mkconfig: No space left on device\nKIPPER-RESTORE-FAILED\nKIPPER-GRUB-FAILED\n"}}}

	err := PinKernel(runner, "7.0.0-42-generic")

	assert.ErrorContains(t, err, "run update-grub on the host")
	assert.ErrorContains(t, err, "update-grub failed (/usr/sbin/grub-mkconfig: No space left on device)")
}

func TestUnpinKernelRetriesAHoldItCouldNotRelease(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	h.run(t, "update-grub")
	h.pinOlder(t)
	recover := h.aptFails(t, "unhold")

	out := h.run(t, unpinKernelCommand(h.pinFile))

	assert.Contains(t, out, "KIPPER-UNHOLD-FAILED= linux-image-7.0.0-38-generic")
	assert.Contains(t, out, "KIPPER-UNPINNED=7.0.0-38-generic")
	assert.Equal(t, "linux-image-7.0.0-38-generic\n", h.file(t, h.held))
	recover()

	out = h.run(t, unpinKernelCommand(h.pinFile))

	assert.Contains(t, out, "KIPPER-NOT-PINNED")
	assert.NotContains(t, out, "KIPPER-UNHOLD-FAILED")
	assert.Empty(t, h.file(t, h.held), "a retry releases the hold the first unpin left behind")
	assert.NoFileExists(t, h.pinFile+".unhold")
}

func TestPinKernelRetriesTheEarlierPinsHold(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	h.run(t, "update-grub")
	h.pinOlder(t)
	recover := h.aptFails(t, "unhold")

	out := h.pinVersion(t, "7.0.0-42-generic")

	assert.Contains(t, out, "KIPPER-PINNED")
	assert.Contains(t, out, "KIPPER-UNHOLD-FAILED= linux-image-7.0.0-38-generic")
	recover()

	out = h.pinVersion(t, "7.0.0-42-generic")

	assert.NotContains(t, out, "KIPPER-UNHOLD-FAILED")
	assert.Equal(t, "linux-image-7.0.0-42-generic\n", h.file(t, h.held))
}

func TestPinKernelKeepsTheEarlierHoldWhenTheNewOneFails(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	h.run(t, "update-grub")
	h.pinOlder(t)
	recover := h.aptFails(t, "hold")

	assert.Contains(t, h.pinVersion(t, "7.0.0-42-generic"), "KIPPER-HOLD-FAILED")

	assert.Equal(t, "linux-image-7.0.0-38-generic\n", h.file(t, h.held), "some kernel the operator chose stays held")
	recover()
	assert.Contains(t, h.pinVersion(t, "7.0.0-42-generic"), "KIPPER-PINNED")
	assert.Equal(t, "linux-image-7.0.0-42-generic\n", h.file(t, h.held))
}

func TestPinKernelOwnsAHoldItWasStillReleasing(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	h.run(t, "update-grub")
	h.pinOlder(t)
	recover := h.aptFails(t, "unhold")
	h.pinVersion(t, "7.0.0-42-generic")
	recover()

	h.pinOlder(t)

	assert.Equal(t, "linux-image-7.0.0-38-generic\n", h.file(t, h.held), "the older kernel's hold came from Kipper, so the pin keeps it")
	h.run(t, unpinKernelCommand(h.pinFile))
	assert.Empty(t, h.file(t, h.held))
}

func TestPinKernelChangesNothingWhenHoldsCannotBeListed(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	h.run(t, "update-grub")
	h.aptFails(t, "showhold")

	assert.Contains(t, h.pinOlder(t), "KIPPER-HOLDS-UNKNOWN")

	assert.NoFileExists(t, h.pinFile)
	assert.NoFileExists(t, h.pinFile+".prev")
	assert.Contains(t, h.file(t, h.grubCfg), `set default="0"`)
}

// Simulate a failed GRUB refresh that leaves grub.cfg truncated.
func TestPinKernelWarnsThatAFailedRefreshCanBreakGRUB(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	h.stub(t, "update-grub", ": > "+h.grubCfg+"; echo '/usr/sbin/grub-mkconfig: No space left on device' >&2; exit 1")

	out := h.pinOlder(t)

	assert.Contains(t, out, "KIPPER-GRUB-REFRESH-FAILED")
	assert.NoFileExists(t, h.pinFile)
	err := PinKernel(&routedRunner{routes: []route{{contains: "update-grub", out: out}}}, "7.0.0-38-generic")
	assert.ErrorContains(t, err, "update-grub failed (/usr/sbin/grub-mkconfig: No space left on device) before the kernel was pinned")
	assert.ErrorContains(t, err, "check "+grubConfig+" before rebooting")
	assert.NotContains(t, err.Error(), "boots as before")
}

func TestKernelPinReportsAHoldItCouldNotRelease(t *testing.T) {
	runner := &routedRunner{routes: []route{{contains: "update-grub", out: "KIPPER-UNHOLD-FAILED= linux-image-7.0.0-38-generic\nKIPPER-PINNED\n"}}}

	err := PinKernel(runner, "7.0.0-42-generic")

	assert.ErrorContains(t, err, "kernel 7.0.0-42-generic is pinned, but apt-mark could not release the hold an earlier pin placed on linux-image-7.0.0-38-generic. Run the same command again to retry")

	runner = &routedRunner{routes: []route{{contains: "update-grub", out: "KIPPER-UNHOLD-FAILED= linux-image-7.0.0-38-generic\nKIPPER-UNPINNED=7.0.0-38-generic\n"}}}

	_, err = UnpinKernel(runner)

	assert.ErrorContains(t, err, "could not release the hold on linux-image-7.0.0-38-generic. Run the same command again to retry",
		"a retry has to reach the same node, so the hint must not drop --host")
}

func TestPinKernelSaysWhenTheKernelIsNotInstalled(t *testing.T) {
	err := PinKernel(&routedRunner{routes: []route{{contains: "update-grub", out: "KIPPER-KERNEL-MISSING\n"}}}, "7.0.0-30-generic")

	assert.ErrorIs(t, err, ErrKernelNotInstalled)
	assert.NotContains(t, err.Error(), "kip node", "only the caller knows which node to list")
}

// GRUB boots a one-time selection made with grub-reboot before its default.
func TestPinKernelClearsAOneTimeBootSelection(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	h.run(t, "update-grub")
	env := filepath.Join(h.dir, "grubenv")
	require.NoError(t, os.WriteFile(env, []byte("saved_entry=0\nnext_entry=gnulinux-advanced-5781b336>gnulinux-7.0.0-42-generic-advanced-5781b336\n"), 0o600))

	assert.Contains(t, h.pinOlder(t), "KIPPER-PINNED")

	assert.Equal(t, "saved_entry=0\n", h.file(t, env))
}

func TestPinKernelSaysWhenAOneTimeBootSelectionStays(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	h.run(t, "update-grub")
	require.NoError(t, os.WriteFile(filepath.Join(h.dir, "grubenv"), []byte("next_entry=2\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(h.dir, "editenv-fails"), nil, 0o600))

	out := h.pinOlder(t)

	assert.Contains(t, out, "KIPPER-NEXT-ENTRY=2")
	err := PinKernel(&routedRunner{routes: []route{{contains: "update-grub", out: out}}}, "7.0.0-38-generic")
	assert.ErrorContains(t, err, "kernel 7.0.0-38-generic is pinned, but GRUB boots its one-time selection 2 first")
	assert.ErrorContains(t, err, "grub-editenv - unset next_entry")
}

func TestPinKernelSaysWhenTheBootEnvironmentCannotBeRead(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	h.run(t, "update-grub")
	h.stub(t, "grub-editenv", "exit 1")

	out := h.pinOlder(t)

	assert.Contains(t, out, "KIPPER-GRUBENV-UNREADABLE")
	err := PinKernel(&routedRunner{routes: []route{{contains: "update-grub", out: out}}}, "7.0.0-38-generic")
	assert.ErrorContains(t, err, "kernel 7.0.0-38-generic is pinned, but reading GRUB's environment failed")
	assert.ErrorContains(t, err, "grub-editenv - list")
}

// A grub.cfg generated before GRUB_DISABLE_SUBMENU=y still shows the submenu
// that the next update-grub drops.
func TestPinKernelUsesTheMenuGRUBGeneratesNow(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	h.run(t, "update-grub")
	require.NoError(t, os.WriteFile(filepath.Join(h.dir, "flat-menu"), nil, 0o600))

	assert.Contains(t, h.pinOlder(t), "KIPPER-PINNED")

	assert.Contains(t, h.file(t, h.grubCfg), `set default="gnulinux-7.0.0-38-generic-advanced-5781b336"`)
}

func TestPinKernelRefusesAKernelWithoutAMenuEntry(t *testing.T) {
	h := newKernelHost(t, "7.0.0-30-generic", "7.0.0-38-generic", "7.0.0-42-generic")
	require.NoError(t, os.MkdirAll(filepath.Dir(h.pinFile), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(h.pinFile), "50-local.cfg"),
		[]byte(`GRUB_DEFAULT="gnulinux-advanced-5781b336>gnulinux-7.0.0-30-generic-advanced-5781b336"`+"\n"), 0o600))

	out := h.pinVersion(t, "7.0.0-30-generic")

	assert.Contains(t, out, "KIPPER-NO-GRUB-ENTRY", "a set default line naming the kernel is not a menu entry")
	assert.NoFileExists(t, h.pinFile)
}

func TestPinKernelPinsADebianKernel(t *testing.T) {
	h := newKernelHost(t, "6.12.113+deb13-amd64")
	require.NoError(t, os.WriteFile(filepath.Join(h.dir, "menu"), []byte(`submenu 'Advanced options for Debian GNU/Linux' $menuentry_id_option 'gnulinux-advanced-5781b336' {
	menuentry 'Debian GNU/Linux, with Linux 6.12.113+deb13-amd64' --class debian $menuentry_id_option 'gnulinux-6.12.113+deb13-amd64-advanced-5781b336' {
`), 0o600))

	assert.Contains(t, h.pinVersion(t, "6.12.113+deb13-amd64"), "KIPPER-PINNED")

	assert.Contains(t, h.file(t, h.grubCfg), `set default="gnulinux-advanced-5781b336>gnulinux-6.12.113+deb13-amd64-advanced-5781b336"`)
}

func TestKernelStateLeavesOutRescueImages(t *testing.T) {
	h := newKernelHost(t, "0-rescue-5781b336", "6.12.0-55.el10.x86_64")

	assert.Equal(t, []string{"6.12.0-55.el10.x86_64"}, h.state(t).Installed)
}

func TestPinKernelSaysWhenNoPackageWasHeld(t *testing.T) {
	h := newKernelHost(t, "7.0.0-38-generic", "7.0.0-42-generic")
	h.run(t, "update-grub")
	h.stub(t, "dpkg", "exit 1")

	out := h.pinOlder(t)

	assert.Contains(t, out, "KIPPER-NOT-HELD")
	assert.Contains(t, h.file(t, h.grubCfg), defaultOlder)
	err := PinKernel(&routedRunner{routes: []route{{contains: "update-grub", out: out}}}, "7.0.0-38-generic")
	assert.ErrorContains(t, err, "kernel 7.0.0-38-generic is pinned. No linux-image-7.0.0-38-generic package is registered with dpkg, so no package hold protects this kernel from removal by automatic updates")
}

func TestUnpinKernelWithoutAPinNamesAHoldItCouldNotRelease(t *testing.T) {
	runner := &routedRunner{routes: []route{{contains: "unhold", out: "KIPPER-UNHOLD-FAILED= linux-image-7.0.0-38-generic\nKIPPER-NOT-PINNED\n"}}}

	_, err := UnpinKernel(runner)

	assert.ErrorContains(t, err, "no kernel is pinned, but apt-mark could not release the hold an earlier pin placed on linux-image-7.0.0-38-generic")
}
