package service

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

// LonghornVolumeGVR identifies Longhorn volume resources.
var LonghornVolumeGVR = schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "volumes"}

// longhornDefaultReplicas is Longhorn's default-replica-count, which a class
// without numberOfReplicas gets.
const longhornDefaultReplicas = 3

// EffectiveCopies estimates the copy count for a new volume in class. It uses
// numberOfReplicas, defaulting to three, capped by the number of eligible nodes
// with the class's node tags, and returns at least one. Eligibility follows
// schedulingDisks and includes disks under pressure, so the estimate does not
// guarantee placement. Classes using other provisioners return one. A missing
// class, invalid replica count or unreadable Longhorn state returns an error.
func (m *Manager) EffectiveCopies(ctx context.Context, class string) (int, error) {
	sc, err := m.Client.StorageV1().StorageClasses().Get(ctx, class, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("reading the %s StorageClass: %w", class, err)
	}
	if sc.Provisioner != longhornProvisioner {
		return 1, nil
	}
	wanted := longhornDefaultReplicas
	if v, ok := sc.Parameters["numberOfReplicas"]; ok {
		if wanted, err = strconv.Atoi(v); err != nil {
			return 0, fmt.Errorf("reading numberOfReplicas of the %s StorageClass: %w", class, err)
		}
	}
	nodes, settings, err := m.longhornState(ctx)
	if err != nil {
		return 0, err
	}
	disks := schedulingDisks(nodes, settings)
	eligible := map[string]bool{}
	for _, i := range withTags(disks, classTags(sc.Parameters)) {
		eligible[disks[i].node] = true
	}
	return max(1, min(wanted, len(eligible))), nil
}

// ClaimCopies is the copy state of a claim's Longhorn volume.
type ClaimCopies struct {
	Robustness string
	Wanted     int64
	Current    int
}

// ClaimCopies reads the Longhorn volume behind pvc: its robustness, its
// configured replica count and the number of distinct nodes holding a current
// replica. A current replica has a valid healthyAt timestamp, an empty failedAt,
// and an empty lastFailedAt or one strictly before healthyAt. An unbound claim,
// a missing PV or Longhorn volume, or a volume using another driver returns the
// zero value. So does a PV whose claim name, namespace or UID differs, or a
// Longhorn volume whose claim name or namespace differs. Other read failures
// return an error.
func (m *Manager) ClaimCopies(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (ClaimCopies, error) {
	if pvc.Spec.VolumeName == "" {
		return ClaimCopies{}, nil
	}
	pv, err := m.Client.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return ClaimCopies{}, nil
	}
	if err != nil {
		return ClaimCopies{}, fmt.Errorf("reading the volume of %s: %w", pvc.Name, err)
	}
	ref := pv.Spec.ClaimRef
	if ref == nil || ref.Namespace != pvc.Namespace || ref.Name != pvc.Name || ref.UID != pvc.UID {
		return ClaimCopies{}, nil
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != longhornProvisioner {
		return ClaimCopies{}, nil
	}
	handle := pv.Spec.CSI.VolumeHandle
	volume, err := m.Dynamic.Resource(LonghornVolumeGVR).Namespace("longhorn-system").Get(ctx, handle, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return ClaimCopies{}, nil
	}
	if err != nil {
		return ClaimCopies{}, fmt.Errorf("reading the Longhorn volume of %s: %w", pvc.Name, err)
	}
	ns, _, _ := unstructured.NestedString(volume.Object, "status", "kubernetesStatus", "namespace")
	claim, _, _ := unstructured.NestedString(volume.Object, "status", "kubernetesStatus", "pvcName")
	if ns != pvc.Namespace || claim != pvc.Name {
		return ClaimCopies{}, nil
	}
	var c ClaimCopies
	c.Robustness, _, _ = unstructured.NestedString(volume.Object, "status", "robustness")
	c.Wanted, _, _ = unstructured.NestedInt64(volume.Object, "spec", "numberOfReplicas")
	replicas, err := m.Dynamic.Resource(LonghornReplicaGVR).Namespace("longhorn-system").List(ctx, metav1.ListOptions{LabelSelector: "longhornvolume=" + handle})
	if err != nil {
		return ClaimCopies{}, fmt.Errorf("reading the replicas of %s: %w", pvc.Name, err)
	}
	nodes := map[string]bool{}
	for _, r := range replicas.Items {
		node, _, _ := unstructured.NestedString(r.Object, "spec", "nodeID")
		healthyAt, _, _ := unstructured.NestedString(r.Object, "spec", "healthyAt")
		failedAt, _, _ := unstructured.NestedString(r.Object, "spec", "failedAt")
		lastFailedAt, _, _ := unstructured.NestedString(r.Object, "spec", "lastFailedAt")
		if node != "" && platform.ReplicaCurrent(healthyAt, failedAt, lastFailedAt) {
			nodes[node] = true
		}
	}
	c.Current = len(nodes)
	return c, nil
}
