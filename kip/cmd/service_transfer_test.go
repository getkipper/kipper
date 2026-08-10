package cmd

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

// TestPauseTuningRoundTrip pins the bulk-operation shield handshake: pausing
// stamps the StatefulSet with a bounded RFC 3339 lease and resuming removes
// the stamp, so the resource tuner cannot roll the database mid-import yet
// always recovers even if the CLI dies.
func TestPauseTuningRoundTrip(t *testing.T) {
	clientset := fake.NewSimpleClientset(&appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "mongodb", Namespace: "blog-test"},
	})

	resume, err := pauseTuning(context.Background(), clientset, "blog-test", "mongodb")
	if err != nil {
		t.Fatalf("pauseTuning: %v", err)
	}

	sts, err := clientset.AppsV1().StatefulSets("blog-test").Get(context.Background(), "mongodb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting statefulset: %v", err)
	}
	raw, ok := sts.Annotations[labels.AnnoTuningPausedUntil]
	if !ok {
		t.Fatal("expected the tuning-paused-until annotation to be set")
	}
	until, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("annotation is not RFC 3339: %q", raw)
	}
	if remaining := time.Until(until); remaining <= 0 || remaining > tuningPauseLease {
		t.Errorf("deadline %s is outside the pause lease", raw)
	}

	resume()
	sts, err = clientset.AppsV1().StatefulSets("blog-test").Get(context.Background(), "mongodb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting statefulset after resume: %v", err)
	}
	if _, ok := sts.Annotations[labels.AnnoTuningPausedUntil]; ok {
		t.Error("expected resume to remove the annotation")
	}
}

// TestPauseTuningResumeLeavesForeignStamp: when another transfer has
// overwritten the lease, this transfer's resume must leave it in place —
// clearing it would expose the still-running transfer to the tuner.
func TestPauseTuningResumeLeavesForeignStamp(t *testing.T) {
	clientset := fake.NewSimpleClientset(&appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "mongodb", Namespace: "blog-test"},
	})

	resume, err := pauseTuning(context.Background(), clientset, "blog-test", "mongodb")
	if err != nil {
		t.Fatalf("pauseTuning: %v", err)
	}

	// A second, later transfer overwrites the stamp with its own lease.
	foreign := time.Now().Add(tuningPauseLease + time.Minute).UTC().Format(time.RFC3339)
	sts, _ := clientset.AppsV1().StatefulSets("blog-test").Get(context.Background(), "mongodb", metav1.GetOptions{})
	sts.Annotations[labels.AnnoTuningPausedUntil] = foreign
	if _, err := clientset.AppsV1().StatefulSets("blog-test").Update(context.Background(), sts, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("simulating the overlapping transfer: %v", err)
	}

	resume()
	sts, err = clientset.AppsV1().StatefulSets("blog-test").Get(context.Background(), "mongodb", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting statefulset after resume: %v", err)
	}
	if got := sts.Annotations[labels.AnnoTuningPausedUntil]; got != foreign {
		t.Errorf("resume cleared a foreign stamp: annotation = %q, want %q", got, foreign)
	}
}

// TestPauseTuningMissingStatefulSet: no StatefulSet means nothing the tuner
// would restart, so the pause is a clean no-op rather than an error.
func TestPauseTuningMissingStatefulSet(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	resume, err := pauseTuning(context.Background(), clientset, "blog-test", "ghost")
	if err != nil {
		t.Fatalf("expected a no-op for a missing statefulset, got %v", err)
	}
	resume()
}
