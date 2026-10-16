package service

import (
	"context"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// LonghornReplicaGVR identifies Longhorn replica resources.
var LonghornReplicaGVR = schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "replicas"}

// Placement describes a service's pod location and healthy data replicas.
type Placement struct {
	PodNode   string
	DataNodes []string
}

// Placement reads the service's pod location and healthy data replicas.
// Missing pods and unbound claims leave the corresponding fields empty.
// Errors may return a partial result with the pod location.
func (m *Manager) Placement(ctx context.Context, namespace, name string) (Placement, error) {
	var p Placement
	pod, err := m.Client.CoreV1().Pods(namespace).Get(ctx, name+"-0", metav1.GetOptions{})
	switch {
	case err == nil:
		p.PodNode = pod.Spec.NodeName
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
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != "driver.longhorn.io" {
		return p, fmt.Errorf("the data volume of %s is not a Longhorn volume", name)
	}
	replicas, err := m.Dynamic.Resource(LonghornReplicaGVR).Namespace("longhorn-system").List(ctx, metav1.ListOptions{LabelSelector: "longhornvolume=" + pv.Spec.CSI.VolumeHandle})
	if err != nil {
		return p, fmt.Errorf("reading the replicas of %s: %w", name, err)
	}
	for _, r := range replicas.Items {
		node, _, _ := unstructured.NestedString(r.Object, "spec", "nodeID")
		healthyAt, _, _ := unstructured.NestedString(r.Object, "spec", "healthyAt")
		failedAt, _, _ := unstructured.NestedString(r.Object, "spec", "failedAt")
		if node != "" && healthyAt != "" && failedAt == "" {
			p.DataNodes = append(p.DataNodes, node)
		}
	}
	sort.Strings(p.DataNodes)
	return p, nil
}
