package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/stranded"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/service"
)

func strandedNode(name string, ready corev1.ConditionStatus, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Taints: taints},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}}},
	}
}

func strandedObjects(workerTaints ...corev1.Taint) []runtime.Object {
	return []runtime.Object{
		strandedNode("server-1", corev1.ConditionTrue),
		strandedNode("worker-1", corev1.ConditionUnknown, workerTaints...),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "db-0", Namespace: "shop", Labels: map[string]string{"app.kubernetes.io/managed-by": "kipper"},
				OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db", APIVersion: "apps/v1"}}},
			Spec: corev1.PodSpec{NodeName: "worker-1", Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-db-0"}}}}},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "shop"},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-1", AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-1"},
			Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: stranded.LonghornDriver, VolumeHandle: "pvc-1"}}},
		},
	}
}

func replicaClient(replicas ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	objects := make([]runtime.Object, 0, len(replicas))
	for _, r := range replicas {
		objects = append(objects, r)
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		service.LonghornReplicaGVR: "ReplicaList",
	}, objects...)
}

func replicaObject(name, volume, node, healthyAt string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "longhorn.io/v1beta2", "kind": "Replica",
		"metadata": map[string]any{"name": name, "namespace": "longhorn-system"},
		"spec":     map[string]any{"volumeName": volume, "nodeID": node, "healthyAt": healthyAt},
	}}
}

func TestStatusNamesStrandedServicesWithTheirCopyAndTheFenceCommand(t *testing.T) {
	client := fake.NewSimpleClientset(strandedObjects()...)
	var out bytes.Buffer
	reportStranded(context.Background(), &out, client, replicaClient(replicaObject("r1", "pvc-1", "server-1", healthy)), nil)
	assert.Contains(t, out.String(), "Stranded:")
	assert.Contains(t, out.String(), "db (shop) on worker-1: current copy on server-1")
	assert.Contains(t, out.String(), "kip node fence worker-1")
}

func TestStatusSaysWhenNoOtherNodeHoldsACurrentCopy(t *testing.T) {
	client := fake.NewSimpleClientset(strandedObjects()...)
	var out bytes.Buffer
	reportStranded(context.Background(), &out, client, replicaClient(replicaObject("r1", "pvc-1", "worker-1", healthy)), nil)
	assert.Contains(t, out.String(), "no other node holds a current copy")
}

func TestStatusPointsAFencedNodeAtRejoinOrRemoval(t *testing.T) {
	client := fake.NewSimpleClientset(strandedObjects(stranded.Taint())...)
	var out bytes.Buffer
	records := []installer.NodeRecord{{Name: "worker-1", Host: "203.0.113.11"}}
	reportStranded(context.Background(), &out, client, replicaClient(), records)
	assert.Contains(t, out.String(), "kip node add 203.0.113.11")
	assert.Contains(t, out.String(), "kip node remove worker-1 --force")
}

func TestStatusListsServicesTheFenceStopped(t *testing.T) {
	objects := []runtime.Object{
		strandedNode("server-1", corev1.ConditionTrue),
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop", Annotations: map[string]string{
			installer.FenceStopAnnotation: `{"node":"worker-1","replicas":1,"reason":"stale","volumes":["pvc-1"]}`}}},
	}
	var out bytes.Buffer
	reportStranded(context.Background(), &out, fake.NewSimpleClientset(objects...), replicaClient(), nil)
	assert.Contains(t, out.String(), "kip service start db")
}

func TestStatusPrintsNothingWhenEveryNodeIsReady(t *testing.T) {
	var out bytes.Buffer
	reportStranded(context.Background(), &out, fake.NewSimpleClientset(strandedNode("server-1", corev1.ConditionTrue)), replicaClient(), nil)
	assert.Empty(t, out.String())
}

func TestStatusNeverNamesAFencedWorkerAsHoldingACurrentCopy(t *testing.T) {
	client := fake.NewSimpleClientset(strandedObjects()...)
	var out bytes.Buffer
	records := []installer.NodeRecord{{Name: "worker-2", State: installer.NodeStateFenced, Fence: &config.NodeFence{}}}
	reportStranded(context.Background(), &out, client, replicaClient(replicaObject("r1", "pvc-1", "worker-2", healthy)), records)
	assert.Contains(t, out.String(), "no other node holds a current copy")
}
