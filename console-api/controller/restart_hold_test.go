package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func webPod(name string, now, started time.Time, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "shop", CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
			Labels: map[string]string{"app.kubernetes.io/managed-by": "kipper", "app": "web"},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{runningSince(started, restarts)}},
	}
}

func hasAlertAction(entries []ResourceLogEntry, action string) bool {
	for _, e := range alertable(entries) {
		if e.Action == action {
			return true
		}
	}
	return false
}

func TestScalingRightAfterARestartIsLoggedButNotAlerted(t *testing.T) {
	now := time.Now()
	client := fake.NewClientset(testHPA(2, 2, 5, nil), webPod("web-a", now, now.Add(-time.Minute), 1))
	rc := NewResourceController(client, nil)
	checkHPAScalingWithin(t, rc)

	rc.refreshRestarts(context.Background(), now)
	setHPA(t, client, testHPA(5, 2, 5, &now))
	entries := checkHPAScalingWithin(t, rc)

	if !hasAction(entries, "scaled out (2 → 5 pods)") {
		t.Fatalf("the scale-out is missing from the resource log: %v", entries)
	}
	if hasAlertAction(entries, "scaled out (2 → 5 pods)") {
		t.Fatal("a scale-out caused by start-up CPU was sent as an alert")
	}
}

func TestScalingWithoutARestartIsAlerted(t *testing.T) {
	now := time.Now()
	client := fake.NewClientset(testHPA(2, 2, 5, nil), webPod("web-new", now, now.Add(-time.Minute), 0))
	rc := NewResourceController(client, nil)
	checkHPAScalingWithin(t, rc)

	rc.refreshRestarts(context.Background(), now)
	setHPA(t, client, testHPA(5, 2, 5, &now))
	if entries := checkHPAScalingWithin(t, rc); !hasAlertAction(entries, "scaled out (2 → 5 pods)") {
		t.Fatalf("a scale-out with new pods only was held back: %v", entries)
	}
}

func TestFirstSightScaleRightAfterARestartIsNotAlerted(t *testing.T) {
	now := time.Now()
	scaled := now.Add(-30 * time.Second)
	client := fake.NewClientset(testHPA(5, 2, 5, &scaled), webPod("web-a", now, now.Add(-time.Minute), 1))
	rc := NewResourceController(client, testCRClient())
	rc.refreshRestarts(context.Background(), now)

	entries := checkHPAScalingWithin(t, rc)
	if !hasAction(entries, "scaled out from the minimum (2 → 5 pods)") {
		t.Fatalf("the first-sight scale is missing from the resource log: %v", entries)
	}
	if len(alertable(entries)) != 0 {
		t.Fatalf("a first-sight scale right after a restart was alerted: %v", alertable(entries))
	}
}

func TestAtMaximumWaitsUntilARestartHasSettled(t *testing.T) {
	now := time.Now()
	client := fake.NewClientset(testHPA(5, 2, 5, &now, limitedByMax), webPod("web-a", now, now.Add(-2*time.Minute), 1))
	rc := NewResourceController(client, nil)
	rc.refreshRestarts(context.Background(), now)

	if due := rc.atMaximumEpisodes(context.Background(), now, now.UTC().Format(time.RFC3339)); len(due) != 0 {
		t.Fatalf("at maximum was alerted two minutes after a restart: %v", due)
	}
	later := now.Add(restartAlertHold)
	if due := rc.atMaximumEpisodes(context.Background(), later, later.UTC().Format(time.RFC3339)); len(due) != 1 {
		t.Fatalf("an app still at its maximum after the hold was not alerted, got %d episodes", len(due))
	}
}

func TestAtMaximumWithNewPodsIsAlertedAtOnce(t *testing.T) {
	now := time.Now()
	client := fake.NewClientset(testHPA(5, 2, 5, &now, limitedByMax), webPod("web-new", now, now.Add(-2*time.Minute), 0))
	rc := NewResourceController(client, nil)
	rc.refreshRestarts(context.Background(), now)

	if due := rc.atMaximumEpisodes(context.Background(), now, now.UTC().Format(time.RFC3339)); len(due) != 1 {
		t.Fatalf("at maximum with new pods was held back, got %d episodes", len(due))
	}
}

func TestTheRestartHoldOutlivesThePodThatRestarted(t *testing.T) {
	now := time.Now()
	client := fake.NewClientset(webPod("web-a", now, now.Add(-time.Minute), 1))
	rc := NewResourceController(client, nil)
	rc.refreshRestarts(context.Background(), now)

	if err := client.CoreV1().Pods("shop").Delete(context.Background(), "web-a", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Pods("shop").Create(context.Background(), webPod("web-new", now, now, 0), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	next := now.Add(time.Minute)
	rc.refreshRestarts(context.Background(), next)

	if !rc.restartHeld("shop", "web", next) {
		t.Fatal("the hold ended early when the restarted pod went away")
	}
	after := now.Add(-time.Minute).Add(restartAlertHold)
	rc.refreshRestarts(context.Background(), after)
	if rc.restartHeld("shop", "web", after) {
		t.Fatal("the hold outlived its ten minutes")
	}
}
