package controllers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func transferFixture(kind string) *kipperv1.DataTransfer {
	return &kipperv1.DataTransfer{
		ObjectMeta: metav1.ObjectMeta{Name: "abc12345-shop-prod-uploads", Namespace: "shop-prod"},
		Spec: kipperv1.DataTransferSpec{
			SessionID:     "abc12345",
			Kind:          kind,
			Source:        kipperv1.DataTransferEndpoint{Volume: "uploads", Service: "files"},
			Target:        kipperv1.DataTransferEndpoint{Volume: "uploads", Service: "files"},
			TargetBaseURL: "https://console-api.203-0-113-10.kipper.run/api/v1/migrate-target/abc12345/transfer/abc12345-shop-prod-uploads",
		},
	}
}

func transferTokenSecret(dt *kipperv1.DataTransfer) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tokenSecretName(dt), Namespace: dt.Namespace},
		Data:       map[string][]byte{"token": []byte("deadbeef")},
	}
}

func reconcileTransfer(t *testing.T, r *DataTransferReconciler, dt *kipperv1.DataTransfer) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: dt.Name, Namespace: dt.Namespace}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// Without the orchestrator-provided token secret the transfer must wait in
// Pending instead of launching an unauthenticatable mover.
func TestReconcileWaitsForTokenSecret(t *testing.T) {
	dt := transferFixture("volume")
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.DataTransfer{}).WithObjects(dt).Build()
	r := &DataTransferReconciler{Client: c, Scheme: testScheme(), DatamoverImage: "ghcr.io/getkipper/kipper-datamover:latest"}

	reconcileTransfer(t, r, dt)
	// Finalizer update happens first; run again to reach phase handling.
	reconcileTransfer(t, r, dt)

	var got kipperv1.DataTransfer
	if err := c.Get(context.Background(), types.NamespacedName{Name: dt.Name, Namespace: dt.Namespace}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != "Pending" {
		t.Fatalf("phase = %q, want Pending", got.Status.Phase)
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Name: moverPodName(dt), Namespace: dt.Namespace}, &pod); err == nil {
		t.Fatal("mover pod must not exist before the token secret does")
	}
}

// With the token secret in place the reconciler creates the export mover
// with the right mount, no service-account token, and the transfer args.
func TestReconcileCreatesMoverPod(t *testing.T) {
	for _, tc := range []struct {
		kind      string
		wantClaim string
	}{
		{"volume", "shared-uploads"},
		{"servicePVC", "data-files-0"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			dt := transferFixture(tc.kind)
			c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.DataTransfer{}).
				WithObjects(dt, transferTokenSecret(dt)).Build()
			r := &DataTransferReconciler{Client: c, Scheme: testScheme(), DatamoverImage: "ghcr.io/getkipper/kipper-datamover:latest"}

			reconcileTransfer(t, r, dt)
			reconcileTransfer(t, r, dt)

			var pod corev1.Pod
			if err := c.Get(context.Background(), types.NamespacedName{Name: moverPodName(dt), Namespace: dt.Namespace}, &pod); err != nil {
				t.Fatalf("mover pod not created: %v", err)
			}
			if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
				t.Fatal("mover pod must not mount a service-account token")
			}
			claim := ""
			for _, v := range pod.Spec.Volumes {
				if v.PersistentVolumeClaim != nil {
					claim = v.PersistentVolumeClaim.ClaimName
					if !v.PersistentVolumeClaim.ReadOnly {
						t.Fatal("export mount must be read-only")
					}
				}
			}
			if claim != tc.wantClaim {
				t.Fatalf("claim = %q, want %q", claim, tc.wantClaim)
			}
		})
	}
}

// A failed mover retries with a fresh pod until attempts are exhausted,
// then the transfer fails terminally.
func TestReconcileRetriesThenFails(t *testing.T) {
	dt := transferFixture("volume")
	dt.Spec.MaxAttempts = 2
	failedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: moverPodName(dt), Namespace: dt.Namespace},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "verify mismatch"}},
			}},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.DataTransfer{}).
		WithObjects(dt, transferTokenSecret(dt), failedPod).Build()
	r := &DataTransferReconciler{Client: c, Scheme: testScheme(), DatamoverImage: "img"}

	// First failure: the finalizer lands, the failed pod is torn down, and
	// the attempt counter moves to 1.
	reconcileTransfer(t, r, dt)

	var got kipperv1.DataTransfer
	_ = c.Get(context.Background(), types.NamespacedName{Name: dt.Name, Namespace: dt.Namespace}, &got)
	if got.Status.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1", got.Status.Attempt)
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Name: moverPodName(dt), Namespace: dt.Namespace}, &pod); err == nil {
		t.Fatal("failed pod must be deleted for the retry")
	}

	// Each further failure burns one attempt; at MaxAttempts the transfer
	// goes terminal instead of retrying again.
	failedPod.ResourceVersion = ""
	if err := c.Create(context.Background(), failedPod); err != nil {
		t.Fatal(err)
	}
	reconcileTransfer(t, r, dt)
	_ = c.Get(context.Background(), types.NamespacedName{Name: dt.Name, Namespace: dt.Namespace}, &got)
	if got.Status.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2", got.Status.Attempt)
	}

	failedPod.ResourceVersion = ""
	if err := c.Create(context.Background(), failedPod); err != nil {
		t.Fatal(err)
	}
	reconcileTransfer(t, r, dt)

	_ = c.Get(context.Background(), types.NamespacedName{Name: dt.Name, Namespace: dt.Namespace}, &got)
	if got.Status.Phase != "Failed" {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if got.Status.LastError == "" {
		t.Fatal("terminal failure must carry the mover's error")
	}
}

// A succeeded mover completes the transfer and stamps LastSyncedAt for the
// future syncing phase's freshness gate.
func TestReconcileCompletesOnSuccess(t *testing.T) {
	dt := transferFixture("volume")
	donePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: moverPodName(dt), Namespace: dt.Namespace},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.DataTransfer{}).
		WithObjects(dt, transferTokenSecret(dt), donePod).Build()
	r := &DataTransferReconciler{Client: c, Scheme: testScheme(), DatamoverImage: "img"}

	reconcileTransfer(t, r, dt)
	reconcileTransfer(t, r, dt)

	var got kipperv1.DataTransfer
	_ = c.Get(context.Background(), types.NamespacedName{Name: dt.Name, Namespace: dt.Namespace}, &got)
	if got.Status.Phase != "Completed" {
		t.Fatalf("phase = %q, want Completed", got.Status.Phase)
	}
	if got.Status.LastSyncedAt == nil {
		t.Fatal("completion must stamp LastSyncedAt")
	}
}

// Deleting the CR removes the source-side mover pod through the finalizer.
func TestReconcileCleanupOnDelete(t *testing.T) {
	dt := transferFixture("volume")
	dt.Finalizers = []string{dataTransferFinalizer}
	now := metav1.Now()
	dt.DeletionTimestamp = &now
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: moverPodName(dt), Namespace: dt.Namespace}}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&kipperv1.DataTransfer{}).
		WithObjects(dt, pod).Build()
	r := &DataTransferReconciler{Client: c, Scheme: testScheme(), DatamoverImage: "img"}

	reconcileTransfer(t, r, dt)

	var gone corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Name: moverPodName(dt), Namespace: dt.Namespace}, &gone); err == nil {
		t.Fatal("mover pod must be deleted with the CR")
	}
}

// Unsupported kinds must fail pod construction, not launch a mover with a
// half-built spec.
func TestBuildMoverPodRejectsUnknownKind(t *testing.T) {
	dt := transferFixture("dbDump")
	r := &DataTransferReconciler{DatamoverImage: "img"}
	if _, err := r.buildMoverPod(dt); err == nil {
		t.Fatal("dbDump has no mover path yet and must be rejected")
	}
}
