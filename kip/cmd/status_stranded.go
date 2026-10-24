package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/stranded"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/service"
)

// reportStranded lists, for every node that is NotReady or fenced, the
// Kipper services with a single-node Longhorn volume on it, the nodes that
// hold a current copy of their data, and the command that recovers them. It
// also lists the services kip node fence left stopped. It prints nothing when
// there is nothing to report. Resource list failures omit the section, while
// replica lookup failures omit only copy locations.
func reportStranded(ctx context.Context, out io.Writer, client kubernetes.Interface, dyn dynamic.Interface, records []installer.NodeRecord) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	var down []corev1.Node
	for _, n := range nodes.Items {
		if !stranded.Ready(&n) {
			down = append(down, n)
		}
	}
	sets, err := client.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	var stopped []string
	for _, s := range sets.Items {
		raw, ok := s.Annotations[installer.FenceStopAnnotation]
		if !ok {
			continue
		}
		var mark installer.FenceStopMark
		_ = json.Unmarshal([]byte(raw), &mark)
		stopped = append(stopped, fmt.Sprintf("%s (%s), stopped when %s was fenced. Start it with 'kip service start %s'", s.Name, s.Namespace, orDash(mark.Node), s.Name))
	}
	if len(down) == 0 && len(stopped) == 0 {
		return
	}

	var lines []string
	if len(down) > 0 {
		pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
		if err != nil {
			return
		}
		claims, err := client.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
		if err != nil {
			return
		}
		pvs, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return
		}
		replicas, replicasErr := readReplicas(ctx, dyn)
		// The local copy of the records is empty on a cluster that never
		// had a worker, and then every node counts.
		if len(records) > 0 {
			replicas = installer.AvailableReplicas(replicas, records, "")
		}
		for _, n := range down {
			lines = append(lines, strandedOn(n, stranded.Find(n.Name, pods.Items, claims.Items, pvs.Items), replicas, replicasErr, records)...)
		}
	}
	lines = append(lines, stopped...)
	if len(lines) == 0 {
		return
	}
	say(out, "  Stranded:\n")
	for _, l := range lines {
		say(out, "    ⚠  %s\n", l)
	}
	say(out, "\n")
}

// strandedOn describes the services with a single-node volume on node.
// replicas holds only the copies on nodes that are not fenced.
func strandedOn(node corev1.Node, found []stranded.Service, replicas []stranded.Replica, replicasErr error, records []installer.NodeRecord) []string {
	volumes := map[string][]string{}
	var order []string
	for _, s := range found {
		if !s.ReadWriteOnce {
			continue
		}
		label := s.Label()
		if _, ok := volumes[label]; !ok {
			order = append(order, label)
		}
		volumes[label] = append(volumes[label], s.Volume)
	}
	if len(order) == 0 {
		return nil
	}
	var lines []string
	for _, label := range order {
		where := "kip could not read where its copies are"
		if replicasErr == nil {
			where = currentCopyText(node.Name, volumes[label], replicas)
		}
		lines = append(lines, fmt.Sprintf("%s on %s: %s", label, node.Name, where))
	}
	host := "<host>"
	for _, r := range records {
		if r.Name == node.Name {
			host = r.Host
		}
	}
	if stranded.OutOfService(&node) {
		lines = append(lines, fmt.Sprintf("%s is fenced. Rejoin it with 'kip node add %s', or remove it with 'kip node remove %s --force'", node.Name, host, node.Name))
	} else {
		lines = append(lines, fmt.Sprintf("%s is not Ready. If it is down for good, run 'kip node fence %s' to start these services on another node", node.Name, node.Name))
	}
	return lines
}

// currentCopyText names the nodes other than node that hold a current copy
// of every one of volumes.
func currentCopyText(node string, volumes []string, replicas []stranded.Replica) string {
	copies := stranded.CopiesOfAll(volumes, node, replicas)
	if len(copies) == 0 {
		return "no other node holds a current copy of its data"
	}
	return "current copy on " + strings.Join(copies, ", ")
}

func readReplicas(ctx context.Context, dyn dynamic.Interface) ([]stranded.Replica, error) {
	if dyn == nil {
		return nil, fmt.Errorf("no cluster connection")
	}
	list, err := dyn.Resource(service.LonghornReplicaGVR).Namespace("longhorn-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	data, err := list.MarshalJSON()
	if err != nil {
		return nil, err
	}
	return stranded.ReplicasFromJSON(data)
}
