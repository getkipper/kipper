package installer

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

// ServerStorageTag selects the server's Longhorn node for replica scheduling.
const ServerStorageTag = platform.ServerStorageTag

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

// replicatedClassParameters are the parameters of the class for volumes with
// two copies. Locality and auto-balance are off so moving a pod leaves its copies
// in place. The revision counter lets the engine reject copies that missed writes
// at startup, except during a live engine upgrade. It must be enabled explicitly
// because Longhorn's CSI driver disables it when the class omits the setting.
var replicatedClassParameters = map[string]string{
	"numberOfReplicas":       "2",
	"dataLocality":           "disabled",
	"nodeSelector":           platform.ReplicaStorageTag,
	"replicaAutoBalance":     "disabled",
	"staleReplicaTimeout":    "30",
	"fsType":                 "ext4",
	"dataEngine":             "v1",
	"disableRevisionCounter": "false",
}

var replicatedClassManifest = func() string {
	keys := make([]string, 0, len(replicatedClassParameters))
	for k := range replicatedClassParameters {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var params strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&params, "  %s: %q\n", k, replicatedClassParameters[k])
	}
	return fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
provisioner: driver.longhorn.io
allowVolumeExpansion: true
reclaimPolicy: Delete
volumeBindingMode: Immediate
parameters:
%s`, platform.LonghornReplicatedClass, params.String())
}()

// serverStorageWait allows a fresh install time for node registration, disk
// synchronization and manifest retries.
var serverStorageWait = joinTimeouts{ready: 5 * time.Minute, interval: 5 * time.Second}

// pinWait uses shorter retry windows for existing clusters so storage problems
// delay upgrades less than fresh installations.
var pinWait = joinTimeouts{ready: 30 * time.Second, interval: 5 * time.Second}

// OffServerCopy identifies a current replica of a single-copy volume on a worker,
// including workers that are unavailable.
type OffServerCopy struct {
	Volume string
	Node   string
	Pinned bool
}

// SingleCopyPlacement reports volume selector changes, patch failures with reasons,
// and current replicas still on workers. Volumes use namespace/claim labels
// when available, otherwise Longhorn volume names.
type SingleCopyPlacement struct {
	Pinned    []string
	Unpinned  []string
	OffServer []OffServerCopy
}

// PinSingleCopiesToServer configures server-only scheduling for the single-copy
// StorageClass and its existing volumes. It also creates or updates the replicated
// StorageClass for new two-copy volumes. Existing replicas can remain on workers.
// The result lists updated volumes, patch failures with reasons, and current
// replicas still on workers. Individual patch failures allow the run to continue.
// Single-node clusters update only the server tags and StorageClasses and return an
// empty result. Other errors stop the run and may leave partial changes applied.
func PinSingleCopiesToServer(client CommandRunner) (SingleCopyPlacement, error) {
	return pinSingleCopies(client, false)
}

// PinSingleCopiesForJoin configures server-only scheduling before a worker joins,
// including existing volumes in single-node clusters. It also creates or updates
// the replicated StorageClass for new two-copy volumes. It returns placement
// details as described by PinSingleCopiesToServer and an error if any volume patch fails.
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
	if err := ensureStorageClasses(client, pinWait); err != nil {
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
		pinned, err := pinVolume(client, v)
		if err != nil {
			placement.Unpinned = append(placement.Unpinned, fmt.Sprintf("%s: %v", v.label(), err))
			continue
		}
		if pinned {
			volumes[i].selector = []string{ServerStorageTag}
			placement.Pinned = append(placement.Pinned, v.label())
		}
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
	names, err := ServerNodeNames(client)
	if err != nil {
		return "", fmt.Errorf("finding the server node: %w", err)
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
	if err := setLonghornNodeTags(client, server, []string{ServerStorageTag, platform.ReplicaStorageTag}, nil, wait); err != nil {
		return "", err
	}
	return server, nil
}

// ensureStorageClasses creates or recreates the single-copy and replicated
// classes, as ensureClass describes.
func ensureStorageClasses(client CommandRunner, wait joinTimeouts) error {
	if err := ensureClass(client, singleCopyClass, singleCopyClassManifest, singleCopyClassCurrent, wait); err != nil {
		return err
	}
	return ensureClass(client, platform.LonghornReplicatedClass, replicatedClassManifest, replicatedClassCurrent, wait)
}

// singleCopyClassCurrent checks the parameters that make the class one copy
// on the server.
func singleCopyClassCurrent(params map[string]interface{}) bool {
	return params["nodeSelector"] == ServerStorageTag && params["numberOfReplicas"] == "1"
}

func replicatedClassCurrent(params map[string]interface{}) bool {
	for k, v := range replicatedClassParameters {
		if params[k] != v {
			return false
		}
	}
	return true
}

// ensureClass recreates the class to change its immutable parameters when
// current rejects them. Existing volumes survive replacement. If creation
// fails, it attempts to restore the previous class so new claims can bind.
func ensureClass(client CommandRunner, name, manifest string, current func(map[string]interface{}) bool, wait joinTimeouts) error {
	out, err := client.Run(fmt.Sprintf("kubectl get storageclass %s --ignore-not-found --request-timeout=30s -o json", name))
	if err != nil {
		return fmt.Errorf("reading the %s StorageClass: %w", name, err)
	}
	var previous string
	if strings.TrimSpace(out) != "" {
		ok, restore, err := readClass(out, current)
		if err != nil {
			return fmt.Errorf("reading the %s StorageClass: %w", name, err)
		}
		if ok {
			return nil
		}
		if _, err := client.Run(fmt.Sprintf("kubectl delete storageclass %s --request-timeout=30s >/dev/null", name)); err != nil {
			return fmt.Errorf("replacing the %s StorageClass: %w", name, err)
		}
		previous = restore
	}
	// Retry creation here because new claims need the class to bind.
	err = applyManifest(client, manifest, wait)
	if err == nil {
		return nil
	}
	if previous != "" && applyManifest(client, previous, wait) == nil {
		return fmt.Errorf("replacing the %s StorageClass: %w. The previous class was restored, so new volumes still use its old parameters. Run 'kip upgrade' again once the cause is fixed", name, err)
	}
	return fmt.Errorf("creating the %s StorageClass: %w. Until it exists, new volumes using it stay Pending. Run 'kip upgrade' again to create it", name, err)
}

func applyManifest(client CommandRunner, manifest string, wait joinTimeouts) error {
	apply := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply --request-timeout=30s -f -\n%s\nKIPEOF", strings.TrimRight(manifest, "\n"))
	return pollUntil(wait.ready, wait.interval, func() (bool, error) {
		_, err := client.Run(apply)
		return err == nil, err
	})
}

// readClass checks the class's parameters with current and prepares a class
// it rejects for restoration by removing server-assigned metadata.
func readClass(raw string, current func(map[string]interface{}) bool) (bool, string, error) {
	var class map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &class); err != nil {
		return false, "", err
	}
	params, _ := class["parameters"].(map[string]interface{})
	if current(params) {
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
	name, claim     string
	selector        []string
	resourceVersion string
}

// pinConflictRetries bounds the re-reads after a resourceVersion conflict.
const pinConflictRetries = 3

// pinVolume replaces v's node selector with the server tag because other tags
// may exclude the server. It patches using the listed resourceVersion and
// retries conflicts after re-reading, up to pinConflictRetries times. It returns
// true when the pin succeeds or a re-read confirms the server-only selector.
// It returns false with no error if a re-read has an unexpected field count or
// a replica count other than one, preserving a concurrent conversion's selector.
// Other read or patch failures and unreadable selectors return an error.
func pinVolume(client CommandRunner, v singleCopyVolume) (bool, error) {
	for attempt := 0; ; attempt++ {
		patch := fmt.Sprintf(`{"metadata":{"resourceVersion":%q},"spec":{"nodeSelector":%s}}`, v.resourceVersion, jsonList([]string{ServerStorageTag}))
		out, err := client.Run(fmt.Sprintf("kubectl -n longhorn-system patch volumes.longhorn.io %s --request-timeout=30s --type merge -p '%s' >/dev/null", v.name, patch))
		if err == nil {
			return true, nil
		}
		if !strings.Contains(out+err.Error(), "(Conflict)") || attempt == pinConflictRetries {
			return false, err
		}
		current, err := client.Run(fmt.Sprintf(`kubectl -n longhorn-system get volumes.longhorn.io %s --request-timeout=30s -o jsonpath='{.spec.nodeSelector}{"\t"}{.spec.numberOfReplicas}{"\t"}{.metadata.resourceVersion}'`, v.name))
		if err != nil {
			return false, fmt.Errorf("re-reading the volume after a conflict: %w", err)
		}
		f := strings.Split(strings.TrimRight(current, "\r\n"), "\t")
		if len(f) != 3 || f[1] != "1" {
			return false, nil
		}
		if v.selector, err = parseJSONList(f[0]); err != nil {
			return false, fmt.Errorf("volume %s has an unreadable node selector %q: %w", v.name, f[0], err)
		}
		if v.pinned() {
			return true, nil
		}
		v.resourceVersion = f[2]
	}
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

// singleCopyVolumes lists the Longhorn volumes of single-copy class claims
// configured for one copy. Converted volumes retain their storage class, so
// checking the configured count prevents pinning a two-copy volume to the server.
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
	out, err := client.Run(`kubectl -n longhorn-system get volumes.longhorn.io --request-timeout=30s -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.nodeSelector}{"\t"}{.status.kubernetesStatus.namespace}{"\t"}{.status.kubernetesStatus.pvcName}{"\t"}{.spec.numberOfReplicas}{"\t"}{.metadata.resourceVersion}{"\n"}{end}'`)
	if err != nil {
		return nil, fmt.Errorf("listing Longhorn volumes: %w", err)
	}
	var volumes []singleCopyVolume
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 6 || !single[f[0]] || f[4] != "1" {
			continue
		}
		selector, err := parseJSONList(f[1])
		if err != nil {
			return nil, fmt.Errorf("volume %s has an unreadable node selector %q: %w", f[0], f[1], err)
		}
		v := singleCopyVolume{name: f[0], selector: selector, resourceVersion: f[5]}
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
	holders, err := CurrentReplicaNodes(client)
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
