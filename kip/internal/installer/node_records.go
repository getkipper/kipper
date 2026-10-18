package installer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"github.com/getkipper/kipper/kip/internal/config"
)

// NodeRecord is kip's record of one node in the kipper-nodes ConfigMap.
type NodeRecord = config.NodeRecord

const (
	nodeRecordsNamespace = "kipper-system"
	nodeRecordsConfigMap = "kipper-nodes"

	NodeRoleServer = "server"
	NodeRoleWorker = "worker"

	NodeStateJoining  = "joining"
	NodeStateReady    = "ready"
	NodeStateRemoving = "removing"
	NodeStateWiping   = "wiping"

	NetworkModePublic  = "public"
	NetworkModePrivate = "private"
)

// interfaceNamePattern permits dots for VLAN subinterfaces such as enp7s0.4000.
var interfaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,14}$`)

// hostnamePattern excludes shell metacharacters and leading hyphens that SSH
// could interpret as options.
var hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

// ValidNodeHost reports whether host can be recorded as a node's SSH address.
func ValidNodeHost(host string) bool {
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Zone() == ""
	}
	return hostnamePattern.MatchString(host)
}

var errInvalidNodeRecords = errors.New("the node records are invalid")

func validateNodeRecord(r NodeRecord) error {
	if !nodeNamePattern.MatchString(r.Name) {
		return fmt.Errorf("node record has invalid node name %q", r.Name)
	}
	if !machineIDPattern.MatchString(r.MachineID) {
		return fmt.Errorf("node record %s has invalid machine ID %q: expected 32 lowercase hexadecimal characters", r.Name, r.MachineID)
	}
	if !ValidNodeHost(r.Host) {
		return fmt.Errorf("node record %s has invalid SSH address %q: expected a hostname or IP address", r.Name, r.Host)
	}
	switch r.Role {
	case NodeRoleServer, NodeRoleWorker:
	default:
		return fmt.Errorf("node record %s has invalid role %q", r.Name, r.Role)
	}
	switch r.State {
	case NodeStateJoining, NodeStateReady, NodeStateRemoving, NodeStateWiping:
	default:
		return fmt.Errorf("node record %s has invalid state %q", r.Name, r.State)
	}
	switch r.Mode {
	case "", NetworkModePublic, NetworkModePrivate:
	default:
		return fmt.Errorf("node record %s has invalid network mode %q", r.Name, r.Mode)
	}
	if r.PeerAddress != "" {
		addr, err := netip.ParseAddr(r.PeerAddress)
		if err != nil || !addr.Is4() {
			return fmt.Errorf("node record %s has invalid peer address %q: expected an IPv4 address", r.Name, r.PeerAddress)
		}
	}
	if r.Interface != "" && !interfaceNamePattern.MatchString(r.Interface) {
		return fmt.Errorf("node record %s has invalid interface name %q", r.Name, r.Interface)
	}
	return nil
}

// ReadNodeRecords reads the kipper-nodes ConfigMap, server first, then by
// name. A missing ConfigMap reads as no records; an unreadable one is an error.
func ReadNodeRecords(client CommandRunner) ([]NodeRecord, error) {
	out, err := client.Run(fmt.Sprintf("kubectl -n %s get configmap %s --ignore-not-found -o jsonpath='{.data}' 2>/dev/null",
		nodeRecordsNamespace, nodeRecordsConfigMap))
	if err != nil {
		return nil, fmt.Errorf("reading the node records: %w", err)
	}
	return parseNodeRecords(out)
}

func parseNodeRecords(out string) ([]NodeRecord, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	var data map[string]string
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		return nil, fmt.Errorf("%w: %s/%s is not a map of records", errInvalidNodeRecords, nodeRecordsNamespace, nodeRecordsConfigMap)
	}
	records := make([]NodeRecord, 0, len(data))
	for key, value := range data {
		var r NodeRecord
		if err := json.Unmarshal([]byte(value), &r); err != nil {
			return nil, fmt.Errorf("%w: entry %s is not a record", errInvalidNodeRecords, key)
		}
		if err := validateNodeRecord(r); err != nil {
			return nil, fmt.Errorf("%w: %v", errInvalidNodeRecords, err)
		}
		if key != r.MachineID {
			return nil, fmt.Errorf("%w: entry %s holds the record for machine %s", errInvalidNodeRecords, key, r.MachineID)
		}
		records = append(records, r)
	}
	sortNodeRecords(records)
	return records, nil
}

func sortNodeRecords(records []NodeRecord) {
	sort.Slice(records, func(i, j int) bool {
		if (records[i].Role == NodeRoleServer) != (records[j].Role == NodeRoleServer) {
			return records[i].Role == NodeRoleServer
		}
		return records[i].Name < records[j].Name
	})
}

// SaveNodeRecord creates or replaces one node's record, keyed by machine id.
func SaveNodeRecord(client CommandRunner, r NodeRecord) error {
	if err := validateNodeRecord(r); err != nil {
		return err
	}
	value, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encoding the record for %s: %w", r.Name, err)
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"labels": map[string]string{"app.kubernetes.io/managed-by": "kipper"}},
		"data":     map[string]string{r.MachineID: string(value)},
	})
	if err != nil {
		return fmt.Errorf("encoding the record for %s: %w", r.Name, err)
	}
	// A concurrent run may create the ConfigMap first; the patch then applies to it.
	cmd := fmt.Sprintf("{ kubectl -n %[1]s create configmap %[2]s >/dev/null 2>&1 || true; } && kubectl -n %[1]s patch configmap %[2]s --type merge --patch-file /dev/stdin >/dev/null",
		nodeRecordsNamespace, nodeRecordsConfigMap)
	if _, err := client.RunStdin(cmd, strings.NewReader(string(patch))); err != nil {
		return fmt.Errorf("recording node %s: %w", r.Name, err)
	}
	return nil
}

// ServerRecord returns the server's record, if any.
func ServerRecord(records []NodeRecord) (NodeRecord, bool) {
	for _, r := range records {
		if r.Role == NodeRoleServer {
			return r, true
		}
	}
	return NodeRecord{}, false
}

// EnsureServerRecord verifies the connected server's machine ID against its
// record, creating the record if missing, and returns all records.
// host is the SSH address to store for a newly recorded server.
func EnsureServerRecord(client CommandRunner, host string) ([]NodeRecord, error) {
	records, err := ReadNodeRecords(client)
	if err != nil {
		return nil, err
	}
	machineID, err := ReadMachineID(client)
	if err != nil {
		return nil, fmt.Errorf("checking the server's identity: %w", err)
	}
	if server, ok := ServerRecord(records); ok {
		if server.MachineID != machineID {
			return nil, fmt.Errorf("%s is machine %s, but the cluster records its server as machine %s (%s); check the address, then run the command again", host, machineID, server.MachineID, server.Name)
		}
		return records, nil
	}
	name, err := NodeNameForMachine(client, machineID)
	if err != nil {
		return nil, fmt.Errorf("recording the server: %w", err)
	}
	server := NodeRecord{Name: name, Host: host, MachineID: machineID, Role: NodeRoleServer, State: NodeStateReady}
	if err := SaveNodeRecord(client, server); err != nil {
		return nil, err
	}
	records = append(records, server)
	sortNodeRecords(records)
	return records, nil
}

// UnrecordedNodes lists the cluster's nodes whose machine id has no record.
// Two nodes sharing a machine id, or a node whose name differs from its
// record's, are errors. A record with no name, or one still joining, matches
// by machine id alone.
func UnrecordedNodes(client CommandRunner, records []NodeRecord) ([]string, error) {
	out, err := listNodeIdentities(client)
	if err != nil {
		return nil, err
	}
	return unrecordedIn(out, records)
}

func listNodeIdentities(client CommandRunner) (string, error) {
	out, err := client.Run(`kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}|{.status.nodeInfo.machineID}{"\n"}{end}' 2>/dev/null`)
	if err != nil {
		return "", fmt.Errorf("listing nodes: %w", err)
	}
	return out, nil
}

func unrecordedIn(out string, records []NodeRecord) ([]string, error) {
	recorded := map[string]string{}
	for _, r := range records {
		// A joining record's name is provisional until the node registers.
		if r.State == NodeStateJoining {
			recorded[r.MachineID] = ""
			continue
		}
		recorded[r.MachineID] = r.Name
	}
	seen := map[string]string{}
	var unrecorded []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, machineID, _ := strings.Cut(line, "|")
		if !nodeNamePattern.MatchString(name) {
			return nil, fmt.Errorf("listing nodes: invalid node name in kubectl output %q", line)
		}
		if other, ok := seen[machineID]; ok && machineID != "" {
			return nil, fmt.Errorf("%w: nodes %s and %s share machine id %s", errSharedMachineID, other, name, machineID)
		}
		seen[machineID] = name
		recordedName, ok := recorded[machineID]
		switch {
		case !ok || machineID == "":
			unrecorded = append(unrecorded, name)
		case recordedName != "" && recordedName != name:
			return nil, fmt.Errorf("node %s runs machine %s, which kip recorded as node %s", name, machineID, recordedName)
		}
	}
	sort.Strings(unrecorded)
	return unrecorded, nil
}

// RequireRecordedNodes creates the server record if needed and returns the
// records once every cluster node matches a recorded identity.
func RequireRecordedNodes(client CommandRunner, host string) ([]NodeRecord, error) {
	records, err := EnsureServerRecord(client, host)
	if err != nil {
		return nil, err
	}
	unrecorded, err := UnrecordedNodes(client, records)
	if err != nil {
		return nil, err
	}
	if len(unrecorded) > 0 {
		return nil, unrecordedNodesError(unrecorded)
	}
	return records, nil
}

func unrecordedNodesError(unrecorded []string) error {
	return fmt.Errorf("unrecorded cluster nodes: %s; run 'kip node add --host <address>' for each to record it, then run this command again", strings.Join(unrecorded, ", "))
}

// RequirePeersRecordedForJoin requires records for all peers before a new node
// joins, since firewall rules and join checks use those records. Existing cluster
// nodes may proceed so adoption can run one node at a time or resume after interruption.
// Conflicting node identities always fail validation.
func RequirePeersRecordedForJoin(client CommandRunner, records []NodeRecord, machineID string) error {
	out, err := listNodeIdentities(client)
	if err != nil {
		return err
	}
	others, err := unrecordedIn(out, WithUnrecordedHost(records, machineID))
	if err != nil || len(others) == 0 {
		return err
	}
	var withoutTarget []NodeRecord
	for _, r := range records {
		if r.MachineID != machineID {
			withoutTarget = append(withoutTarget, r)
		}
	}
	// Reuse the same listing so a node registering between checks cannot change the result.
	// Removing the target's record adds an unrecorded node only if it has registered.
	all, err := unrecordedIn(out, withoutTarget)
	if err != nil {
		return err
	}
	if len(all) > len(others) {
		return nil
	}
	return unrecordedNodesError(others)
}

// WithUnrecordedHost adds an unnamed record so UnrecordedNodes accepts a host
// awaiting registration. Existing records retain their node name checks.
func WithUnrecordedHost(records []NodeRecord, machineID string) []NodeRecord {
	for _, r := range records {
		if r.MachineID == machineID {
			return records
		}
	}
	return append(append([]NodeRecord(nil), records...), NodeRecord{MachineID: machineID})
}

// ProposedNodeName uses the lowercase hostname as a provisional name.
// Registration determines the final name.
func ProposedNodeName(client CommandRunner) (string, error) {
	out, err := client.Run("hostname")
	if err != nil {
		return "", fmt.Errorf("reading the host name: %w", err)
	}
	name := strings.ToLower(strings.TrimSpace(out))
	if !nodeNamePattern.MatchString(name) {
		return "", fmt.Errorf("the host name %q is not a valid node name", name)
	}
	return name, nil
}
