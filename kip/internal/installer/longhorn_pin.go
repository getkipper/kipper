package installer

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ServerStorageTag selects the server's Longhorn node for replica scheduling.
const ServerStorageTag = "kipper-server"

const singleCopyClass = "longhorn-single"

var singleCopyClassManifest = fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
provisioner: driver.longhorn.io
allowVolumeExpansion: true
reclaimPolicy: Delete
volumeBindingMode: Immediate
parameters:
  numberOfReplicas: "1"
  staleReplicaTimeout: "30"
  fsType: ext4
  dataEngine: v1
  nodeSelector: "%s"
`, singleCopyClass, ServerStorageTag)

// serverStorageWait allows a fresh install time for node registration, disk
// synchronization and manifest retries.
var serverStorageWait = joinTimeouts{ready: 5 * time.Minute, interval: 5 * time.Second}

// pinWait uses shorter retry windows for existing clusters so storage problems
// delay upgrades less than fresh installations.
var pinWait = joinTimeouts{ready: 30 * time.Second, interval: 5 * time.Second}

// OffServerCopy identifies a healthy replica of a single-copy volume on a worker.
type OffServerCopy struct {
	Volume string
	Node   string
	Pinned bool
}

// SingleCopyPlacement reports volume selector changes, patch failures with reasons,
// and healthy replicas still on workers. Volumes use namespace/claim labels
// when available, otherwise Longhorn volume names.
type SingleCopyPlacement struct {
	Pinned    []string
	Unpinned  []string
	OffServer []OffServerCopy
}

// PinSingleCopiesToServer configures server-only scheduling for the single-copy
// StorageClass and its existing volumes. Existing replicas can remain on workers.
// The result lists updated volumes, patch failures with reasons, and healthy
// replicas still on workers. Individual patch failures allow the run to continue.
// Single-node clusters update only the server tag and StorageClass and return an
// empty result. Other errors stop the run and may leave partial changes applied.
func PinSingleCopiesToServer(client CommandRunner) (SingleCopyPlacement, error) {
	return pinSingleCopies(client, false)
}

// PinSingleCopiesForJoin configures server-only scheduling before a worker joins,
// including existing volumes in single-node clusters. It returns placement details
// as described by PinSingleCopiesToServer and an error if any volume patch fails.
// Errors may leave partial changes applied. Existing replicas can remain on workers.
func PinSingleCopiesForJoin(client CommandRunner) (SingleCopyPlacement, error) {
	placement, err := pinSingleCopies(client, true)
	if err == nil && len(placement.Unpinned) > 0 {
		err = fmt.Errorf("existing single-copy volumes could not be pinned to the server: %s. Fix the reported errors, then run kip node add again", strings.Join(placement.Unpinned, "; "))
	}
	return placement, err
}

func pinSingleCopies(client CommandRunner, joining bool) (SingleCopyPlacement, error) {
	server, err := tagServerStorage(client, pinWait)
	if err != nil {
		return SingleCopyPlacement{}, err
	}
	if err := ensureSingleCopyClass(client, pinWait); err != nil {
		return SingleCopyPlacement{}, err
	}
	// A single-node cluster needs volume selectors before a worker joins
	// to keep future replicas on the server.
	if !joining {
		if nodes, err := NodeCount(client); err != nil {
			return SingleCopyPlacement{}, err
		} else if nodes < 2 {
			return SingleCopyPlacement{}, nil
		}
	}
	volumes, err := singleCopyVolumes(client)
	if err != nil {
		return SingleCopyPlacement{}, err
	}
	var placement SingleCopyPlacement
	for i, v := range volumes {
		if v.pinned() {
			continue
		}
		// Replace other selectors because they may exclude the server.
		patch := fmt.Sprintf(`{"spec":{"nodeSelector":%s}}`, jsonList([]string{ServerStorageTag}))
		if _, err := client.Run(fmt.Sprintf("kubectl -n longhorn-system patch volumes.longhorn.io %s --request-timeout=30s --type merge -p '%s' >/dev/null", v.name, patch)); err != nil {
			placement.Unpinned = append(placement.Unpinned, fmt.Sprintf("%s: %v", v.label(), err))
			continue
		}
		volumes[i].selector = []string{ServerStorageTag}
		placement.Pinned = append(placement.Pinned, v.label())
	}
	if placement.OffServer, err = offServerCopies(client, volumes, server); err != nil {
		return placement, err
	}
	return placement, nil
}

// OffServerNotice returns migration guidance for replicas on workers,
// or an empty string when there are none.
func OffServerNotice(copies []OffServerCopy) string {
	var pinned, unpinned []OffServerCopy
	for _, c := range copies {
		if c.Pinned {
			pinned = append(pinned, c)
		} else {
			unpinned = append(unpinned, c)
		}
	}
	var notices []string
	if len(pinned) > 0 {
		notices = append(notices, fmt.Sprintf("Single-copy volumes off the server: %s. 'kip node reboot %s' moves the copy to the server when the server has room for it; otherwise the reboot refuses: the drain blocks until --drain-timeout, then fails, and kip cancels the reboot.", listCopies(pinned), rebootTarget(pinned)))
	}
	if len(unpinned) > 0 {
		notices = append(notices, fmt.Sprintf("Single-copy volumes not pinned to the server: %s. Permanent loss of a listed worker can lose its volume data. A reboot may move these copies to another worker. Run 'kip upgrade' to pin them.", listCopies(unpinned)))
	}
	return strings.Join(notices, " ")
}

func listCopies(copies []OffServerCopy) string {
	var listed []string
	for _, c := range copies {
		listed = append(listed, fmt.Sprintf("%s (on %s)", c.Volume, c.Node))
	}
	return strings.Join(listed, ", ")
}

func rebootTarget(copies []OffServerCopy) string {
	for _, c := range copies {
		if c.Node != copies[0].Node {
			return "<worker>"
		}
	}
	return copies[0].Node
}

// SingleCopiesOffServer lists healthy replicas of longhorn-single volumes on
// workers, ordered by node name within each volume. It returns nil when none
// are found and an error if the server or storage state cannot be read.
func SingleCopiesOffServer(client CommandRunner) ([]OffServerCopy, error) {
	server, err := serverNodeName(client)
	if err != nil {
		return nil, err
	}
	volumes, err := singleCopyVolumes(client)
	if err != nil {
		return nil, err
	}
	return offServerCopies(client, volumes, server)
}

func serverNodeName(client CommandRunner) (string, error) {
	out, err := client.Run("kubectl get nodes -l " + ServerNodeLabel + " --request-timeout=30s -o name")
	if err != nil {
		return "", fmt.Errorf("finding the server node: %w", err)
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "node/"); ok && name != "" {
			names = append(names, name)
		}
	}
	if len(names) != 1 {
		return "", fmt.Errorf("expected one server node labeled %s, found %d", ServerNodeLabel, len(names))
	}
	return names[0], nil
}

func tagServerStorage(client CommandRunner, wait joinTimeouts) (string, error) {
	server, err := serverNodeName(client)
	if err != nil {
		return "", err
	}
	var tags []string
	err = pollUntil(wait.ready, wait.interval, func() (bool, error) {
		out, err := client.Run(fmt.Sprintf("kubectl -n longhorn-system get nodes.longhorn.io %s --request-timeout=30s -o jsonpath='{.spec.tags}'", server))
		if err != nil {
			return false, err
		}
		tags, err = parseJSONList(out)
		return err == nil, err
	})
	if err != nil {
		return "", fmt.Errorf("reading the Longhorn tags of %s: %w", server, err)
	}
	if slices.Contains(tags, ServerStorageTag) {
		return server, nil
	}
	patch := fmt.Sprintf(`{"spec":{"tags":%s}}`, jsonList(append(tags, ServerStorageTag)))
	// Longhorn's webhook refuses node changes while a new node's disks sync.
	err = pollUntil(wait.ready, wait.interval, func() (bool, error) {
		_, err := client.Run(fmt.Sprintf("kubectl -n longhorn-system patch nodes.longhorn.io %s --request-timeout=30s --type merge -p '%s' >/dev/null", server, patch))
		return err == nil, err
	})
	if err != nil {
		return "", fmt.Errorf("tagging the server's Longhorn node %s: %w", server, err)
	}
	return server, nil
}

// ensureSingleCopyClass recreates the class to change its immutable parameters.
// Existing volumes survive replacement. If creation fails, it attempts to
// restore the previous class so new claims can bind.
func ensureSingleCopyClass(client CommandRunner, wait joinTimeouts) error {
	out, err := client.Run(fmt.Sprintf("kubectl get storageclass %s --ignore-not-found --request-timeout=30s -o json", singleCopyClass))
	if err != nil {
		return fmt.Errorf("reading the %s StorageClass: %w", singleCopyClass, err)
	}
	var previous string
	if strings.TrimSpace(out) != "" {
		pinned, restore, err := readSingleCopyClass(out)
		if err != nil {
			return fmt.Errorf("reading the %s StorageClass: %w", singleCopyClass, err)
		}
		if pinned {
			return nil
		}
		if _, err := client.Run(fmt.Sprintf("kubectl delete storageclass %s --request-timeout=30s >/dev/null", singleCopyClass)); err != nil {
			return fmt.Errorf("replacing the %s StorageClass: %w", singleCopyClass, err)
		}
		previous = restore
	}
	// Retry creation here because new claims need the class to bind.
	err = applyManifest(client, singleCopyClassManifest, wait)
	if err == nil {
		return nil
	}
	if previous != "" && applyManifest(client, previous, wait) == nil {
		return fmt.Errorf("replacing the %s StorageClass: %w. The previous class was restored, so new volumes are not pinned to the server; run 'kip upgrade' again once the cause is fixed", singleCopyClass, err)
	}
	return fmt.Errorf("creating the %s StorageClass: %w. Until it exists, new databases, registry and backup volumes stay Pending; run 'kip upgrade' again to create it", singleCopyClass, err)
}

func applyManifest(client CommandRunner, manifest string, wait joinTimeouts) error {
	apply := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply --request-timeout=30s -f -\n%s\nKIPEOF", strings.TrimRight(manifest, "\n"))
	return pollUntil(wait.ready, wait.interval, func() (bool, error) {
		_, err := client.Run(apply)
		return err == nil, err
	})
}

// readSingleCopyClass checks the server selector and prepares an unpinned
// class for restoration by removing server-assigned metadata.
func readSingleCopyClass(raw string) (bool, string, error) {
	var class map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &class); err != nil {
		return false, "", err
	}
	params, _ := class["parameters"].(map[string]interface{})
	if params["nodeSelector"] == ServerStorageTag {
		return true, "", nil
	}
	if meta, ok := class["metadata"].(map[string]interface{}); ok {
		for _, field := range []string{"uid", "resourceVersion", "creationTimestamp", "managedFields", "generation"} {
			delete(meta, field)
		}
	}
	restore, err := json.Marshal(class)
	return false, string(restore), err
}

type singleCopyVolume struct {
	name, claim string
	selector    []string
}

func (v singleCopyVolume) pinned() bool {
	return len(v.selector) == 1 && v.selector[0] == ServerStorageTag
}

func (v singleCopyVolume) label() string {
	if v.claim != "" {
		return v.claim
	}
	return v.name
}

// singleCopyVolumes matches PV volume handles to Longhorn volumes
// to identify single-copy storage.
func singleCopyVolumes(client CommandRunner) ([]singleCopyVolume, error) {
	pvs, err := client.Run(`kubectl get pv --request-timeout=30s -o jsonpath='{range .items[*]}{.spec.storageClassName}{"\t"}{.spec.csi.volumeHandle}{"\n"}{end}'`)
	if err != nil {
		return nil, fmt.Errorf("listing persistent volumes: %w", err)
	}
	single := map[string]bool{}
	for _, line := range strings.Split(pvs, "\n") {
		if class, handle, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok && class == singleCopyClass && handle != "" {
			single[handle] = true
		}
	}
	out, err := client.Run(`kubectl -n longhorn-system get volumes.longhorn.io --request-timeout=30s -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.nodeSelector}{"\t"}{.status.kubernetesStatus.namespace}{"\t"}{.status.kubernetesStatus.pvcName}{"\n"}{end}'`)
	if err != nil {
		return nil, fmt.Errorf("listing Longhorn volumes: %w", err)
	}
	var volumes []singleCopyVolume
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 || !single[f[0]] {
			continue
		}
		selector, err := parseJSONList(f[1])
		if err != nil {
			return nil, fmt.Errorf("volume %s has an unreadable node selector %q: %w", f[0], f[1], err)
		}
		v := singleCopyVolume{name: f[0], selector: selector}
		if f[2] != "" && f[3] != "" {
			v.claim = f[2] + "/" + f[3]
		}
		volumes = append(volumes, v)
	}
	return volumes, nil
}

func offServerCopies(client CommandRunner, volumes []singleCopyVolume, server string) ([]OffServerCopy, error) {
	if len(volumes) == 0 {
		return nil, nil
	}
	holders, err := HealthyReplicaNodes(client)
	if err != nil {
		return nil, err
	}
	var off []OffServerCopy
	for _, v := range volumes {
		var nodes []string
		for node := range holders[v.name] {
			if node != server {
				nodes = append(nodes, node)
			}
		}
		slices.Sort(nodes)
		for _, node := range nodes {
			off = append(off, OffServerCopy{Volume: v.label(), Node: node, Pinned: v.pinned()})
		}
	}
	return off, nil
}

// parseJSONList reads a jsonpath rendering of a string list, which is empty
// or "null" when the field is unset.
func parseJSONList(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal([]byte(s), &list); err != nil {
		return nil, err
	}
	return list, nil
}

func jsonList(list []string) string {
	b, _ := json.Marshal(list)
	return string(b)
}
