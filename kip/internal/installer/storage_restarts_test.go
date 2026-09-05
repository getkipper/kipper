package installer

import (
	"io"
	"strings"
	"testing"
)

// A security update to open-iscsi restarts iscsid. Every Longhorn volume on the
// node holds an iSCSI session against an instance-manager pod, and when the
// daemon goes the sessions go with it: the kernel's block device fails, ext4
// remounts read-only, and the database on top crash-loops until somebody
// recreates the pod. Debian's needrestart does this automatically after any
// unattended upgrade that touches the library.
//
// needrestart parses its configuration as Perl, so the override has to be a
// quoted regex. A bare service name parses as something else entirely and
// needrestart falls back to restarting everything, which is exactly the failure
// this file exists to prevent while looking like it was configured.

type storageHostRunner struct {
	commands []string
	stdin    []string
	replies  map[string]string
	fail     map[string]error
}

func (r *storageHostRunner) Run(command string) (string, error) {
	r.commands = append(r.commands, command)
	for match, err := range r.fail {
		if strings.Contains(command, match) {
			return "", err
		}
	}
	for match, out := range r.replies {
		if strings.Contains(command, match) {
			return out, nil
		}
	}
	return "", nil
}

func (r *storageHostRunner) RunStdin(command string, stdin io.Reader) (string, error) {
	b, _ := io.ReadAll(stdin)
	r.stdin = append(r.stdin, string(b))
	return r.Run(command)
}

func TestStorageRestartConfigIsValidPerl(t *testing.T) {
	cfg := storageRestartConfig()

	for _, unit := range []string{"iscsid", "systemd-networkd", "systemd-resolved"} {
		want := `$nrconf{override_rc}{qr(^` + unit + `\.service$)} = 0;`
		if !strings.Contains(cfg, want) {
			t.Errorf("config does not hold the exact override for %s.\nwant a line: %s\ngot:\n%s", unit, want, cfg)
		}
	}
}

// A bare name is the mistake this guards: it parses, needrestart ignores it, and
// the node restarts iscsid on the next unattended upgrade while the file on disk
// suggests otherwise.
func TestStorageRestartConfigNamesNoUnitWithoutARegex(t *testing.T) {
	for _, wrong := range []string{`{"iscsid"}`, `{iscsid}`, `{'iscsid'}`} {
		if strings.Contains(storageRestartConfig(), wrong) {
			t.Errorf("config holds %s, which needrestart does not apply", wrong)
		}
	}
}

func TestConfigureStorageRestartsWritesTheDropIn(t *testing.T) {
	runner := &storageHostRunner{}

	if err := ConfigureStorageRestarts(runner); err != nil {
		t.Fatalf("ConfigureStorageRestarts: %v", err)
	}

	all := strings.Join(runner.commands, "\n")
	if !strings.Contains(all, "mkdir -p /etc/needrestart/conf.d") {
		t.Error("the directory has to be created: a node without needrestart installed has no conf.d")
	}
	if !strings.Contains(all, needrestartDropIn) {
		t.Errorf("nothing wrote %s; commands were:\n%s", needrestartDropIn, all)
	}
	if len(runner.stdin) != 1 {
		t.Fatalf("sent %d payloads, want the config written once", len(runner.stdin))
	}
	if !strings.Contains(runner.stdin[0], `qr(^iscsid\.service$)`) {
		t.Errorf("the payload is not the config: %q", runner.stdin[0])
	}
}

// A half-written file is broken Perl, and needrestart falling back to restarting
// everything is the incident. The write lands under a temporary name in the same
// directory and is moved into place, so the file at the real path is either the
// previous version or the whole new one.
func TestConfigureStorageRestartsNeverLeavesAPartialFile(t *testing.T) {
	runner := &storageHostRunner{}

	if err := ConfigureStorageRestarts(runner); err != nil {
		t.Fatalf("ConfigureStorageRestarts: %v", err)
	}

	all := strings.Join(runner.commands, "\n")
	if !strings.Contains(all, "mv -f") {
		t.Error("the config is written straight to its final path, so an interrupted write leaves broken Perl")
	}
	tmp := needrestartDropIn + ".tmp"
	if !strings.Contains(all, tmp) {
		t.Errorf("no temporary file in the same directory; commands were:\n%s", all)
	}
}

// Running it twice must be the same as running it once. It runs at install, at
// node add, and from the repair command, and an operator has to be able to reach
// for it without wondering whether it is safe.
func TestConfigureStorageRestartsIsIdempotent(t *testing.T) {
	first := &storageHostRunner{}
	second := &storageHostRunner{}

	if err := ConfigureStorageRestarts(first); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := ConfigureStorageRestarts(second); err != nil {
		t.Fatalf("second run: %v", err)
	}

	if strings.Join(first.commands, "\n") != strings.Join(second.commands, "\n") {
		t.Error("the second run does something different from the first")
	}
	if first.stdin[0] != second.stdin[0] {
		t.Error("the config written differs between runs")
	}
}

// The stamp records that a write once succeeded on this host. It carries the
// machine identity as well as the version, because a node reimaged under the
// same name keeps its annotation and would otherwise report a false green.
func TestStampStorageRestartsRecordsVersionAndIdentity(t *testing.T) {
	runner := &storageHostRunner{replies: map[string]string{
		"machine-id": "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d",
	}}

	if err := StampStorageRestarts(runner, "worker-2", "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d"); err != nil {
		t.Fatalf("StampStorageRestarts: %v", err)
	}

	all := strings.Join(runner.commands, "\n")
	if !strings.Contains(all, "kubectl annotate node 'worker-2'") {
		t.Errorf("nothing annotated the node; commands were:\n%s", all)
	}
	if !strings.Contains(all, StorageRestartsVersionAnnotation+"="+StorageRestartConfigVersion) {
		t.Error("the stamp does not record which config version was applied")
	}
	if !strings.Contains(all, StorageRestartsMachineAnnotation+"='9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d'") {
		t.Error("the stamp does not record the machine it was applied to, so a reimage reads as covered")
	}
	if !strings.Contains(all, "--overwrite") {
		t.Error("a re-run must replace the stamp rather than fail on the existing one")
	}
}

// The identity comes from the host itself. Reading it from the Kubernetes node
// would make the stamp agree with the node object by construction and prove
// nothing about the machine that was actually written to.
func TestReadMachineIDAsksTheHost(t *testing.T) {
	runner := &storageHostRunner{replies: map[string]string{
		"/etc/machine-id": "  9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d \n",
	}}

	id, err := ReadMachineID(runner)
	if err != nil {
		t.Fatalf("ReadMachineID: %v", err)
	}
	if id != "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d" {
		t.Errorf("machine id = %q, want the trimmed value", id)
	}
}

// A host with no machine-id is not an error worth failing an install over, but
// it must not produce a stamp either: an empty identity matches every node and
// would read as covered forever.
func TestReadMachineIDRefusesAnEmptyAnswer(t *testing.T) {
	runner := &storageHostRunner{replies: map[string]string{"/etc/machine-id": "\n"}}

	if _, err := ReadMachineID(runner); err == nil {
		t.Error("an empty machine id must be an error, not a stamp that matches everything")
	}
}

// The node to stamp is found by the identity the machine published to the API,
// not by matching a hostname the operator typed. A worker's SSH address, its
// hostname and its Kubernetes node name are three different things that usually
// agree, and stamping the wrong node is worse than not stamping.
func TestResolveNodeByMachineID(t *testing.T) {
	runner := &storageHostRunner{replies: map[string]string{
		"kubectl get nodes": "worker-2\n",
	}}

	name, err := NodeNameForMachine(runner, "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d")
	if err != nil {
		t.Fatalf("NodeNameForMachine: %v", err)
	}
	if name != "worker-2" {
		t.Errorf("node = %q, want worker-2", name)
	}
	if !strings.Contains(strings.Join(runner.commands, "\n"), "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d") {
		t.Error("the lookup does not use the machine id")
	}
}

// No match means the node has not registered yet, or registered under an
// identity this machine does not have. Either way there is nothing safe to
// stamp.
func TestResolveNodeByMachineIDFindsNothing(t *testing.T) {
	runner := &storageHostRunner{replies: map[string]string{"kubectl get nodes": "\n"}}

	if _, err := NodeNameForMachine(runner, "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d"); err == nil {
		t.Error("an unmatched machine id must be an error rather than an empty node name")
	}
}

// Two nodes reporting one machine id means a cloned image, and picking either
// stamps a node that was never written to.
func TestResolveNodeByMachineIDRefusesTwoMatches(t *testing.T) {
	runner := &storageHostRunner{replies: map[string]string{"kubectl get nodes": "worker-2\nworker-3\n"}}

	_, err := NodeNameForMachine(runner, "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d")
	if err == nil {
		t.Fatal("two nodes sharing a machine id must be an error")
	}
	if !strings.Contains(err.Error(), "worker-2") || !strings.Contains(err.Error(), "worker-3") {
		t.Errorf("the error has to name both nodes, got %v", err)
	}
}

// Deferring those restarts trades an availability risk for a security one: the
// patched library stays unloaded until something restarts the daemon, and
// nothing on the node says so. kip status reads it back, so the deferral is
// visible rather than permanent.

func TestPendingRestartsReadsNeedrestart(t *testing.T) {
	out := `NEEDRESTART-VER: 3.5
NEEDRESTART-KCUR: 6.1.0-18-amd64
NEEDRESTART-KEXP: 6.1.0-21-amd64
NEEDRESTART-KSTA: 3
NEEDRESTART-SVC: iscsid.service
NEEDRESTART-SVC: cron.service
`
	runner := &storageHostRunner{replies: map[string]string{"needrestart": out}}

	pending, err := PendingRestarts(runner)
	if err != nil {
		t.Fatalf("PendingRestarts: %v", err)
	}

	if len(pending.Deferred) != 1 || pending.Deferred[0] != "iscsid.service" {
		t.Errorf("deferred = %v, want only the units Kipper excluded", pending.Deferred)
	}
	if len(pending.Other) != 1 || pending.Other[0] != "cron.service" {
		t.Errorf("other = %v, want the services needrestart would have restarted itself", pending.Other)
	}
	if !pending.KernelOutdated {
		t.Error("KSTA 3 means the running kernel is not the installed one")
	}
}

// A node that has caught up says nothing, and status stays quiet.
func TestPendingRestartsOnACleanHost(t *testing.T) {
	runner := &storageHostRunner{replies: map[string]string{"needrestart": "NEEDRESTART-VER: 3.5\nNEEDRESTART-KSTA: 1\n"}}

	pending, err := PendingRestarts(runner)
	if err != nil {
		t.Fatalf("PendingRestarts: %v", err)
	}
	if pending.Any() {
		t.Errorf("a clean host reported %+v", pending)
	}
}

// needrestart is Debian's, and Kipper runs on hosts without it. Reporting
// nothing pending would be a lie; the caller has to be able to tell "checked and
// clean" from "could not check".
func TestPendingRestartsWithoutNeedrestart(t *testing.T) {
	runner := &storageHostRunner{replies: map[string]string{"needrestart": "NEEDRESTART-ABSENT"}}

	_, err := PendingRestarts(runner)
	if err == nil {
		t.Error("a host without needrestart must be an error, not a clean bill of health")
	}
}

// A Longhorn volume that remounted read-only says so in /proc/mounts, and the
// control plane's mount namespace is one thing the status command can already
// reach. It is confirmation rather than the primary signal, because it sees one
// node's mounts and the cluster may have more.

func TestReadOnlyMountsFindsAPersistentVolume(t *testing.T) {
	mounts := `/dev/longhorn/pvc-4f2a /var/lib/kubelet/pods/9c1/volumes/kubernetes.io~csi/pvc-4f2a/mount ext4 ro,relatime 0 0
/dev/longhorn/pvc-88b /var/lib/kubelet/pods/1a2/volumes/kubernetes.io~csi/pvc-88b/mount ext4 rw,relatime 0 0
/dev/sda1 / ext4 rw,relatime 0 0
`
	runner := &storageHostRunner{replies: map[string]string{"/proc/mounts": mounts}}

	readOnly, err := ReadOnlyVolumeMounts(runner)
	if err != nil {
		t.Fatalf("ReadOnlyVolumeMounts: %v", err)
	}
	if len(readOnly) != 1 {
		t.Fatalf("found %v, want only the read-only volume", readOnly)
	}
	if !strings.Contains(readOnly[0], "pvc-4f2a") {
		t.Errorf("the finding has to name the volume, got %q", readOnly[0])
	}
}

// A read-only root filesystem, a squashfs, and every other ro mount on a normal
// host are not what this looks for. Reporting them would bury the one that
// matters in noise the operator has to learn to ignore.
func TestReadOnlyMountsIgnoresEverythingButVolumes(t *testing.T) {
	mounts := `/dev/loop0 /snap/core/1 squashfs ro,nodev 0 0
sysfs /sys sysfs rw,nosuid 0 0
/dev/sda1 /boot/efi vfat ro,relatime 0 0
`
	runner := &storageHostRunner{replies: map[string]string{"/proc/mounts": mounts}}

	readOnly, err := ReadOnlyVolumeMounts(runner)
	if err != nil {
		t.Fatalf("ReadOnlyVolumeMounts: %v", err)
	}
	if len(readOnly) != 0 {
		t.Errorf("reported %v; only persistent volume mounts belong here", readOnly)
	}
}

// "ro" as a whole option, never as a prefix. A "rootcontext=" or "rw" mount is
// not read-only, and matching loosely would report a healthy volume as broken.
func TestReadOnlyMountsMatchesTheOptionNotAPrefix(t *testing.T) {
	mounts := `/dev/longhorn/pvc-1 /var/lib/kubelet/pods/1/volumes/kubernetes.io~csi/pvc-1/mount ext4 rw,rootcontext=x 0 0
/dev/longhorn/pvc-2 /var/lib/kubelet/pods/2/volumes/kubernetes.io~csi/pvc-2/mount ext4 rw,errors=remount-ro 0 0
`
	runner := &storageHostRunner{replies: map[string]string{"/proc/mounts": mounts}}

	readOnly, err := ReadOnlyVolumeMounts(runner)
	if err != nil {
		t.Fatalf("ReadOnlyVolumeMounts: %v", err)
	}
	if len(readOnly) != 0 {
		t.Errorf("reported %v; both of those are read-write", readOnly)
	}
}

// The machine id comes from the worker, and every command built from it runs as
// root on the control plane. A worker that is compromised, or simply has a
// mangled /etc/machine-id, could otherwise close the shell quote and have the
// rest of its file run as a command on the node holding the cluster's keys.
//
// machine-id has one format: 32 lowercase hex characters. Anything else is not
// a machine id, and guessing what an operator meant is not worth a root shell.
func TestReadMachineIDRejectsAnythingButAMachineID(t *testing.T) {
	for _, bad := range []string{
		`x'; touch /root/pwned; #`,
		`9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d; rm -rf /`,
		"9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d\nrm -rf /",
		"9A3C1F2E4B5D6A7B8C9D0E1F2A3B4C5D",  // uppercase is not the format
		"9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c",    // too short
		"9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5dd", // too long
		"9a3c1f2e-4b5d-6a7b-8c9d-0e1f2a3b4c",
		"",
	} {
		runner := &storageHostRunner{replies: map[string]string{"/etc/machine-id": bad}}
		if id, err := ReadMachineID(runner); err == nil {
			t.Errorf("accepted %q as a machine id (got %q)", bad, id)
		}
	}
}

func TestReadMachineIDAcceptsTheRealFormat(t *testing.T) {
	runner := &storageHostRunner{replies: map[string]string{"/etc/machine-id": "  9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d \n"}}

	id, err := ReadMachineID(runner)
	if err != nil {
		t.Fatalf("ReadMachineID: %v", err)
	}
	if id != "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d" {
		t.Errorf("machine id = %q", id)
	}
}

// Validation is the guard, and quoting is the second one. Neither command may
// interpolate a value straight into the shell, whatever reached it.
func TestNodeCommandsQuoteTheirArguments(t *testing.T) {
	runner := &storageHostRunner{replies: map[string]string{"kubectl get nodes": "worker-2\n"}}
	if _, err := NodeNameForMachine(runner, "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d"); err != nil {
		t.Fatalf("NodeNameForMachine: %v", err)
	}
	if err := StampStorageRestarts(runner, "worker-2", "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d"); err != nil {
		t.Fatalf("StampStorageRestarts: %v", err)
	}

	for _, cmd := range runner.commands {
		if strings.Contains(cmd, "kubectl") && !strings.Contains(cmd, "'") {
			t.Errorf("a kubectl command built from cluster input carries no quoting: %s", cmd)
		}
	}
}

// A node name comes from the API rather than from the worker, but it is still
// interpolated into a root command, so it is validated too.
func TestStampRejectsAnImpossibleNodeName(t *testing.T) {
	runner := &storageHostRunner{}
	if err := StampStorageRestarts(runner, "worker-2; rm -rf /", "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d"); err == nil {
		t.Error("accepted a node name that is not a Kubernetes object name")
	}
	if len(runner.commands) != 0 {
		t.Errorf("ran %v before rejecting it", runner.commands)
	}
}
