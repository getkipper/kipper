package installer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// longhornNodeExists reports whether Longhorn has a node object for node. A
// cluster without Longhorn's node CRD has none.
func longhornNodeExists(master CommandRunner, node string) (bool, error) {
	crd, err := master.Run("kubectl get crd nodes.longhorn.io --ignore-not-found -o name")
	if err != nil {
		return false, fmt.Errorf("checking for Longhorn: %w", err)
	}
	if !listsObject(crd, "customresourcedefinition.apiextensions.k8s.io/nodes.longhorn.io") {
		return false, nil
	}
	out, err := master.Run(fmt.Sprintf("kubectl -n longhorn-system get nodes.longhorn.io %s --ignore-not-found -o name", shellQuote(node)))
	if err != nil {
		return false, fmt.Errorf("reading Longhorn's node %s: %w", node, err)
	}
	return listsObject(out, "node.longhorn.io/"+node), nil
}

// listsObject reports whether kubectl's -o name output names object on a
// line of its own, so warnings mixed into the output are ignored.
func listsObject(out, object string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == object {
			return true
		}
	}
	return false
}

// EvictLonghornNode stops Longhorn from scheduling copies on node and asks it
// to move the copies already there. It does nothing when Longhorn has no node
// object for node.
func EvictLonghornNode(master CommandRunner, node string) error {
	exists, err := longhornNodeExists(master, node)
	if err != nil || !exists {
		return err
	}
	cmd := fmt.Sprintf(`kubectl -n longhorn-system patch nodes.longhorn.io %s --type merge -p '{"spec":{"allowScheduling":false,"evictionRequested":true}}' >/dev/null`, shellQuote(node))
	if _, err := master.Run(cmd); err != nil {
		return fmt.Errorf("asking Longhorn to move volume copies off %s: %w", node, err)
	}
	return nil
}

// DeleteNodeObjects deletes node's Kubernetes Node and its Longhorn node. Call
// it after k3s is uninstalled from the host, or its kubelet registers the Node
// again. Missing objects are accepted. Longhorn deletion is requested without
// waiting for it to finish.
func DeleteNodeObjects(master CommandRunner, node string) error {
	if _, err := master.Run(fmt.Sprintf("kubectl delete node %s --ignore-not-found >/dev/null", shellQuote(node))); err != nil {
		return fmt.Errorf("deleting node %s: %w", node, err)
	}
	exists, err := longhornNodeExists(master, node)
	if err != nil || !exists {
		return err
	}
	if _, err := master.Run(fmt.Sprintf("kubectl -n longhorn-system delete nodes.longhorn.io %s --ignore-not-found --wait=false >/dev/null", shellQuote(node))); err != nil {
		return fmt.Errorf("deleting Longhorn's node %s: %w", node, err)
	}
	return nil
}

// NodeRegistered reports whether the cluster has a Node named node. A missing
// Node returns false and nil. Lookup failures return an error.
func NodeRegistered(master CommandRunner, node string) (bool, error) {
	out, err := master.Run(fmt.Sprintf("kubectl get node %s --ignore-not-found -o name", shellQuote(node)))
	if err != nil {
		return false, fmt.Errorf("looking up node %s: %w", node, err)
	}
	return listsObject(out, "node/"+node), nil
}

// RemovalStarted reports whether state is removing or wiping. A wiping
// worker must finish removal before it can be added again.
func RemovalStarted(state string) bool {
	return state == NodeStateRemoving || state == NodeStateWiping
}

// RemovalInProgress returns the first record in input order being removed,
// excluding machineID. It returns a zero record and false if none matches.
func RemovalInProgress(records []NodeRecord, machineID string) (NodeRecord, bool) {
	for _, r := range records {
		if RemovalStarted(r.State) && r.MachineID != machineID {
			return r, true
		}
	}
	return NodeRecord{}, false
}

// JoinInProgress returns the first record in input order still joining,
// excluding machineID. It returns a zero record and false if none matches.
func JoinInProgress(records []NodeRecord, machineID string) (NodeRecord, bool) {
	return otherInState(records, machineID, NodeStateJoining)
}

func otherInState(records []NodeRecord, machineID, state string) (NodeRecord, bool) {
	for _, r := range records {
		if r.State == state && r.MachineID != machineID {
			return r, true
		}
	}
	return NodeRecord{}, false
}

// VolumesOnlyOn lists the Longhorn volumes that have a replica on node, in any
// state, and no healthy replica on another node. It returns sorted names or nil
// if none match. A healthy replica has healthyAt set and failedAt empty. Once
// Longhorn marks a deleted node's replicas failed, the host may still hold the
// volumes' only data, so failed replicas also matter here.
func VolumesOnlyOn(master CommandRunner, node string) ([]string, error) {
	out, err := master.Run(replicaQuery)
	if err != nil {
		return nil, fmt.Errorf("listing Longhorn replicas: %w", err)
	}
	return volumesOnlyOn(out, node), nil
}

func volumesOnlyOn(replicas, node string) []string {
	here := map[string]bool{}
	healthyElsewhere := map[string]bool{}
	for _, line := range strings.Split(replicas, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 || f[0] == "" {
			continue
		}
		if f[1] == node {
			here[f[0]] = true
		} else if f[2] != "" && f[3] == "" {
			healthyElsewhere[f[0]] = true
		}
	}
	var only []string
	for v := range here {
		if !healthyElsewhere[v] {
			only = append(only, v)
		}
	}
	sort.Strings(only)
	return only
}

// DeleteNodeRecord removes the record with machineID from the cluster's node
// records and verifies its removal. An absent record or ConfigMap succeeds.
// An invalid machine ID or a failure to read, patch or verify the records
// returns an error.
func DeleteNodeRecord(client CommandRunner, machineID string) error {
	if !machineIDPattern.MatchString(machineID) {
		return fmt.Errorf("invalid machine ID %q", machineID)
	}
	patch, err := json.Marshal(map[string]any{"data": map[string]any{machineID: nil}})
	if err != nil {
		return err
	}
	// A missing ConfigMap allows a retry to succeed. A failed read must stop removal.
	cmd := fmt.Sprintf(`found=$(kubectl -n %[1]s get configmap %[2]s --ignore-not-found -o name 2>/dev/null) || exit 1; [ "$found" = configmap/%[2]s ] || exit 0; kubectl -n %[1]s patch configmap %[2]s --type merge --patch-file /dev/stdin >/dev/null`,
		NodeRecordsNamespace, NodeRecordsConfigMap)
	if _, err := client.RunStdin(cmd, strings.NewReader(string(patch))); err != nil {
		return fmt.Errorf("removing the record of %s: %w", machineID, err)
	}
	records, err := ReadNodeRecords(client)
	if err != nil {
		return fmt.Errorf("checking that the record of %s is gone: %w", machineID, err)
	}
	for _, r := range records {
		if r.MachineID == machineID {
			return fmt.Errorf("the record of %s is still there after removing it", machineID)
		}
	}
	return nil
}
