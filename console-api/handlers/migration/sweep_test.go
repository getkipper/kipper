package migration

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// TestSweepRemovesATransferWhoseSessionIsGone guards against repeated startup
// crashes when a transfer outlives its session record and the startup sweep runs.
func TestSweepRemovesATransferWhoseSessionIsGone(t *testing.T) {
	orphan := &kipperv1.DataTransfer{
		ObjectMeta: metav1.ObjectMeta{Name: "dt-orphan", Namespace: "shop-prod"},
		Spec:       kipperv1.DataTransferSpec{SessionID: "gone", Kind: "dbDump"},
	}
	h := &Handler{
		Client:   fake.NewSimpleClientset(),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithObjects(orphan).Build(),
		Sessions: NewSessionStore(),
	}

	h.sweepAbandonedTransfersOnce(context.Background())

	var left kipperv1.DataTransferList
	if err := h.CRClient.List(context.Background(), &left); err != nil {
		t.Fatal(err)
	}
	if len(left.Items) != 0 {
		t.Fatalf("%d transfers left, want the orphan removed", len(left.Items))
	}
}
