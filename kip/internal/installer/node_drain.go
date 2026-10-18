package installer

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LonghornEvictionDrainPolicy makes drains wait for Longhorn to copy a volume's
// last healthy replica to another node.
const LonghornEvictionDrainPolicy = "block-for-eviction-if-contains-last-replica"

// NodeVolume is a Longhorn volume with at least one healthy replica on a node.
type NodeVolume struct {
	Name      string
	Claim     string
	Here      int
	Elsewhere int
}

// LastCopy reports whether the node holds every healthy replica of the volume.
func (v NodeVolume) LastCopy() bool { return v.Here > 0 && v.Elsewhere == 0 }

// Label names the volume by its claim when it has one.
func (v NodeVolume) Label() string {
	if v.Claim != "" {
		return v.Claim
	}
	return v.Name
}

// RequireEvictionDrainPolicy requires automatic eviction of last healthy replicas.
func RequireEvictionDrainPolicy(master CommandRunner) error {
	out, err := master.Run(`kubectl -n longhorn-system get settings.longhorn.io node-drain-policy -o jsonpath='{.value}'`)
	if err != nil {
		return fmt.Errorf("reading Longhorn's node drain policy: %w", err)
	}
	if policy := strings.TrimSpace(out); policy != LonghornEvictionDrainPolicy {
		return fmt.Errorf("the Longhorn node drain policy is %q. Draining a worker requires %s to copy last healthy replicas to other nodes. Run 'kip upgrade'", policy, LonghornEvictionDrainPolicy)
	}
	return nil
}

// VolumesOnNode lists the Longhorn volumes with a healthy replica on node,
// with how many healthy replicas each has there and on other nodes.
func VolumesOnNode(master CommandRunner, node string) ([]NodeVolume, error) {
	replicas, err := master.Run(replicaQuery)
	if err != nil {
		return nil, fmt.Errorf("listing Longhorn replicas: %w", err)
	}
	volumes, err := master.Run(`kubectl -n longhorn-system get volumes.longhorn.io --request-timeout=30s -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.kubernetesStatus.namespace}{"\t"}{.status.kubernetesStatus.pvcName}{"\n"}{end}'`)
	if err != nil {
		return nil, fmt.Errorf("listing Longhorn volumes: %w", err)
	}
	return volumesOnNode(replicas, volumes, node), nil
}

// HealthyReplicaNodes maps each Longhorn volume to the nodes with healthy replicas.
func HealthyReplicaNodes(master CommandRunner) (map[string]map[string]bool, error) {
	out, err := master.Run(replicaQuery)
	if err != nil {
		return nil, fmt.Errorf("listing Longhorn replicas: %w", err)
	}
	nodes := map[string]map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 || f[0] == "" || f[1] == "" || f[2] == "" || f[3] != "" {
			continue
		}
		if nodes[f[0]] == nil {
			nodes[f[0]] = map[string]bool{}
		}
		nodes[f[0]][f[1]] = true
	}
	return nodes, nil
}

const replicaQuery = `kubectl -n longhorn-system get replicas.longhorn.io --request-timeout=30s -o jsonpath='{range .items[*]}{.spec.volumeName}{"\t"}{.spec.nodeID}{"\t"}{.spec.healthyAt}{"\t"}{.spec.failedAt}{"\n"}{end}'`

func volumesOnNode(replicas, volumes, node string) []NodeVolume {
	byName := map[string]*NodeVolume{}
	for _, line := range strings.Split(replicas, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 || f[0] == "" {
			continue
		}
		// Count replicas with healthyAt set and failedAt empty.
		if f[2] == "" || f[3] != "" {
			continue
		}
		v := byName[f[0]]
		if v == nil {
			v = &NodeVolume{Name: f[0]}
			byName[f[0]] = v
		}
		if f[1] == node {
			v.Here++
		} else {
			v.Elsewhere++
		}
	}
	for _, line := range strings.Split(volumes, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			continue
		}
		if v := byName[f[0]]; v != nil && f[1] != "" && f[2] != "" {
			v.Claim = f[1] + "/" + f[2]
		}
	}
	var out []NodeVolume
	for _, v := range byName {
		if v.Here > 0 {
			out = append(out, *v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label() < out[j].Label() })
	return out
}

// VolumeHealth describes a Longhorn volume's health and desired replica count.
type VolumeHealth struct {
	Name       string
	Claim      string
	State      string
	Robustness string
	Replicas   int
}

// Label names the volume by its claim when it has one.
func (v VolumeHealth) Label() string {
	if v.Claim != "" {
		return v.Claim
	}
	return v.Name
}

// Unhealthy reports whether an attached volume has robustness other than healthy.
func (v VolumeHealth) Unhealthy() bool { return v.State == "attached" && v.Robustness != "healthy" }

// LonghornVolumes reads every Longhorn volume's health.
func LonghornVolumes(master CommandRunner) ([]VolumeHealth, error) {
	out, err := master.Run(`kubectl -n longhorn-system get volumes.longhorn.io --request-timeout=30s -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.kubernetesStatus.namespace}{"\t"}{.status.kubernetesStatus.pvcName}{"\t"}{.status.state}{"\t"}{.status.robustness}{"\t"}{.spec.numberOfReplicas}{"\n"}{end}'`)
	if err != nil {
		return nil, fmt.Errorf("listing Longhorn volumes: %w", err)
	}
	var volumes []VolumeHealth
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 6 {
			return nil, fmt.Errorf("unexpected Longhorn volume line %q", line)
		}
		replicas, err := strconv.Atoi(f[5])
		if err != nil {
			return nil, fmt.Errorf("volume %s has an unreadable replica count %q", f[0], f[5])
		}
		v := VolumeHealth{Name: f[0], State: f[3], Robustness: f[4], Replicas: replicas}
		if f[1] != "" && f[2] != "" {
			v.Claim = f[1] + "/" + f[2]
		}
		volumes = append(volumes, v)
	}
	return volumes, nil
}

// NodeNames lists the cluster's Kubernetes nodes.
func NodeNames(master CommandRunner) ([]string, error) {
	out, err := master.Run("kubectl get nodes --request-timeout=30s -o name")
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	// The SSH runner merges stderr, so only node lines count.
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "node/"); ok && name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("listing nodes returned none")
	}
	return names, nil
}

// NodeCount counts the cluster's Kubernetes nodes.
func NodeCount(master CommandRunner) (int, error) {
	names, err := NodeNames(master)
	return len(names), err
}

// NodeCordoned reports whether node is marked unschedulable.
func NodeCordoned(master CommandRunner, node string) (bool, error) {
	out, err := master.Run(fmt.Sprintf("kubectl get node %s --request-timeout=30s -o jsonpath='{.spec.unschedulable}'", node))
	if err != nil {
		return false, fmt.Errorf("reading node %s: %w", node, err)
	}
	return strings.TrimSpace(out) == "true", nil
}

// CordonNode marks node unschedulable.
func CordonNode(master CommandRunner, node string) error {
	if _, err := master.Run("kubectl cordon " + node + " >/dev/null"); err != nil {
		return fmt.Errorf("cordoning %s: %w", node, err)
	}
	return nil
}

// UncordonNode marks node schedulable again.
func UncordonNode(master CommandRunner, node string) error {
	if _, err := master.Run("kubectl uncordon " + node + " >/dev/null"); err != nil {
		return fmt.Errorf("uncordoning %s: %w", node, err)
	}
	return nil
}

// DrainNode drains a node within timeout, honoring pod disruption budgets.
// DaemonSet and mirror pods stay. Unmanaged pods block the drain.
// Evicted pods lose their emptyDir data. A timeout below one second returns an error.
// Drain failures include distinct error lines in their original order, with occurrence
// counts for repeated lines, or the command error if no error lines are recognised.
func DrainNode(master CommandRunner, node string, timeout time.Duration) error {
	// kubectl reads a zero timeout as no limit.
	if timeout < time.Second {
		return fmt.Errorf("the drain timeout must be at least 1s, got %s", timeout)
	}
	out, err := master.Run(fmt.Sprintf("kubectl drain %s --ignore-daemonsets --delete-emptydir-data --timeout=%s 2>&1", node, timeout))
	if err == nil {
		return nil
	}
	reasons := drainFailureReasons(out)
	if len(reasons) == 0 {
		return fmt.Errorf("draining %s: %w", node, err)
	}
	return fmt.Errorf("draining %s: %s", node, strings.Join(reasons, ". "))
}

// drainRetryNote matches kubectl's retry delay so errors with different delays
// can be counted together.
var drainRetryNote = regexp.MustCompile(` \(will retry after [^)]*\)`)

// drainFailureReasons groups kubectl drain errors by first occurrence, ignoring
// retry delays and including occurrence counts for repeated lines. kubectl
// repeats eviction errors every few seconds until timeout, so listing each
// error once keeps the failure message readable.
func drainFailureReasons(out string) []string {
	var order []string
	counts := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "error") && !strings.HasPrefix(line, "cannot delete") {
			continue
		}
		line = drainRetryNote.ReplaceAllString(line, "")
		if counts[line] == 0 {
			order = append(order, line)
		}
		counts[line]++
	}
	reasons := make([]string, 0, len(order))
	for _, line := range order {
		if n := counts[line]; n > 1 {
			line = fmt.Sprintf("%s (%d attempts)", line, n)
		}
		reasons = append(reasons, line)
	}
	return reasons
}

// WaitForWorkloadsReady waits until every workload in ready, as listed by
// ReadyWorkloads, is fully ready again.
func WaitForWorkloadsReady(client CommandRunner, ready []string, timeout time.Duration) error {
	return waitForWorkloads(client, ready, networkChangeTimeouts{workloads: timeout, interval: 5 * time.Second})
}

// ReadyWorkloads lists Deployments, StatefulSets and DaemonSets as
// Kind/namespace/name, with a positive desired pod count and at least that many ready.
func ReadyWorkloads(client CommandRunner) ([]string, error) { return readyWorkloads(client) }
