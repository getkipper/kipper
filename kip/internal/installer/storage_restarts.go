package installer

import (
	"fmt"
	"strings"
)

// needrestartDropIn is where the storage-path exclusions live. The 50- prefix
// puts it after the distribution's own defaults, so it overrides them.
const needrestartDropIn = "/etc/needrestart/conf.d/50-kipper-storage.conf"

// StorageRestartConfigVersion identifies what a node was last configured with.
// It is stamped on the Kubernetes node so an operator can find nodes still
// running an older set without SSHing to each one, and it changes whenever the
// content below changes.
const StorageRestartConfigVersion = "1"

// storageRestartUnits are the daemons whose restart takes a Longhorn volume
// down with it.
//
// iscsid holds every volume's session, and losing it fails the block device
// under a mounted filesystem, which ext4 answers by remounting read-only. The
// two systemd units are here because Longhorn's data path runs over the node
// network: restarting either mid-session produces the same lost sessions by a
// slower route.
var storageRestartUnits = []string{
	"iscsid",
	"systemd-networkd",
	"systemd-resolved",
}

// storageRestartConfig is the needrestart drop-in.
//
// needrestart parses its configuration as Perl, so each override is keyed by a
// quoted regex. A bare service name parses as a plain string key, never matches
// a unit, and leaves needrestart restarting everything while the file suggests
// it was configured.
func storageRestartConfig() string {
	var b strings.Builder
	b.WriteString("# Managed by Kipper. Restarting these takes every Longhorn volume\n")
	b.WriteString("# on this node down with it: iscsid holds the sessions, and the\n")
	b.WriteString("# block device failing under a mounted filesystem remounts it\n")
	b.WriteString("# read-only. Patched libraries stay unloaded until the next\n")
	b.WriteString("# reboot; kip status reports when a node has restarts pending.\n")
	for _, unit := range storageRestartUnits {
		fmt.Fprintf(&b, "$nrconf{override_rc}{qr(^%s\\.service$)} = 0;\n", unit)
	}
	return b.String()
}

// ConfigureStorageRestarts installs the drop-in on a host.
//
// It runs at install, at node add, and from the repair command, so it has to be
// safe to run twice. The write lands under a temporary name in the same
// directory and is moved into place: a connection lost mid-write leaves the
// previous file rather than half a line of Perl, which needrestart cannot parse
// and answers by restarting everything.
func ConfigureStorageRestarts(runner commandRunner) error {
	dir := needrestartDropIn[:strings.LastIndex(needrestartDropIn, "/")]

	// The directory is absent on a node where needrestart is not installed, and
	// the file has to be there before it ever is.
	if _, err := runner.Run(fmt.Sprintf("mkdir -p %s", dir)); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	tmp := needrestartDropIn + ".tmp"
	write := fmt.Sprintf("umask 022 && cat > %s && chmod 0644 %s && mv -f %s %s",
		tmp, tmp, tmp, needrestartDropIn)
	if _, err := runner.RunStdin(write, strings.NewReader(storageRestartConfig())); err != nil {
		return fmt.Errorf("writing %s: %w", needrestartDropIn, err)
	}

	return nil
}

// The stamp says what a node was last configured with, so `kip status` can name
// the nodes still running an older set without SSHing to each one.
//
// It is a record that a write once succeeded, not a live audit: a file deleted
// or edited by hand afterwards leaves the stamp saying the node is covered. The
// docs say so, because reading it as an audit is the way it misleads.
const (
	storageRestartsVersionAnnotation = "kipper.run/storage-restarts-version"
	storageRestartsMachineAnnotation = "kipper.run/storage-restarts-machine"
)

// ReadMachineID reads the host's own identity.
//
// It asks the host rather than the Kubernetes node object, because the point of
// stamping it is to notice a node reimaged under the same name. An identity
// read from the object it is compared against would always agree.
func ReadMachineID(runner commandRunner) (string, error) {
	out, err := runner.Run("cat /etc/machine-id")
	if err != nil {
		return "", fmt.Errorf("reading machine id: %w", err)
	}
	id := strings.TrimSpace(out)
	if id == "" {
		// An empty identity would match every node and read as covered forever.
		return "", fmt.Errorf("host reported an empty machine id")
	}
	return id, nil
}

// StampStorageRestarts records on the Kubernetes node what was written to the
// host, so the two can be compared later without another SSH connection.
//
// Run it on the control plane, after the write it describes has succeeded.
func StampStorageRestarts(runner commandRunner, nodeName, machineID string) error {
	if nodeName == "" || machineID == "" {
		return fmt.Errorf("stamping storage restarts needs both a node name and a machine id")
	}

	cmd := fmt.Sprintf("kubectl annotate node %s %s=%s %s=%s --overwrite",
		nodeName,
		storageRestartsVersionAnnotation, StorageRestartConfigVersion,
		storageRestartsMachineAnnotation, machineID)
	if _, err := runner.Run(cmd); err != nil {
		return fmt.Errorf("stamping node %s: %w", nodeName, err)
	}
	return nil
}

// NodeNameForMachine finds the Kubernetes node a host registered as, by the
// identity the kubelet published from that host.
//
// A worker's SSH address, its hostname and its node name are three things that
// usually agree and occasionally do not, so the match is made on the one value
// that came from the machine itself. Run it on the control plane.
func NodeNameForMachine(runner commandRunner, machineID string) (string, error) {
	if machineID == "" {
		return "", fmt.Errorf("looking up a node needs a machine id")
	}

	cmd := fmt.Sprintf(
		`kubectl get nodes -o jsonpath='{range .items[?(@.status.nodeInfo.machineID==%q)]}{.metadata.name}{"\n"}{end}'`,
		machineID)
	out, err := runner.Run(cmd)
	if err != nil {
		return "", fmt.Errorf("looking for the node with machine id %s: %w", machineID, err)
	}

	var names []string
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}

	switch len(names) {
	case 0:
		return "", fmt.Errorf("no node reports machine id %s", machineID)
	case 1:
		return names[0], nil
	default:
		// A cloned image gives two machines one identity, and stamping either
		// would record a write that never happened on the other.
		return "", fmt.Errorf("nodes %s share machine id %s", strings.Join(names, " and "), machineID)
	}
}

// RestartsPending is what a host still owes: daemons holding an old copy of a
// patched library, and a kernel that has been replaced on disk but not booted.
type RestartsPending struct {
	// Deferred are units Kipper told needrestart to leave alone. Their patched
	// libraries stay unloaded until the node is rebooted or the unit is
	// restarted at a moment the operator chooses.
	Deferred []string
	// Other are units needrestart would have restarted on its own.
	Other []string
	// KernelOutdated is true when the installed kernel is not the running one.
	KernelOutdated bool
}

// Any reports whether the host owes anything at all.
func (p RestartsPending) Any() bool {
	return len(p.Deferred) > 0 || len(p.Other) > 0 || p.KernelOutdated
}

// PendingRestarts asks the host what still needs restarting.
//
// It fails rather than reporting nothing when needrestart is absent, because
// "nothing pending" and "could not check" are different answers and only one of
// them is reassuring.
func PendingRestarts(runner commandRunner) (RestartsPending, error) {
	out, err := runner.Run("command -v needrestart >/dev/null 2>&1 && needrestart -b || echo NEEDRESTART-ABSENT")
	if err != nil {
		return RestartsPending{}, fmt.Errorf("reading pending restarts: %w", err)
	}
	if strings.Contains(out, "NEEDRESTART-ABSENT") {
		return RestartsPending{}, fmt.Errorf("needrestart is not installed on this host")
	}

	deferred := map[string]bool{}
	for _, unit := range storageRestartUnits {
		deferred[unit+".service"] = true
	}

	var pending RestartsPending
	for _, line := range strings.Split(out, "\n") {
		field, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch field {
		case "NEEDRESTART-SVC":
			if deferred[value] {
				pending.Deferred = append(pending.Deferred, value)
			} else {
				pending.Other = append(pending.Other, value)
			}
		case "NEEDRESTART-KSTA":
			// 1 is the running kernel being the expected one. 2 and 3 are an
			// ABI or version upgrade waiting on a reboot; 0 is unknown, which
			// is not a claim that anything is pending.
			pending.KernelOutdated = value == "2" || value == "3"
		}
	}
	return pending, nil
}
