package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/handlers"
)

var managedLabels = map[string]string{"app.kubernetes.io/managed-by": "kipper"}

func testHPA(current, min, max int32, lastScale *time.Time, conditions ...autoscalingv2.HorizontalPodAutoscalerCondition) *autoscalingv2.HorizontalPodAutoscaler {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop", Labels: managedLabels},
		Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: ptr.To(min), MaxReplicas: max},
		Status:     autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: current, Conditions: conditions},
	}
	if lastScale != nil {
		hpa.Status.LastScaleTime = &metav1.Time{Time: *lastScale}
	}
	return hpa
}

var limitedByMax = autoscalingv2.HorizontalPodAutoscalerCondition{
	Type: autoscalingv2.ScalingLimited, Status: corev1.ConditionTrue, Reason: "TooManyReplicas",
	Message: "the desired replica count is more than the maximum replica count",
}

func setHPA(t *testing.T, client *fake.Clientset, hpa *autoscalingv2.HorizontalPodAutoscaler) {
	t.Helper()
	if _, err := client.AutoscalingV2().HorizontalPodAutoscalers(hpa.Namespace).Update(context.Background(), hpa, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// checkHPAScalingWithin fails the test instead of hanging it when the check
// never returns.
func checkHPAScalingWithin(t *testing.T, rc *ResourceController) []ResourceLogEntry {
	t.Helper()
	done := make(chan []ResourceLogEntry, 1)
	go func() { done <- rc.checkHPAScaling(context.Background()) }()
	select {
	case entries := <-done:
		return entries
	case <-time.After(5 * time.Second):
		t.Fatal("checkHPAScaling did not return")
		return nil
	}
}

func TestCheckHPAScalingRecordsChangesAsInfo(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	client := fake.NewClientset(testHPA(1, 1, 5, &old))
	rc := NewResourceController(client, nil)

	if entries := checkHPAScalingWithin(t, rc); len(entries) != 0 {
		t.Fatalf("a first look at an HPA that scaled long ago records nothing, got %+v", entries)
	}

	setHPA(t, client, testHPA(3, 1, 5, &old))
	entries := checkHPAScalingWithin(t, rc)
	if len(entries) != 1 || entries[0].Action != "scaled out (1 → 3 pods)" || entries[0].Severity != "info" || entries[0].Reason != handlers.HPAScaleReason {
		t.Fatalf("expected one info scale-out entry, got %+v", entries)
	}

	setHPA(t, client, testHPA(2, 1, 5, &old))
	entries = checkHPAScalingWithin(t, rc)
	if len(entries) != 1 || entries[0].Action != "scaled in (3 → 2 pods)" || entries[0].Severity != "info" {
		t.Fatalf("expected one info scale-in entry, got %+v", entries)
	}

	setHPA(t, client, testHPA(1, 1, 5, &old))
	entries = checkHPAScalingWithin(t, rc)
	if len(entries) != 1 || entries[0].Action != "scaled in (2 → 1 pod)" {
		t.Fatalf("expected a scale-in to a single pod, got %+v", entries)
	}
}

func TestCheckHPAScalingTreatsARecreatedAutoscalerAsNew(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	client := fake.NewClientset(testHPA(1, 1, 5, &old))
	rc := NewResourceController(client, nil)
	checkHPAScalingWithin(t, rc)

	if err := client.AutoscalingV2().HorizontalPodAutoscalers("shop").Delete(context.Background(), "web", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	checkHPAScalingWithin(t, rc)
	if _, err := client.AutoscalingV2().HorizontalPodAutoscalers("shop").Create(context.Background(), testHPA(3, 1, 5, &old), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if entries := checkHPAScalingWithin(t, rc); len(entries) != 0 {
		t.Fatalf("a recreated autoscaler starts from a first look, not from the old count, got %+v", entries)
	}
}

func TestCheckHPAScalingTreatsAReplacedAutoscalerAsNew(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	first := testHPA(1, 1, 5, &old)
	first.UID = "first"
	client := fake.NewClientset(first)
	rc := NewResourceController(client, nil)
	checkHPAScalingWithin(t, rc)

	second := testHPA(3, 1, 5, &old)
	second.UID = "second"
	replaceHPA(t, client, second)
	if entries := checkHPAScalingWithin(t, rc); len(entries) != 0 {
		t.Fatalf("a replaced autoscaler starts from a first look, not from the old count, got %+v", entries)
	}
}

func TestAlertSeverityTreatsScalingAsActivity(t *testing.T) {
	for _, action := range []string{"scaled out (1 → 3 pods)", "scaled in (3 → 1 pod)"} {
		if got := alertSeverity(action); got != "info" {
			t.Errorf("alertSeverity(%q) = %q, want info", action, got)
		}
	}
}

func TestCheckHPAScalingFirstSightRecordsARecentScaleFromTheMinimum(t *testing.T) {
	recent := time.Now().Add(-30 * time.Second).Truncate(time.Second)
	rc := NewResourceController(fake.NewClientset(testHPA(3, 1, 5, &recent)), nil)

	entries := checkHPAScalingWithin(t, rc)
	if len(entries) != 1 {
		t.Fatalf("expected the recent scale to be recorded, got %+v", entries)
	}
	e := entries[0]
	if e.Action != "scaled out from the minimum (1 → 3 pods)" || e.From != "1" || e.To != "3" || e.Severity != "info" || e.Reason != handlers.HPAScaleReason {
		t.Fatalf("unexpected entry %+v", e)
	}
	if e.Time != recent.UTC().Format(time.RFC3339) {
		t.Fatalf("the entry carries the autoscaler's scale time %s, got %s", recent.UTC().Format(time.RFC3339), e.Time)
	}
	if again := checkHPAScalingWithin(t, rc); len(again) != 0 {
		t.Fatalf("the scale is recorded once, got %+v", again)
	}
}

func TestCheckHPAScalingFirstSightStaysSilent(t *testing.T) {
	recent := time.Now().Add(-30 * time.Second)
	old := time.Now().Add(-time.Hour)
	cases := map[string]*autoscalingv2.HorizontalPodAutoscaler{
		"scaled long ago": testHPA(3, 1, 5, &old),
		"at the minimum":  testHPA(2, 2, 5, &recent),
		"never scaled":    testHPA(3, 1, 5, nil),
		"minimum left unset": func() *autoscalingv2.HorizontalPodAutoscaler {
			h := testHPA(1, 1, 5, &recent)
			h.Spec.MinReplicas = nil
			return h
		}(),
		"scaled to the floor": testHPA(1, 1, 5, &recent),
	}
	for name, hpa := range cases {
		t.Run(name, func(t *testing.T) {
			rc := NewResourceController(fake.NewClientset(hpa), nil)
			if entries := checkHPAScalingWithin(t, rc); len(entries) != 0 {
				t.Fatalf("expected a silent first look, got %+v", entries)
			}
		})
	}
}

func resourceLog(t *testing.T, entries ...ResourceLogEntry) *corev1.ConfigMap {
	t.Helper()
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: resourceLogConfigMap, Namespace: modeConfigMapNamespace},
		Data:       map[string]string{"entries": string(data)},
	}
}

// A restart empties the in-memory replica counts, and the resource log is what
// says that the previous leader already recorded the scale.
func TestCheckHPAScalingFirstSightDoesNotRepeatALoggedScale(t *testing.T) {
	scaledAt := time.Now().Add(-40 * time.Second).Truncate(time.Second)
	logged := ResourceLogEntry{
		Time: scaledAt.Add(10 * time.Second).UTC().Format(time.RFC3339), App: "web", Namespace: "shop",
		Action: "scaled out (1 → 3 pods)", Reason: handlers.HPAScaleReason,
	}
	rc := NewResourceController(fake.NewClientset(testHPA(3, 1, 5, &scaledAt), resourceLog(t, logged)), nil)
	if entries := checkHPAScalingWithin(t, rc); len(entries) != 0 {
		t.Fatalf("a scale the log already holds is not recorded again, got %+v", entries)
	}

	earlier := logged
	earlier.Time = scaledAt.Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	other := logged
	other.App = "api"
	rc = NewResourceController(fake.NewClientset(testHPA(3, 1, 5, &scaledAt), resourceLog(t, earlier, other)), nil)
	if entries := checkHPAScalingWithin(t, rc); len(entries) != 1 {
		t.Fatalf("an older entry, or another app's, does not cover this scale, got %+v", entries)
	}
}

func atMaxEntries(batches []alertBatch) []ResourceLogEntry {
	var out []ResourceLogEntry
	for _, e := range entriesOf(batches) {
		if e.Action == "autoscaling at maximum" {
			out = append(out, e)
		}
	}
	return out
}

func TestCheckAutoscalingWarnsOncePerEpisodeAtTheMaximum(t *testing.T) {
	scaledAt := time.Now().Add(-time.Minute)
	hpa := testHPA(3, 1, 3, &scaledAt, limitedByMax)
	hpa.Spec.Metrics = []autoscalingv2.MetricSpec{{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceMemory}}}
	client := fake.NewClientset(hpa)
	rc := NewResourceController(client, nil)
	ctx := context.Background()

	batches := rc.checkAutoscaling(ctx)
	entries := atMaxEntries(batches)
	if len(entries) != 1 {
		t.Fatalf("expected one at-maximum warning, got %+v", entriesOf(batches))
	}
	e := entries[0]
	if e.Severity != "warning" || e.App != "web" || e.Namespace != "shop" || e.Reason != "the app runs 3 pods, the maximum of 3, and the autoscaler wants more. Raise the maximum, or the memory request, when the load is expected" {
		t.Fatalf("unexpected entry %+v", e)
	}

	if again := atMaxEntries(rc.checkAutoscaling(ctx)); len(again) != 1 {
		t.Fatalf("an alert that was never stored stays due, got %+v", again)
	}
	rc.commitBatches(batches)
	if again := atMaxEntries(rc.checkAutoscaling(ctx)); len(again) != 0 {
		t.Fatalf("the same episode warns once, got %+v", again)
	}

	// Still at the maximum without the limit condition: the episode goes on.
	setHPA(t, client, testHPA(3, 1, 3, &scaledAt))
	if again := atMaxEntries(rc.checkAutoscaling(ctx)); len(again) != 0 {
		t.Fatalf("the episode lasts until the app leaves the maximum, got %+v", again)
	}
	setHPA(t, client, testHPA(3, 1, 3, &scaledAt, limitedByMax))
	if again := atMaxEntries(rc.checkAutoscaling(ctx)); len(again) != 0 {
		t.Fatalf("a limit returning at the same maximum is the same episode, got %+v", again)
	}

	setHPA(t, client, testHPA(2, 1, 3, &scaledAt))
	if again := atMaxEntries(rc.checkAutoscaling(ctx)); len(again) != 0 {
		t.Fatalf("below the maximum there is nothing to say, got %+v", again)
	}
	later := time.Now()
	setHPA(t, client, testHPA(3, 1, 3, &later, limitedByMax))
	if again := atMaxEntries(rc.checkAutoscaling(ctx)); len(again) != 1 {
		t.Fatalf("a new episode warns again, got %+v", again)
	}
}

func TestCheckAutoscalingIgnoresAMaximumThatDoesNotLimit(t *testing.T) {
	scaledAt := time.Now()
	fewer := autoscalingv2.HorizontalPodAutoscalerCondition{Type: autoscalingv2.ScalingLimited, Status: corev1.ConditionTrue, Reason: "TooFewReplicas"}
	notLimited := limitedByMax
	notLimited.Status = corev1.ConditionFalse
	cases := map[string]*autoscalingv2.HorizontalPodAutoscaler{
		"no condition":           testHPA(3, 1, 3, &scaledAt),
		"limited by the minimum": testHPA(3, 3, 3, &scaledAt, fewer),
		"condition false":        testHPA(3, 1, 3, &scaledAt, notLimited),
		"below the maximum":      testHPA(2, 1, 3, &scaledAt, limitedByMax),
		"unmanaged autoscaler": func() *autoscalingv2.HorizontalPodAutoscaler {
			h := testHPA(3, 1, 3, &scaledAt, limitedByMax)
			h.Labels = nil
			return h
		}(),
	}
	for name, hpa := range cases {
		t.Run(name, func(t *testing.T) {
			rc := NewResourceController(fake.NewClientset(hpa), nil)
			if entries := atMaxEntries(rc.checkAutoscaling(context.Background())); len(entries) != 0 {
				t.Fatalf("expected no warning, got %+v", entries)
			}
		})
	}
}

func alertStore(t *testing.T, alerts ...handlers.Alert) *corev1.ConfigMap {
	t.Helper()
	data, err := json.Marshal(alerts)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-alerts", Namespace: "kipper-system"},
		Data:       map[string]string{"alerts": string(data)},
	}
}

// After a restart the in-memory marks are empty, and the stored alerts say
// which episodes the previous leader already reported.
func TestCheckAutoscalingDoesNotRepeatAStoredAtMaximumAlert(t *testing.T) {
	scaledAt := time.Now().Add(-10 * time.Minute)
	stored := handlers.Alert{
		Time: scaledAt.Add(time.Minute).UTC().Format(time.RFC3339), App: "web", Namespace: "shop",
		Action: "autoscaling at maximum", Severity: "warning",
	}
	client := fake.NewClientset(testHPA(3, 1, 3, &scaledAt, limitedByMax), alertStore(t, stored))
	rc := NewResourceController(client, nil)
	if entries := atMaxEntries(rc.checkAutoscaling(context.Background())); len(entries) != 0 {
		t.Fatalf("an episode the store already holds is not reported again, got %+v", entries)
	}
	// The store keeps only the newest alerts, so the episode must stay marked
	// after its alert has been pushed out.
	if err := client.CoreV1().ConfigMaps("kipper-system").Delete(context.Background(), "kipper-alerts", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if entries := atMaxEntries(rc.checkAutoscaling(context.Background())); len(entries) != 0 {
		t.Fatalf("the stored alert marks the episode as reported, got %+v", entries)
	}

	earlier := stored
	earlier.Time = scaledAt.Add(-time.Hour).UTC().Format(time.RFC3339)
	rc = NewResourceController(fake.NewClientset(testHPA(3, 1, 3, &scaledAt, limitedByMax), alertStore(t, earlier)), nil)
	if entries := atMaxEntries(rc.checkAutoscaling(context.Background())); len(entries) != 1 {
		t.Fatalf("an alert from an earlier episode does not cover this one, got %+v", entries)
	}
}

// The stored match carries no autoscaler identity, so the episode must start no
// earlier than the autoscaler it belongs to.
func TestCheckAutoscalingDoesNotLetAnOlderAutoscalersAlertCoverANewOne(t *testing.T) {
	stored := handlers.Alert{
		Time: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339), App: "web", Namespace: "shop",
		Action: "autoscaling at maximum", Severity: "warning",
	}
	hpa := testHPA(3, 1, 3, nil, limitedByMax)
	hpa.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
	rc := NewResourceController(fake.NewClientset(hpa, alertStore(t, stored)), nil)
	if entries := atMaxEntries(rc.checkAutoscaling(context.Background())); len(entries) != 1 {
		t.Fatalf("an alert from before the autoscaler existed does not cover it, got %+v", entries)
	}
}

// Leaving the maximum ends the episode even when no scale follows, for example
// when the maximum is raised and then lowered again.
func TestCheckAutoscalingStartsANewEpisodeAfterLeavingTheMaximumWithoutAScale(t *testing.T) {
	scaledAt := time.Now().Add(-10 * time.Minute)
	stored := handlers.Alert{
		Time: scaledAt.Add(time.Minute).UTC().Format(time.RFC3339), App: "web", Namespace: "shop",
		Action: "autoscaling at maximum", Severity: "warning",
	}
	client := fake.NewClientset(testHPA(3, 1, 3, &scaledAt, limitedByMax), alertStore(t, stored))
	rc := NewResourceController(client, nil)
	ctx := context.Background()
	if entries := atMaxEntries(rc.checkAutoscaling(ctx)); len(entries) != 0 {
		t.Fatalf("the stored alert covers the episode it was raised for, got %+v", entries)
	}

	setHPA(t, client, testHPA(3, 1, 4, &scaledAt))
	rc.checkAutoscaling(ctx)
	setHPA(t, client, testHPA(3, 1, 3, &scaledAt, limitedByMax))
	if entries := atMaxEntries(rc.checkAutoscaling(ctx)); len(entries) != 1 {
		t.Fatalf("back at a lowered maximum is a new episode, got %+v", entries)
	}
}

// The autoscaler's own condition is the evidence that survives a restart: it
// changes its transition time when the limit lifts and returns.
func TestCheckAutoscalingStartsANewEpisodeAfterARestartBetweenLeavingAndReturning(t *testing.T) {
	scaledAt := time.Now().Add(-10 * time.Minute)
	stored := handlers.Alert{
		Time: scaledAt.Add(time.Minute).UTC().Format(time.RFC3339), App: "web", Namespace: "shop",
		Action: "autoscaling at maximum", Severity: "warning",
	}
	limitedAgain := limitedByMax
	limitedAgain.LastTransitionTime = metav1.NewTime(time.Now().Add(-30 * time.Second))
	client := fake.NewClientset(testHPA(3, 1, 3, &scaledAt, limitedAgain), alertStore(t, stored))

	rc := NewResourceController(client, nil)
	if entries := atMaxEntries(rc.checkAutoscaling(context.Background())); len(entries) != 1 {
		t.Fatalf("a limit that returned after the stored alert is a new episode, got %+v", entries)
	}
}

func replaceHPA(t *testing.T, client *fake.Clientset, hpa *autoscalingv2.HorizontalPodAutoscaler) {
	t.Helper()
	ctx := context.Background()
	if err := client.AutoscalingV2().HorizontalPodAutoscalers(hpa.Namespace).Delete(ctx, hpa.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AutoscalingV2().HorizontalPodAutoscalers(hpa.Namespace).Create(ctx, hpa, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// Switching autoscaling off and on between two checks replaces the autoscaler
// under the same name.
func TestCheckAutoscalingTreatsAReplacedAutoscalerAsANewEpisode(t *testing.T) {
	scaledAt := time.Now().Add(-time.Minute)
	first := testHPA(3, 1, 3, &scaledAt, limitedByMax)
	first.UID = "first"
	client := fake.NewClientset(first)
	rc := NewResourceController(client, nil)
	rc.commitBatches(rc.checkAutoscaling(context.Background()))

	second := testHPA(3, 1, 3, &scaledAt, limitedByMax)
	second.UID = "second"
	replaceHPA(t, client, second)
	if entries := atMaxEntries(rc.checkAutoscaling(context.Background())); len(entries) != 1 {
		t.Fatalf("a replaced autoscaler at its maximum is a new episode, got %+v", entries)
	}
}

func appWithReadiness(status metav1.ConditionStatus, reason, message string, at time.Time) *kipperv1.App {
	app := &kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"}}
	if reason != "" {
		app.Status.Conditions = []metav1.Condition{{
			Type: kipperv1.ConditionAutoscalingReady, Status: status, Reason: reason, Message: message,
			LastTransitionTime: metav1.Time{Time: at},
		}}
	}
	return app
}

func notReadyEntries(batches []alertBatch) []ResourceLogEntry {
	var out []ResourceLogEntry
	for _, e := range entriesOf(batches) {
		if e.Action == "autoscaling not ready" {
			out = append(out, e)
		}
	}
	return out
}

func testCRClientWithStatus(objs ...crclient.Object) crclient.Client {
	return crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).WithStatusSubresource(&kipperv1.App{}).Build()
}

func setApp(t *testing.T, rc *ResourceController, app *kipperv1.App) {
	t.Helper()
	var current kipperv1.App
	if err := rc.crClient.Get(context.Background(), crclient.ObjectKeyFromObject(app), &current); err != nil {
		t.Fatal(err)
	}
	current.Status = app.Status
	if err := rc.crClient.Status().Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
}

func TestCheckAutoscalingWarnsOncePerNotReadyReason(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	app := appWithReadiness(metav1.ConditionFalse, "AutoscalerReconcileFailed", "the autoscaler could not be written: denied", at)
	rc := NewResourceController(fake.NewClientset(), testCRClientWithStatus(app))
	ctx := context.Background()

	batches := rc.checkAutoscaling(ctx)
	entries := notReadyEntries(batches)
	if len(entries) != 1 {
		t.Fatalf("expected one not-ready warning, got %+v", entriesOf(batches))
	}
	if e := entries[0]; e.Severity != "warning" || e.Reason != "AutoscalerReconcileFailed: the autoscaler could not be written: denied" {
		t.Fatalf("unexpected entry %+v", e)
	}
	if again := notReadyEntries(rc.checkAutoscaling(ctx)); len(again) != 1 {
		t.Fatalf("an alert that was never stored stays due, got %+v", again)
	}
	rc.commitBatches(batches)
	if again := notReadyEntries(rc.checkAutoscaling(ctx)); len(again) != 0 {
		t.Fatalf("the same reason warns once, got %+v", again)
	}

	setApp(t, rc, appWithReadiness(metav1.ConditionFalse, "AutoscalerReconcileFailed", "the autoscaler could not be written: timeout", at))
	if again := notReadyEntries(rc.checkAutoscaling(ctx)); len(again) != 0 {
		t.Fatalf("a new message under the same reason is the same episode, got %+v", again)
	}

	setApp(t, rc, appWithReadiness(metav1.ConditionFalse, "InvalidPolicy", "the autoscaling policy is not applied: no maximum", at))
	batches = rc.checkAutoscaling(ctx)
	if again := notReadyEntries(batches); len(again) != 1 || !strings.HasPrefix(again[0].Reason, "InvalidPolicy: ") {
		t.Fatalf("a new reason warns again, got %+v", again)
	}
	rc.commitBatches(batches)

	setApp(t, rc, appWithReadiness(metav1.ConditionTrue, "PolicyApplied", "ok", at))
	if again := notReadyEntries(rc.checkAutoscaling(ctx)); len(again) != 0 {
		t.Fatalf("a ready app warns about nothing, got %+v", again)
	}
	setApp(t, rc, appWithReadiness(metav1.ConditionFalse, "InvalidPolicy", "the autoscaling policy is not applied: no maximum", time.Now()))
	batches = rc.checkAutoscaling(ctx)
	if again := notReadyEntries(batches); len(again) != 1 {
		t.Fatalf("recovery ends the episode, so the reason warns again, got %+v", again)
	}
	rc.commitBatches(batches)

	setApp(t, rc, appWithReadiness("", "", "", at))
	if again := notReadyEntries(rc.checkAutoscaling(ctx)); len(again) != 0 {
		t.Fatalf("a removed condition warns about nothing, got %+v", again)
	}
	setApp(t, rc, appWithReadiness(metav1.ConditionFalse, "InvalidPolicy", "the autoscaling policy is not applied: no maximum", time.Now()))
	if again := notReadyEntries(rc.checkAutoscaling(ctx)); len(again) != 1 {
		t.Fatalf("removing the condition ends the episode, got %+v", again)
	}
}

func TestCheckAutoscalingTreatsAReplacedAppAsANewReadinessEpisode(t *testing.T) {
	first := appWithReadiness(metav1.ConditionFalse, "AutoscalerReconcileFailed", "denied", time.Now().Add(-time.Hour))
	first.UID = "first"
	rc := NewResourceController(fake.NewClientset(), testCRClientWithStatus(first))
	ctx := context.Background()
	rc.commitBatches(rc.checkAutoscaling(ctx))

	if err := rc.crClient.Delete(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := appWithReadiness(metav1.ConditionFalse, "AutoscalerReconcileFailed", "denied", time.Now())
	second.UID = "second"
	if err := rc.crClient.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	setApp(t, rc, second)
	if entries := notReadyEntries(rc.checkAutoscaling(ctx)); len(entries) != 1 {
		t.Fatalf("a replaced App with the same reason is a new episode, got %+v", entries)
	}
}

func TestCheckAutoscalingDoesNotRepeatAStoredNotReadyAlert(t *testing.T) {
	at := time.Now().Add(-10 * time.Minute)
	app := appWithReadiness(metav1.ConditionFalse, "InvalidPolicy", "the autoscaling policy is not applied: no maximum", at)
	stored := handlers.Alert{
		Time: at.Add(time.Minute).UTC().Format(time.RFC3339), App: "web", Namespace: "shop",
		Action: "autoscaling not ready", Severity: "warning", Reason: "InvalidPolicy: the autoscaling policy is not applied: no maximum",
	}
	rc := NewResourceController(fake.NewClientset(alertStore(t, stored)), testCRClientWithStatus(app))
	if entries := notReadyEntries(rc.checkAutoscaling(context.Background())); len(entries) != 0 {
		t.Fatalf("an episode the store already holds is not reported again, got %+v", entries)
	}

	otherReason := stored
	otherReason.Reason = "AutoscalerReconcileFailed: denied"
	rc = NewResourceController(fake.NewClientset(alertStore(t, otherReason)), testCRClientWithStatus(app))
	if entries := notReadyEntries(rc.checkAutoscaling(context.Background())); len(entries) != 1 {
		t.Fatalf("an alert for another reason does not cover this one, got %+v", entries)
	}
}

// The autoscaling warnings run with the failure alerts, so they reach the
// store in expert mode too.
func TestTickStoresAutoscalingWarningsInExpertMode(t *testing.T) {
	scaledAt := time.Now()
	objs := []runtime.Object{
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "kipper-mode", Namespace: "kipper-system"},
			Data:       map[string]string{"mode": "expert"},
		},
		testHPA(3, 1, 3, &scaledAt, limitedByMax),
	}
	client := fake.NewClientset(objs...)
	rc := NewResourceController(client, nil)

	rc.tick(context.Background())

	cm, err := client.CoreV1().ConfigMaps("kipper-system").Get(context.Background(), "kipper-alerts", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected the alert store to be written: %v", err)
	}
	var alerts []handlers.Alert
	if err := json.Unmarshal([]byte(cm.Data["alerts"]), &alerts); err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].Action != "autoscaling at maximum" || alerts[0].Severity != "warning" {
		t.Fatalf("expected the at-maximum warning in the store, got %+v", alerts)
	}
	rc.tick(context.Background())
	cm, _ = client.CoreV1().ConfigMaps("kipper-system").Get(context.Background(), "kipper-alerts", metav1.GetOptions{})
	_ = json.Unmarshal([]byte(cm.Data["alerts"]), &alerts)
	if len(alerts) != 1 {
		t.Fatalf("the next tick adds nothing for the same episode, got %+v", alerts)
	}
}

func TestAtMaximumAdviceNamesTheTrackedMetrics(t *testing.T) {
	metric := func(name corev1.ResourceName) autoscalingv2.MetricSpec {
		return autoscalingv2.MetricSpec{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: name}}
	}
	for _, tc := range []struct {
		name    string
		metrics []autoscalingv2.MetricSpec
		want    string
	}{
		{"cpu", []autoscalingv2.MetricSpec{metric(corev1.ResourceCPU)}, "Raise the maximum, or the CPU request, when the load is expected"},
		{"memory", []autoscalingv2.MetricSpec{metric(corev1.ResourceMemory)}, "Raise the maximum, or the memory request, when the load is expected"},
		{"both", []autoscalingv2.MetricSpec{metric(corev1.ResourceCPU), metric(corev1.ResourceMemory)}, "Raise the maximum, or the CPU and memory requests, when the load is expected"},
		{"none", nil, "Raise the maximum when the load is expected"},
	} {
		hpa := testHPA(3, 1, 3, nil)
		hpa.Spec.Metrics = tc.metrics
		if got := atMaximumAdvice(hpa); got != tc.want {
			t.Errorf("%s: atMaximumAdvice = %q, want %q", tc.name, got, tc.want)
		}
	}
}
