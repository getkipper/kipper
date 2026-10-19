package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// workloadNodes summarises pods labelled app=workload in namespace, excluding
// succeeded and failed pods. It lists pod counts by node name, then the count
// of unscheduled pods. It returns "no pods running" when no such pods remain,
// or "" when the cluster has fewer than two nodes. Listing failures return errors.
func workloadNodes(ctx context.Context, client kubernetes.Interface, namespace, workload string) (string, error) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing nodes: %w", err)
	}
	if len(nodes.Items) < 2 {
		return "", nil
	}
	pods, err := listWorkloadPods(ctx, client, namespace, workload)
	if err != nil {
		return "", fmt.Errorf("listing pods: %w", err)
	}
	perNode := map[string]int{}
	unscheduled := 0
	for _, p := range pods {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if p.Spec.NodeName == "" {
			unscheduled++
			continue
		}
		perNode[p.Spec.NodeName]++
	}
	if len(perNode) == 0 && unscheduled == 0 {
		return "no pods running", nil
	}
	names := make([]string, 0, len(perNode))
	for n := range perNode {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names)+1)
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s (%d %s)", n, perNode[n], plural(perNode[n], "pod", "pods")))
	}
	if unscheduled > 0 {
		parts = append(parts, fmt.Sprintf("%d %s not scheduled", unscheduled, plural(unscheduled, "pod", "pods")))
	}
	return strings.Join(parts, ", "), nil
}

// writeWorkloadNodes prints the Nodes summary or error for an info command.
// An empty summary with no error omits the line.
func writeWorkloadNodes(out io.Writer, nodes string, err error) {
	switch {
	case err != nil:
		say(out, "  Nodes:   not checked (%v)\n", err)
	case nodes != "":
		say(out, "  Nodes:   %s\n", nodes)
	}
}
