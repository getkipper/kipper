package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/stranded"
)

func downNode(taints ...corev1.Taint) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec:       corev1.NodeSpec{Taints: taints},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionUnknown}}},
	}
}

func strandedDB() []runtime.Object {
	return []runtime.Object{
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

func nodeAlertReason(t *testing.T, rc *ResourceController, node corev1.Node) string {
	t.Helper()
	entries := entriesOf(rc.checkNodeReady(context.Background(), []corev1.Node{node}))
	if len(entries) != 1 {
		t.Fatalf("expected one node alert, got %+v", entries)
	}
	return entries[0].Reason
}

func TestNodeAlertNamesStrandedServicesTheirCopyAndTheFence(t *testing.T) {
	rc := NewResourceController(fake.NewClientset(strandedDB()...), nil)
	rc.listReplicas = func(context.Context) ([]stranded.Replica, error) {
		return []stranded.Replica{{Name: "r1", Volume: "pvc-1", Node: "server-1", HealthyAt: "2026-10-24T08:00:00Z"}}, nil
	}
	reason := nodeAlertReason(t, rc, downNode())
	for _, want := range []string{"db (shop)", "current copy on server-1", "kip node fence worker-1"} {
		if !strings.Contains(reason, want) {
			t.Errorf("alert %q does not mention %q", reason, want)
		}
	}
}

func TestNodeAlertLeavesTheCopyOutWhenReplicasCannotBeRead(t *testing.T) {
	rc := NewResourceController(fake.NewClientset(strandedDB()...), nil)
	rc.listReplicas = func(context.Context) ([]stranded.Replica, error) { return nil, errors.New("no Longhorn") }
	reason := nodeAlertReason(t, rc, downNode())
	if !strings.Contains(reason, "db (shop)") || !strings.Contains(reason, "kip node fence worker-1") || strings.Contains(reason, "current copy") {
		t.Errorf("alert %q", reason)
	}
}

func TestNodeAlertWithoutStrandedServicesKeepsItsText(t *testing.T) {
	rc := NewResourceController(fake.NewClientset(), nil)
	reason := nodeAlertReason(t, rc, downNode())
	if strings.Contains(reason, "kip node fence") {
		t.Errorf("a node without Kipper data needs no fence: %q", reason)
	}
}

func TestNodeAlertForAFencedNodeNamesTheRejoin(t *testing.T) {
	rc := NewResourceController(fake.NewClientset(), nil)
	fenced := downNode(stranded.Taint())
	fenced.Status.Conditions[0].Status = corev1.ConditionTrue
	reason := nodeAlertReason(t, rc, fenced)
	if !strings.Contains(reason, "fenced") || !strings.Contains(reason, "kip node add") || !strings.Contains(reason, "kip node remove worker-1 --force") {
		t.Errorf("alert %q", reason)
	}
}
