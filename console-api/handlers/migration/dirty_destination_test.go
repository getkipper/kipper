package migration

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func pausedServiceFixture() (*appsv1.StatefulSet, *corev1.Secret) {
	zero := int32(0)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "minio", Namespace: "shop-prod"},
		Spec:       appsv1.StatefulSetSpec{Replicas: &zero},
	}
	// The transfer's token Secret carries which service was paused and the
	// replica count to come back to.
	tok := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      "dt-tr1-token",
		Namespace: "shop-prod",
		Labels: map[string]string{
			transferIDLabel:              "tr1",
			transferServiceLabel:         "minio",
			transferServiceReplicasLabel: "2",
		},
	}}
	return sts, tok
}

// A transfer that failed after the importer began writing leaves the volume
// part source and part target. Teardown must NOT bring the service back up on
// it: for a database that is corruption served as if it were fine.
func TestTeardownLeavesServiceStoppedWhenDestinationDirty(t *testing.T) {
	ctx := context.Background()
	sts, tok := pausedServiceFixture()
	client := fake.NewSimpleClientset(sts, tok)
	h := &Handler{Client: client}

	if err := h.deleteImportResources(ctx, "shop-prod", "tr1", true); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	got, err := client.AppsV1().StatefulSets("shop-prod").Get(ctx, "minio", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading statefulset: %v", err)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 0 {
		t.Errorf("replicas = %v, want the service left stopped", got.Spec.Replicas)
	}
	if got.Annotations["kipper.run/held-down-reason"] == "" {
		t.Error("no reason recorded — an operator would find a stopped database with no explanation")
	}
	if got.Annotations["kipper.run/held-down-restore-replicas"] != "2" {
		t.Errorf("restore hint = %q, want the pre-pause replica count 2",
			got.Annotations["kipper.run/held-down-restore-replicas"])
	}
}

// A transfer that failed before the importer wrote anything left the volume
// untouched, so the service is restored to the size it had.
func TestTeardownRestoresServiceWhenDestinationClean(t *testing.T) {
	ctx := context.Background()
	sts, tok := pausedServiceFixture()
	client := fake.NewSimpleClientset(sts, tok)
	h := &Handler{Client: client}

	if err := h.deleteImportResources(ctx, "shop-prod", "tr1", false); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	got, err := client.AppsV1().StatefulSets("shop-prod").Get(ctx, "minio", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading statefulset: %v", err)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 2 {
		t.Errorf("replicas = %v, want 2 restored", got.Spec.Replicas)
	}
	if got.Annotations["kipper.run/held-down-reason"] != "" {
		t.Error("a clean teardown must not mark the service as held down")
	}
}
