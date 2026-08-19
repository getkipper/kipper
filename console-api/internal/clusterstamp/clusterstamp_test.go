package clusterstamp

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

func namespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: Namespace}}
}

// An upgrade reads this to tell whether the console-api serving the cluster is
// one that keeps a credential's allow-list. Nothing else can tell it: the image
// is a moving tag, so a completed rollout proves a new pod rather than new code.
func TestRecordStampsTheBuild(t *testing.T) {
	client := fake.NewClientset(namespace())

	if err := Record(context.Background(), client, "v0.14.0"); err != nil {
		t.Fatalf("record: %v", err)
	}

	ns, err := client.CoreV1().Namespaces().Get(context.Background(), Namespace, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ns.Annotations[labels.AnnoConsoleAPIBuild] != "v0.14.0" {
		t.Errorf("stamp = %q", ns.Annotations[labels.AnnoConsoleAPIBuild])
	}
}

// Every pod that starts records the same build, and rewriting it would be a
// namespace update per restart for nothing.
func TestRecordWritesNothingWhenTheStampIsAlreadyThere(t *testing.T) {
	ns := namespace()
	ns.Annotations = map[string]string{labels.AnnoConsoleAPIBuild: "v0.14.0"}
	client := fake.NewClientset(ns)
	client.ClearActions()

	if err := Record(context.Background(), client, "v0.14.0"); err != nil {
		t.Fatalf("record: %v", err)
	}

	for _, action := range client.Actions() {
		if action.GetVerb() == "update" {
			t.Error("a restart rewrote a stamp that was already correct")
		}
	}
}
