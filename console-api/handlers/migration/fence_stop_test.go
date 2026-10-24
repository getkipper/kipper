package migration

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/stranded"
)

func TestRestoreLeavesAServiceTheFenceStoppedAlone(t *testing.T) {
	zero := int32(0)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop", Annotations: map[string]string{
			stranded.FenceStopAnnotation: `{"node":"worker-1","replicas":1,"reason":"stale"}`}},
		Spec: appsv1.StatefulSetSpec{Replicas: &zero},
	}
	client := fake.NewSimpleClientset(sts)
	h := &Handler{Client: client}
	if err := h.restoreStatefulSet(context.Background(), "shop", "db", 1); err != nil {
		t.Fatalf("restoring: %v", err)
	}
	got, err := client.AppsV1().StatefulSets("shop").Get(context.Background(), "db", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *got.Spec.Replicas != 0 {
		t.Fatalf("a fence-stopped service was scaled to %d. Migration cleanup must honor its stop annotation", *got.Spec.Replicas)
	}
}
