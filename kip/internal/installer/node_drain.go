package installer

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

// LonghornEvictionDrainPolicy makes drains wait for Longhorn to copy a volume's
// last healthy replica to another node.
const LonghornEvictionDrainPolicy = "block-for-eviction-if-contains-last-replica"

// NodeVolume is a Longhorn volume with a current replica on a node.
type NodeVolume struct {
	Name      string
	Claim     string
	Here      int
	Elsewhere int
}

// LastCopy reports whether this node holds a current copy and no other
// available node holds one.
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
		return failedRead("reading Longhorn's node drain policy", out, err)
	}
	if policy := strings.TrimSpace(out); policy != LonghornEvictionDrainPolicy {
		return fmt.Errorf("the Longhorn node drain policy is %q. Draining a worker requires %s to copy last healthy replicas to other nodes. Run 'kip upgrade'", policy, LonghornEvictionDrainPolicy)
	}
	return nil
}

// VolumesOnNode lists Longhorn volumes with a current replica on node, sorted
// by Label. It returns nil if none match. Here is 1 for each volume, regardless
// of node availability. Elsewhere counts distinct available nodes holding a
// current replica. [platform.ReplicaCurrent] and [platform.NodeAvailable] define
// these checks. A failed replica, volume or node inventory query returns an error.
func VolumesOnNode(master CommandRunner, node string) ([]NodeVolume, error) {
	replicas, err := readReplicas(master)
	if err != nil {
		return nil, err
	}
	available, err := availableNodes(master)
	if err != nil {
		return nil, err
	}
	volumes, err := master.Run(`kubectl -n longhorn-system get volumes.longhorn.io --request-timeout=30s -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.kubernetesStatus.namespace}{"\t"}{.status.kubernetesStatus.pvcName}{"\n"}{end}'`)
	if err != nil {
		return nil, failedRead("listing Longhorn volumes", volumes, err)
	}
	return volumesOnNode(replicas, available, volumes, node), nil
}

// HealthyReplicaNodes maps Longhorn volumes to sets of available nodes holding
// current replicas, using [platform.NodeAvailable] and [platform.ReplicaCurrent].
// Volumes without a qualifying replica are omitted. A failed replica or node
// inventory query returns an error.
func HealthyReplicaNodes(master CommandRunner) (map[string]map[string]bool, error) {
	replicas, err := readReplicas(master)
	if err != nil {
		return nil, err
	}
	available, err := availableNodes(master)
	if err != nil {
		return nil, err
	}
	return replicaHolders(replicas, available), nil
}

// CurrentReplicaNodes maps Longhorn volumes to sets of nodes holding current
// replicas, as defined by [platform.ReplicaCurrent]. It includes unavailable
// nodes to locate current data independently of whether it can be served.
// Volumes without a qualifying replica are omitted. A failed replica inventory
// query returns an error.
func CurrentReplicaNodes(master CommandRunner) (map[string]map[string]bool, error) {
	replicas, err := readReplicas(master)
	if err != nil {
		return nil, err
	}
	return replicaHolders(replicas, nil), nil
}

// replicaHolders maps volumes to the nodes holding a current replica. With
// available set, only available nodes are kept.
func replicaHolders(replicas []longhornReplica, available map[string]bool) map[string]map[string]bool {
	nodes := map[string]map[string]bool{}
	for _, r := range replicas {
		if r.node == "" || !r.current || (available != nil && !available[r.node]) {
			continue
		}
		if nodes[r.volume] == nil {
			nodes[r.volume] = map[string]bool{}
		}
		nodes[r.volume][r.node] = true
	}
	return nodes
}

const replicaQuery = `kubectl -n longhorn-system get replicas.longhorn.io --request-timeout=30s -o jsonpath='{range .items[*]}{.spec.volumeName}{"\t"}{.spec.nodeID}{"\t"}{.spec.healthyAt}{"\t"}{.spec.failedAt}{"\t"}{.spec.lastFailedAt}{"\n"}{end}'`

// longhornReplica is one line of replicaQuery's output.
type longhornReplica struct {
	volume, node string
	current      bool
}

func readReplicas(master CommandRunner) ([]longhornReplica, error) {
	out, err := master.Run(replicaQuery)
	if err != nil {
		return nil, failedRead("listing Longhorn replicas", out, err)
	}
	var replicas []longhornReplica
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 5 || f[0] == "" {
			continue
		}
		replicas = append(replicas, longhornReplica{volume: f[0], node: f[1], current: platform.ReplicaCurrent(f[2], f[3], f[4])})
	}
	return replicas, nil
}

const nodeAvailabilityQuery = `kubectl get nodes --request-timeout=30s -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.conditions[?(@.type=="Ready")].status}{"\t"}{.spec.taints[*].key}{"\n"}{end}'`

// availableNodes checks Kubernetes nodes with [platform.NodeAvailable].
// A node missing from the result is treated as unavailable.
func availableNodes(master CommandRunner) (map[string]bool, error) {
	out, err := master.Run(nodeAvailabilityQuery)
	if err != nil {
		return nil, failedRead("listing nodes", out, err)
	}
	available := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 || f[0] == "" {
			continue
		}
		node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionStatus(f[1])}}}}
		for _, key := range strings.Fields(f[2]) {
			node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{Key: key})
		}
		available[f[0]] = platform.NodeAvailable(node)
	}
	return available, nil
}

func volumesOnNode(replicas []longhornReplica, available map[string]bool, volumes, node string) []NodeVolume {
	byName := map[string]*NodeVolume{}
	elsewhere := map[string]map[string]bool{}
	for _, r := range replicas {
		if r.node == "" || !r.current {
			continue
		}
		v := byName[r.volume]
		if v == nil {
			v = &NodeVolume{Name: r.volume}
			byName[r.volume] = v
			elsewhere[r.volume] = map[string]bool{}
		}
		switch {
		case r.node == node:
			v.Here = 1
		case available[r.node] && !elsewhere[r.volume][r.node]:
			elsewhere[r.volume][r.node] = true
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
		return nil, failedRead("listing Longhorn volumes", out, err)
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
		return nil, failedRead("listing nodes", out, err)
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
		return false, failedRead(fmt.Sprintf("reading node %s", node), out, err)
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

// drainRound limits each kubectl drain so an interrupted kip run leaves
// only a short drain round to finish.
const drainRound = 2 * time.Minute

// DrainNode drains a node with a total kubectl timeout budget of timeout,
// honoring pod disruption budgets. DaemonSet and mirror pods stay, and
// unmanaged pods block the drain. Evicted pods lose their emptyDir data.
// Each round has a timeout of at most two minutes. Only round timeout failures
// are retried. A timeout below one second returns an error.
// Drain failures list recognized reasons from the last round in order of first
// appearance, with attempt counts for repeated errors that include retry delays.
// When the timeout budget is exhausted, the error ends with the total timeout.
// Other failures wrap the command error if no reason is recognized.
func DrainNode(master CommandRunner, node string, timeout time.Duration) error {
	// kubectl reads a zero timeout as no limit.
	if timeout < time.Second {
		return fmt.Errorf("the drain timeout must be at least 1s, got %s", timeout)
	}
	var out string
	var err error
	timedOut := false
	for left := timeout; left > 0; left -= drainRound {
		out, err = master.Run(fmt.Sprintf("kubectl drain %s --ignore-daemonsets --delete-emptydir-data --timeout=%s 2>&1", node, min(left, drainRound)))
		if err == nil {
			return nil
		}
		timedOut = roundTimedOut(out)
		if !timedOut {
			break
		}
	}
	// Keep kubectl's lowercase errors in one sentence.
	msg := strings.Join(drainFailureReasons(out), " and ")
	switch {
	case timedOut && msg == "":
		msg = fmt.Sprintf("the drain did not finish within %s", timeout)
	case timedOut:
		msg += fmt.Sprintf(". The drain did not finish within %s", timeout)
	case msg == "":
		return fmt.Errorf("draining %s: %w", node, err)
	}
	return fmt.Errorf("draining %s: %s", node, msg)
}

// roundTimedOut reports whether kubectl drain's output indicates a round timeout.
// DrainNode reports the total timeout budget in place of kubectl's round timeout.
func roundTimedOut(text string) bool {
	return strings.Contains(text, "global timeout reached") || strings.Contains(text, "drain did not complete within") ||
		strings.Contains(text, "to terminate: context deadline exceeded")
}

// drainRetryNote matches kubectl's retry delay so errors with different delays
// can be counted together.
var drainRetryNote = regexp.MustCompile(` \(will retry after [^)]*\)`)

// drainSummary captures errors from kubectl's summaries, which can repeat
// errors printed individually at the end of the output.
var drainSummary = regexp.MustCompile(`^(?:unable to drain node "[^"]*" due to error: |There are pending pods in node "[^"]*" when an error occurred: )\[?(.*?)\]?(?:, continuing command\.\.\.)?$`)

// drainRoundTimeout matches kubectl's timeout suffix for a pod whose eviction
// was still pending when the round timed out.
var drainRoundTimeout = regexp.MustCompile(`: global timeout reached: \S+$`)

// withoutRoundTimeout removes a recognized round timeout suffix from line and
// reports whether it found one. For a pod still terminating at the deadline,
// kubectl appends "context deadline exceeded" after "to terminate". Other
// occurrences are preserved because they can describe request timeouts.
func withoutRoundTimeout(line string) (string, bool) {
	if rest, ok := strings.CutSuffix(line, " to terminate: context deadline exceeded"); ok {
		return rest + " to terminate", true
	}
	if loc := drainRoundTimeout.FindStringIndex(line); loc != nil {
		return line[:loc[0]], true
	}
	return line, false
}

// drainPod matches the three ways kubectl names a pod in a drain error, with
// the pod's name and namespace as submatches.
var drainPod = regexp.MustCompile(`pods/"([^"]*)" -n "([^"]*)"|pod "([^"]*)" (?:from terminating namespace|in namespace) "([^"]*)"`)

// drainPodName returns a drain error's pod as namespace/name, or "" when the
// error has no recognized pod reference.
func drainPodName(line string) string {
	m := drainPod.FindStringSubmatch(line)
	switch {
	case m == nil:
		return ""
	case m[1] != "":
		return m[2] + "/" + m[1]
	default:
		return m[4] + "/" + m[3]
	}
}

// drainFailureReasons returns distinct recognized drain errors in order of
// first appearance. kubectl repeats eviction errors at retry intervals, so lines
// with retry delays count as attempts. Counts above one appear in the result.
// Summaries provide a fallback when no individual error is recognized, since
// kubectl can also print summary errors individually. Per-pod round timeout errors
// retain the text before the timeout suffix so DrainNode can report the total
// timeout. A round timeout error is omitted if an earlier error names the same pod.
func drainFailureReasons(out string) []string {
	var order []string
	attempts := map[string]int{}
	seen := map[string]bool{}
	named := map[string]bool{}
	var summaries []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "error: ")
		if m := drainSummary.FindStringSubmatch(line); m != nil {
			summaries = append(summaries, m[1])
			continue
		}
		if !strings.HasPrefix(line, "error") && !strings.HasPrefix(line, "cannot delete") {
			continue
		}
		if rest, ok := withoutRoundTimeout(line); ok {
			if pod := drainPodName(line); pod != "" && named[pod] {
				continue
			}
			line = rest
		}
		if pod := drainPodName(line); pod != "" {
			named[pod] = true
		}
		retry := drainRetryNote.MatchString(line)
		line = strings.TrimSuffix(drainRetryNote.ReplaceAllString(line, ""), ".")
		if !seen[line] {
			seen[line] = true
			order = append(order, line)
		}
		if retry {
			attempts[line]++
		}
	}
	if len(order) == 0 && len(summaries) > 0 {
		return drainFailureReasons(strings.Join(summaries, "\n"))
	}
	reasons := make([]string, 0, len(order))
	for _, line := range order {
		if n := attempts[line]; n > 1 {
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
