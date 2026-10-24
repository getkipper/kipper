package service

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

// LonghornReplicaGVR identifies Longhorn replica resources.
var LonghornReplicaGVR = schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "replicas"}

var longhornVolumeGVR = schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "volumes"}

// Placement describes a service's pod location and data nodes.
type Placement struct {
	PodNode          string
	Unscheduled      string
	DataNodes        []string
	UnavailableNodes []string
	UncheckedNodes   []string
}

// Placement reads the service's pod location and scheduler failure message.
// For Longhorn volumes, DataNodes lists distinct nodes holding healthy copies
// in alphabetical order. A healthy copy passes [platform.HealthyCopies] and
// [platform.NodeAvailable]. UnavailableNodes lists distinct nodes that hold a
// healthy copy but are not Ready or are fenced, in alphabetical order.
// UncheckedNodes lists distinct nodes with healthy copies whose lookup failed,
// sorted alphabetically and excluding missing nodes.
// For local-path volumes, DataNodes comes from node affinity without a health check.
// Missing pods and missing or unbound claims leave the corresponding fields empty.
// A missing Longhorn volume leaves health checks to replica markers, so
// unmarked replicas cannot qualify as copies of an empty volume. Node lookup
// failures populate UncheckedNodes. Other read failures return an error and
// may include a partial result with the pod location.
func (m *Manager) Placement(ctx context.Context, namespace, name string) (Placement, error) {
	var p Placement
	pod, err := m.Client.CoreV1().Pods(namespace).Get(ctx, name+"-0", metav1.GetOptions{})
	switch {
	case err == nil:
		p.PodNode = pod.Spec.NodeName
		p.Unscheduled = unschedulableReason(pod)
	case !errors.IsNotFound(err):
		return p, fmt.Errorf("reading the pod of %s: %w", name, err)
	}
	claim, err := m.Client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, "data-"+name+"-0", metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return p, nil
	}
	if err != nil {
		return p, fmt.Errorf("reading the data claim of %s: %w", name, err)
	}
	if claim.Spec.VolumeName == "" {
		return p, nil
	}
	// Use the CSI volume handle because the PV name may differ from the Longhorn volume name.
	pv, err := m.Client.CoreV1().PersistentVolumes().Get(ctx, claim.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		return p, fmt.Errorf("reading the data volume of %s: %w", name, err)
	}
	if node := localVolumeNode(pv); node != "" {
		p.DataNodes = []string{node}
		return p, nil
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != "driver.longhorn.io" {
		return p, fmt.Errorf("the data volume of %s is not a Longhorn volume", name)
	}
	var copyVolume platform.CopyVolume
	volume, err := m.Dynamic.Resource(longhornVolumeGVR).Namespace("longhorn-system").Get(ctx, pv.Spec.CSI.VolumeHandle, metav1.GetOptions{})
	switch {
	case err == nil:
		copyVolume = platform.CopyVolumeOf(volume.Object)
	case !errors.IsNotFound(err):
		return p, fmt.Errorf("reading the data volume of %s: %w", name, err)
	}
	replicas, err := m.Dynamic.Resource(LonghornReplicaGVR).Namespace("longhorn-system").List(ctx, metav1.ListOptions{LabelSelector: "longhornvolume=" + pv.Spec.CSI.VolumeHandle})
	if err != nil {
		return p, fmt.Errorf("reading the replicas of %s: %w", name, err)
	}
	markers := make([]platform.CopyMarkers, len(replicas.Items))
	for i, r := range replicas.Items {
		str := func(field string) string {
			v, _, _ := unstructured.NestedString(r.Object, "spec", field)
			return v
		}
		markers[i] = platform.CopyMarkers{HealthyAt: str("healthyAt"), FailedAt: str("failedAt"), LastFailedAt: str("lastFailedAt"), LastHealthyAt: str("lastHealthyAt")}
	}
	healthy := platform.HealthyCopies(copyVolume, markers)
	seen := map[string]bool{}
	for i, r := range replicas.Items {
		node, _, _ := unstructured.NestedString(r.Object, "spec", "nodeID")
		if node == "" || seen[node] || !healthy[i] {
			continue
		}
		seen[node] = true
		holder, err := m.Client.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		switch {
		case err == nil:
			if platform.NodeAvailable(holder) {
				p.DataNodes = append(p.DataNodes, node)
			} else {
				p.UnavailableNodes = append(p.UnavailableNodes, node)
			}
		case !errors.IsNotFound(err):
			p.UncheckedNodes = append(p.UncheckedNodes, node)
		}
	}
	sort.Strings(p.DataNodes)
	sort.Strings(p.UnavailableNodes)
	sort.Strings(p.UncheckedNodes)
	return p, nil
}

// localVolumeNode returns the first single-hostname affinity match for a Local
// or HostPath volume. It returns an empty string if no such match exists.
func localVolumeNode(pv *corev1.PersistentVolume) string {
	if pv.Spec.Local == nil && pv.Spec.HostPath == nil {
		return ""
	}
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return ""
	}
	for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		for _, req := range term.MatchExpressions {
			if req.Key == corev1.LabelHostname && req.Operator == corev1.NodeSelectorOpIn && len(req.Values) == 1 {
				return req.Values[0]
			}
		}
	}
	return ""
}
