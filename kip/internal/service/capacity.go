package service

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/controller/pkg/servicecatalog"
)

// NodeHeadroom holds allocatable CPU in millicores and memory in bytes, minus
// requests from assigned, unfinished pods. Values may be negative.
// AllocatableCPUMilli is the node's allocatable CPU before any request.
type NodeHeadroom struct {
	Node                string
	AllocatableCPUMilli int64
	CPUMilli            int64
	MemoryBytes         int64
}

// NodeHeadroom returns remaining CPU and memory for Ready, uncordoned nodes
// without NoSchedule or NoExecute taints. It excludes completed pods.
// It returns nil if no nodes qualify and propagates node or pod listing errors.
// The client needs cluster-wide read access to nodes and pods.
func (m *Manager) NodeHeadroom(ctx context.Context) ([]NodeHeadroom, error) {
	nodes, err := m.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	pods, err := m.Client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	var eligible []corev1.Node
	for _, n := range nodes.Items {
		if !n.Spec.Unschedulable && nodeReady(&n) && !hasBlockingTaint(&n) {
			eligible = append(eligible, n)
		}
	}
	return Headroom(eligible, pods.Items), nil
}

// Headroom subtracts assigned, unfinished pods' requests from each node's
// allocatable CPU and memory, using platform.PodRequests. It includes all
// supplied nodes in input order, including unready and cordoned nodes. CPU is
// in millicores and memory in bytes, and either remainder may be negative. An
// empty node slice produces a nil result.
func Headroom(nodes []corev1.Node, pods []corev1.Pod) []NodeHeadroom {
	var out []NodeHeadroom
	index := map[string]int{}
	for i := range nodes {
		n := &nodes[i]
		index[n.Name] = len(out)
		cpu := n.Status.Allocatable.Cpu().MilliValue()
		out = append(out, NodeHeadroom{
			Node:                n.Name,
			AllocatableCPUMilli: cpu,
			CPUMilli:            cpu,
			MemoryBytes:         n.Status.Allocatable.Memory().Value(),
		})
	}
	for i := range pods {
		pod := &pods[i]
		at, ok := index[pod.Spec.NodeName]
		if !ok || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		cpu, memory := platform.PodRequests(&pod.Spec)
		out[at].CPUMilli -= cpu
		out[at].MemoryBytes -= memory
	}
	return out
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func hasBlockingTaint(n *corev1.Node) bool {
	for _, t := range n.Spec.Taints {
		if t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}

// CheckFits returns nil if one node has enough CPU and memory for the service.
// Empty quantities use the service type's defaults, with the cleanup init
// container's requests as a minimum. Invalid Kubernetes quantities, an empty
// node list or insufficient capacity return an error. Other scheduling
// constraints are outside this check.
func CheckFits(nodes []NodeHeadroom, serviceType, cpu, memory string) error {
	defaultCPU, defaultMemory := servicecatalog.DefaultResources(serviceType)
	if cpu == "" {
		cpu = defaultCPU
	}
	if memory == "" {
		memory = defaultMemory
	}
	cpuQ, err := resource.ParseQuantity(cpu)
	if err != nil {
		return fmt.Errorf("--cpu %q: %w", cpu, err)
	}
	memQ, err := resource.ParseQuantity(memory)
	if err != nil {
		return fmt.Errorf("--memory %q: %w", memory, err)
	}
	initCPU, initMemory := servicecatalog.InitContainerRequests()
	initCPUQ, initMemoryQ := resource.MustParse(initCPU), resource.MustParse(initMemory)
	needCPU := max(cpuQ.MilliValue(), initCPUQ.MilliValue())
	needMemory := max(memQ.Value(), initMemoryQ.Value())
	if len(nodes) == 0 {
		return fmt.Errorf("no node is currently eligible. Check 'kip status'")
	}
	var bestCPU, bestMemory NodeHeadroom
	for _, n := range nodes {
		if n.CPUMilli >= needCPU && n.MemoryBytes >= needMemory {
			return nil
		}
		if n.CPUMilli > bestCPU.CPUMilli || bestCPU.Node == "" {
			bestCPU = n
		}
		if n.MemoryBytes > bestMemory.MemoryBytes || bestMemory.Node == "" {
			bestMemory = n
		}
	}
	return fmt.Errorf("no node has capacity for a %s service requesting %dm CPU and %s memory. The largest remaining capacities are %dm CPU (%s) and %s memory (%s). Reduce requests with --cpu and --memory, or add cluster capacity",
		serviceType, needCPU, formatMemory(needMemory), max(bestCPU.CPUMilli, 0), bestCPU.Node, formatMemory(max(bestMemory.MemoryBytes, 0)), bestMemory.Node)
}

// formatMemory formats non-negative byte counts as exact whole Gi or truncated
// Mi, keeping capacity messages readable without overstating available memory.
func formatMemory(bytes int64) string {
	if bytes >= 1024*1024*1024 && bytes%(1024*1024*1024) == 0 {
		return fmt.Sprintf("%dGi", bytes/(1024*1024*1024))
	}
	return fmt.Sprintf("%dMi", bytes/(1024*1024))
}
