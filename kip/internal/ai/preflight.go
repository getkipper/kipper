package ai

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// gpuResourceName is the standard NVIDIA device-plugin resource key.
const gpuResourceName = corev1.ResourceName("nvidia.com/gpu")

// gpuMemoryLabel is the per-device memory label set by NVIDIA GPU Feature
// Discovery, expressed in MiB.
const gpuMemoryLabel = "nvidia.com/gpu.memory"

// NodeReport is the AI-relevant view of a single Ready node.
type NodeReport struct {
	Name              string
	AllocatableMemory int64
	RequestedMemory   int64
	FreeMemory        int64
	GPUCount          int64
	// PerGPUMemoryBytes is the memory of a single device on this node.
	// Tier 4 requires a single GPU at or above the threshold; aggregating
	// VRAM across devices would mis-classify two small GPUs that no single
	// requested device could run a 14B model on.
	PerGPUMemoryBytes int64
}

// ClusterReport summarises capacity for the AI bundle preflight across all
// Ready nodes in the cluster.
//
// Phase 1 assumes a single-node topology: the bundle's Deployments have no
// node selectors, so we tier on the BestNode (the most-free Ready node)
// rather than the aggregated total. Aggregating across nodes would
// classify two 9 GiB boxes as tier 2 even though no single node has the
// 16 GiB needed to run a 7B model.
type ClusterReport struct {
	Nodes        []NodeReport
	TotalFree    int64
	BestNode     NodeReport
	HasGPU       bool
	MaxGPUMemory int64
	UnreadyNodes []string
}

// Tier maps the cluster report to the highest tier that fits on the
// single best Ready node. Memory and GPU checks both come from BestNode
// so a GPU on a different machine does not get credited here. Tier 4
// requires a single device with the full VRAM threshold; we never sum
// memory across devices on the same node.
func (r ClusterReport) Tier() Tier {
	free := r.BestNode.FreeMemory
	if free < tierOneMemory {
		return TierNone
	}
	if r.BestNode.GPUCount > 0 {
		if free >= tierTwoMemory && r.BestNode.PerGPUMemoryBytes >= tierFourGPUVRAM {
			return TierFour
		}
		if free >= tierTwoMemory {
			return TierThree
		}
		return TierOne
	}
	if free >= tierTwoMemory {
		return TierTwo
	}
	return TierOne
}

// Detect inspects every Ready node and the pods running on it to produce a
// ClusterReport. Pods in terminal phases are excluded from the requested-
// memory total.
func Detect(ctx context.Context, client kubernetes.Interface) (ClusterReport, error) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return ClusterReport{}, fmt.Errorf("listing nodes: %w", err)
	}

	report := ClusterReport{}
	for _, node := range nodes.Items {
		if !isNodeReady(node) {
			report.UnreadyNodes = append(report.UnreadyNodes, node.Name)
			continue
		}
		// Cordoned nodes (spec.unschedulable=true) cannot accept new
		// pods. Tainted nodes that we do not tolerate are also off-
		// limits. Either way, including them in the tier picture would
		// let preflight pass against capacity the scheduler cannot
		// actually use.
		if node.Spec.Unschedulable {
			report.UnreadyNodes = append(report.UnreadyNodes, node.Name)
			continue
		}
		if hasBlockingTaint(node) {
			report.UnreadyNodes = append(report.UnreadyNodes, node.Name)
			continue
		}

		nodeReport, err := buildNodeReport(ctx, client, node)
		if err != nil {
			return ClusterReport{}, err
		}
		report.Nodes = append(report.Nodes, nodeReport)
		report.TotalFree += nodeReport.FreeMemory

		if nodeReport.FreeMemory > report.BestNode.FreeMemory {
			report.BestNode = nodeReport
		}

		if nodeReport.GPUCount > 0 {
			report.HasGPU = true
			if nodeReport.PerGPUMemoryBytes > report.MaxGPUMemory {
				report.MaxGPUMemory = nodeReport.PerGPUMemoryBytes
			}
		}
	}

	return report, nil
}

func buildNodeReport(ctx context.Context, client kubernetes.Interface, node corev1.Node) (NodeReport, error) {
	// Allocatable, not Capacity: kubelet subtracts system-reserved and
	// kube-reserved overheads from Capacity, and a node with capacity
	// but zero allocatable (e.g. GPU device plugin not yet ready) cannot
	// host workloads.
	alloc := node.Status.Allocatable.Memory().Value()

	pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + node.Name,
	})
	if err != nil {
		return NodeReport{}, fmt.Errorf("listing pods on %s: %w", node.Name, err)
	}

	var requested, gpuRequested int64
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range pod.Spec.Containers {
			requested += c.Resources.Requests.Memory().Value()
			if gpu, ok := c.Resources.Requests[gpuResourceName]; ok {
				gpuRequested += gpu.Value()
			}
		}
	}

	free := alloc - requested
	if free < 0 {
		free = 0
	}

	// GPU count reflects FREE devices, not raw capacity. A node where
	// another workload already claims the only GPU, or where the device
	// plugin has not yet reported allocatable GPUs, must not promote us
	// to a GPU tier; the install would render Ollama with gpu.enabled
	// and the pod would stay Pending forever. Allocatable already
	// excludes devices the kubelet considers unavailable.
	gpuAllocatable := node.Status.Allocatable.Name(gpuResourceName, "").Value()
	gpuFree := gpuAllocatable - gpuRequested
	if gpuFree < 0 {
		gpuFree = 0
	}
	perGPU := perGPUMemoryBytes(node.Labels)

	return NodeReport{
		Name:              node.Name,
		AllocatableMemory: alloc,
		RequestedMemory:   requested,
		FreeMemory:        free,
		GPUCount:          gpuFree,
		PerGPUMemoryBytes: perGPU,
	}, nil
}

func isNodeReady(node corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// hasBlockingTaint returns true when the node carries a NoSchedule or
// NoExecute taint that the AI bundle does not tolerate. The bundle's
// pods do not declare any tolerations, so any taint with those effects
// is blocking.
func hasBlockingTaint(node corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}

// perGPUMemoryBytes reads the NVIDIA GPU Feature Discovery label, which
// exposes per-device memory in MiB. Returns 0 when the label is absent or
// unparseable, which the caller treats as "unknown VRAM".
func perGPUMemoryBytes(labels map[string]string) int64 {
	raw, ok := labels[gpuMemoryLabel]
	if !ok {
		return 0
	}
	mib, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return mib * 1024 * 1024
}
