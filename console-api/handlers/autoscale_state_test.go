package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

var scaleTime = metav1.NewTime(time.Date(2026, 10, 4, 9, 30, 0, 0, time.UTC))

func countedDeployment(desired, ready int32) *appsv1.Deployment {
	dep := liveDeployment(desired)
	dep.Status = appsv1.DeploymentStatus{Replicas: desired, ReadyReplicas: ready}
	return dep
}

func scalingHPA() *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging"},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{
			CurrentReplicas: 4,
			DesiredReplicas: 4,
			LastScaleTime:   &scaleTime,
			Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{
				{Type: autoscalingv2.AbleToScale, Status: corev1.ConditionTrue, Reason: "ReadyForNewScale", Message: "recommended size matches current size"},
				{Type: autoscalingv2.ScalingActive, Status: corev1.ConditionTrue, Reason: "ValidMetricFound", Message: "the HPA was able to compute the replica count"},
				{Type: autoscalingv2.ScalingLimited, Status: corev1.ConditionTrue, Reason: "TooManyReplicas", Message: "the desired replica count is more than the maximum replica count"},
			},
		},
	}
}

// isNull reports whether the response carries key with a JSON null, which is
// how the handler says a value is unknown.
func isNull(resp map[string]any, key string) bool {
	v, ok := resp[key]
	return ok && v == nil
}

func TestAutoscaleGet_ReportsTheCapacityState(t *testing.T) {
	app := capacityApp(int32Ptr(3), onPolicy(2, 6))
	app.Status.Conditions = []metav1.Condition{{
		Type: kipperv1.ConditionAutoscalingReady, Status: metav1.ConditionTrue, Reason: "Reconciled", Message: "autoscaler in place",
	}}
	code, resp := serveAutoscale(t, &Autoscale{Client: fake.NewClientset(countedDeployment(4, 3), scalingHPA()), CRClient: testCRClient(app)}, "GET", "")

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", code, resp)
	}
	want := map[string]any{
		"enabled":             true,
		"replicas":            float64(3),
		"deployment_replicas": float64(4),
		"ready_replicas":      float64(3),
		"current_replicas":    float64(4),
		"last_scale_time":     "2026-10-04T09:30:00Z",
		"stopped":             false,
		"quota_blocked":       false,
		"capacity_api":        float64(1),
	}
	for key, value := range want {
		if resp[key] != value {
			t.Errorf("%s = %#v, want %#v", key, resp[key], value)
		}
	}
	conditions, _ := resp["conditions"].([]any)
	if len(conditions) != 3 {
		t.Fatalf("expected the three HPA conditions, got %v", resp["conditions"])
	}
	limited, _ := conditions[2].(map[string]any)
	if limited["type"] != "ScalingLimited" || limited["status"] != "True" || limited["reason"] != "TooManyReplicas" ||
		!strings.Contains(limited["message"].(string), "more than the maximum") {
		t.Errorf("ScalingLimited = %v, want its status, reason and message", limited)
	}
	ready, _ := resp["autoscaling_ready"].(map[string]any)
	if ready["status"] != "True" || ready["reason"] != "Reconciled" || ready["message"] != "autoscaler in place" {
		t.Errorf("autoscaling_ready = %v, want the App's condition", resp["autoscaling_ready"])
	}
}

// The console says which resources automatic sizing leaves alone, so it needs
// the metrics the controller treats as tracked, not the readings available.
func TestAutoscaleGet_ReportsTheTrackedMetrics(t *testing.T) {
	invalid := &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](4), MaxReplicas: ptr.To[int32](2), CPUTarget: ptr.To[int32](70)}
	memoryHPA := scalingHPA()
	memoryHPA.Spec.Metrics = []autoscalingv2.MetricSpec{{
		Type:     autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceMemory},
	}}
	forbidden := func() *fake.Clientset {
		client := fake.NewClientset()
		client.PrependReactor("get", "horizontalpodautoscalers", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "horizontalpodautoscalers"}, "web", errors.New("no"))
		})
		return client
	}
	tests := []struct {
		name   string
		as     *kipperv1.AppAutoscale
		client *fake.Clientset
		want   map[string]any
	}{
		{"a usable policy tracks its targets", onPolicy(1, 3), fake.NewClientset(scalingHPA()), map[string]any{"cpu": true, "memory": false}},
		{"an invalid policy reads the running autoscaler's metrics", invalid, fake.NewClientset(memoryHPA), map[string]any{"cpu": false, "memory": true}},
		{"an invalid policy without an autoscaler tracks nothing", invalid, fake.NewClientset(), map[string]any{"cpu": false, "memory": false}},
		{"an invalid policy whose autoscaler cannot be read counts both", invalid, forbidden(), map[string]any{"cpu": true, "memory": true}},
		{"a policy that is off tracks nothing", &kipperv1.AppAutoscale{Enabled: false, CPUTarget: ptr.To[int32](70)}, fake.NewClientset(scalingHPA()), map[string]any{"cpu": false, "memory": false}},
		{"no block tracks nothing", nil, fake.NewClientset(), map[string]any{"cpu": false, "memory": false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, resp := serveAutoscale(t, &Autoscale{Client: tt.client, CRClient: testCRClient(capacityApp(int32Ptr(2), tt.as))}, "GET", "")
			if code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %v", code, resp)
			}
			if !reflect.DeepEqual(resp["tracked"], tt.want) {
				t.Errorf("tracked = %#v, want %#v", resp["tracked"], tt.want)
			}
		})
	}
}

func TestAutoscaleGet_MissingDataIsUnknown(t *testing.T) {
	code, resp := serveAutoscale(t, &Autoscale{Client: fake.NewClientset(), CRClient: testCRClient(capacityApp(nil, onPolicy(2, 6)))}, "GET", "")

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", code, resp)
	}
	for _, key := range []string{"deployment_replicas", "running_replicas", "ready_replicas", "conditions", "last_scale_time", "quota_blocked", "autoscaling_ready"} {
		if !isNull(resp, key) {
			t.Errorf("%s = %#v, want null for unknown", key, resp[key])
		}
	}
	if resp["replicas"] != float64(1) {
		t.Errorf("replicas = %v, want 1 for an absent stored count", resp["replicas"])
	}
	if resp["stopped"] != false || resp["capacity_api"] != float64(1) {
		t.Errorf("stopped and capacity_api must still be known, got %v", resp)
	}
}

func TestAutoscaleGet_ReportsTheRunningPodsApartFromTheDesiredCount(t *testing.T) {
	dep := liveDeployment(4)
	dep.Status = appsv1.DeploymentStatus{Replicas: 5, ReadyReplicas: 3}
	code, resp := serveAutoscale(t, &Autoscale{Client: fake.NewClientset(dep), CRClient: testCRClient(capacityApp(int32Ptr(4), nil))}, "GET", "")

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", code, resp)
	}
	if resp["deployment_replicas"] != float64(4) || resp["running_replicas"] != float64(5) || resp["ready_replicas"] != float64(3) {
		t.Errorf("expected desired 4, running 5 and ready 3, got %v", resp)
	}
}

func TestAutoscaleGet_AStoppedAppReportsZeroPods(t *testing.T) {
	app := capacityApp(int32Ptr(3), onPolicy(2, 6))
	app.Spec.Stopped = &kipperv1.AppStopped{Reason: "maintenance"}
	code, resp := serveAutoscale(t, &Autoscale{Client: fake.NewClientset(countedDeployment(0, 0)), CRClient: testCRClient(app)}, "GET", "")

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", code, resp)
	}
	if resp["stopped"] != true || resp["deployment_replicas"] != float64(0) || resp["ready_replicas"] != float64(0) || resp["replicas"] != float64(3) {
		t.Errorf("expected a stopped app with a known zero and the stored 3, got %v", resp)
	}
}

func TestAutoscaleGet_LeavesTheAutoscalerAloneWhenOff(t *testing.T) {
	app := capacityApp(int32Ptr(3), &kipperv1.AppAutoscale{Enabled: false, MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](6)})
	code, resp := serveAutoscale(t, &Autoscale{Client: fake.NewClientset(countedDeployment(3, 3), scalingHPA()), CRClient: testCRClient(app)}, "GET", "")

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", code, resp)
	}
	if !isNull(resp, "conditions") || !isNull(resp, "last_scale_time") {
		t.Errorf("an app with its policy off has no autoscaler state, got conditions %v and last_scale_time %v", resp["conditions"], resp["last_scale_time"])
	}
	if resp["deployment_replicas"] != float64(3) || resp["ready_replicas"] != float64(3) {
		t.Errorf("the Deployment's counts are still reported, got %v", resp)
	}
}

func TestAutoscaleGet_ReportsAQuotaBlock(t *testing.T) {
	dep := countedDeployment(4, 2)
	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate",
		Message: `pods "web-abc" is forbidden: exceeded quota: project-quota, requested: cpu=500m`,
	}}
	code, resp := serveAutoscale(t, &Autoscale{Client: fake.NewClientset(dep, scalingHPA()), CRClient: testCRClient(capacityApp(int32Ptr(3), onPolicy(2, 6)))}, "GET", "")

	if code != http.StatusOK || resp["quota_blocked"] != true {
		t.Errorf("expected quota_blocked true, got %d: %v", code, resp["quota_blocked"])
	}
}

func TestAutoscaleGet_AMissingAppIsUnknown(t *testing.T) {
	code, resp := serveAutoscale(t, &Autoscale{Client: fake.NewClientset(), CRClient: testCRClient()}, "GET", "")

	if code != http.StatusOK || resp["enabled"] != false || resp["capacity_api"] != float64(1) {
		t.Fatalf("expected 200 disabled with capacity_api, got %d: %v", code, resp)
	}
	for _, key := range []string{"replicas", "stopped", "deployment_replicas", "activity"} {
		if !isNull(resp, key) {
			t.Errorf("%s = %#v, want null for a missing app", key, resp[key])
		}
	}
}

func hpaEvent(name, kind, reason, message string, at time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "staging"},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: "web", Namespace: "staging"},
		Type:           corev1.EventTypeNormal,
		Reason:         reason,
		Message:        message,
		LastTimestamp:  metav1.NewTime(at),
	}
}

func scaleLog(t *testing.T, entries []ResourceLogEntry) *corev1.ConfigMap {
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

func TestAutoscaleGet_ReportsRecentScaleActivity(t *testing.T) {
	base := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	log := scaleLog(t, []ResourceLogEntry{
		{Time: base.Add(10 * time.Minute).Format(time.RFC3339), App: "web", Namespace: "staging", Action: "scaled out (2 → 4 pods)", From: "2", To: "4", Reason: "HPA autoscaling"},
		{Time: base.Add(11 * time.Minute).Format(time.RFC3339), App: "web", Namespace: "staging", Action: "increased memory", From: "128Mi", To: "256Mi", Reason: "usage"},
		{Time: base.Add(12 * time.Minute).Format(time.RFC3339), App: "api", Namespace: "staging", Action: "scaled out (1 → 2 pods)", Reason: "HPA autoscaling"},
		{Time: base.Add(13 * time.Minute).Format(time.RFC3339), App: "web", Namespace: "production", Action: "scaled in (3 → 2 pods)", Reason: "HPA autoscaling"},
	})
	client := fake.NewClientset(
		log,
		hpaEvent("web.1", "HorizontalPodAutoscaler", "SuccessfulRescale", "New size: 4; reason: cpu resource utilization above target", base.Add(5*time.Minute)),
		hpaEvent("web.2", "HorizontalPodAutoscaler", "FailedGetResourceMetric", "failed to get cpu utilization", base.Add(20*time.Minute)),
		hpaEvent("web.3", "Deployment", "ScalingReplicaSet", "Scaled up replica set web-abc to 4", base.Add(6*time.Minute)),
	)
	code, resp := serveAutoscale(t, &Autoscale{Client: client, CRClient: testCRClient(capacityApp(int32Ptr(3), onPolicy(2, 6)))}, "GET", "")

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", code, resp)
	}
	if resp["activity_partial"] != true {
		t.Errorf("activity must be labelled partial, got %v", resp["activity_partial"])
	}
	activity, _ := resp["activity"].([]any)
	var got []string
	for _, a := range activity {
		entry := a.(map[string]any)
		got = append(got, entry["source"].(string)+" "+entry["reason"].(string)+" "+entry["time"].(string))
	}
	want := []string{
		"hpa_event FailedGetResourceMetric 2026-10-04T09:20:00Z",
		"scale_log HPA autoscaling 2026-10-04T09:10:00Z",
		"hpa_event SuccessfulRescale 2026-10-04T09:05:00Z",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("activity = %v, want newest first and only this app's autoscaler entries: %v", got, want)
	}
	first := activity[1].(map[string]any)
	if first["message"] != "scaled out (2 → 4 pods)" {
		t.Errorf("scale log message = %v, want the logged action", first["message"])
	}
}

func TestAutoscaleGet_RepeatedEventUsesItsLatestOccurrence(t *testing.T) {
	base := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	log := scaleLog(t, []ResourceLogEntry{
		{Time: base.Add(10 * time.Minute).Format(time.RFC3339), App: "web", Namespace: "staging", Action: "scaled out (2 → 4 pods)", Reason: "HPA autoscaling"},
	})
	repeated := hpaEvent("web.1", "HorizontalPodAutoscaler", "FailedGetResourceMetric", "failed to get cpu utilization", time.Time{})
	repeated.LastTimestamp = metav1.Time{}
	repeated.EventTime = metav1.NewMicroTime(base.Add(1 * time.Minute))
	repeated.Series = &corev1.EventSeries{Count: 12, LastObservedTime: metav1.NewMicroTime(base.Add(15 * time.Minute))}
	client := fake.NewClientset(log, repeated)
	code, resp := serveAutoscale(t, &Autoscale{Client: client, CRClient: testCRClient(capacityApp(int32Ptr(3), onPolicy(2, 6)))}, "GET", "")

	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %v", code, resp)
	}
	activity, _ := resp["activity"].([]any)
	var got []string
	for _, a := range activity {
		entry := a.(map[string]any)
		got = append(got, entry["source"].(string)+" "+entry["time"].(string))
	}
	want := []string{"hpa_event 2026-10-04T09:15:00Z", "scale_log 2026-10-04T09:10:00Z"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("activity = %v, want the series' latest occurrence ahead of the older scale-log entry: %v", got, want)
	}
}

func TestAutoscaleGet_ActivityIsUnknownWhenNeitherSourceReads(t *testing.T) {
	client := fake.NewClientset()
	fail := func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("unavailable") }
	client.PrependReactor("list", "events", fail)
	client.PrependReactor("get", "configmaps", fail)
	code, resp := serveAutoscale(t, &Autoscale{Client: client, CRClient: testCRClient(capacityApp(int32Ptr(3), onPolicy(2, 6)))}, "GET", "")

	if code != http.StatusOK || !isNull(resp, "activity") {
		t.Errorf("expected 200 with activity null, got %d: %v", code, resp["activity"])
	}
}

func TestAutoscaleGet_AnEmptyHistoryIsAnEmptyList(t *testing.T) {
	code, resp := serveAutoscale(t, &Autoscale{Client: fake.NewClientset(), CRClient: testCRClient(capacityApp(int32Ptr(3), onPolicy(2, 6)))}, "GET", "")

	activity, ok := resp["activity"].([]any)
	if code != http.StatusOK || !ok || len(activity) != 0 {
		t.Errorf("expected an empty activity list, got %d: %#v", code, resp["activity"])
	}
}

func TestAutoscaleSet_EnabledFieldAndReplicas(t *testing.T) {
	disabledBlock := &kipperv1.AppAutoscale{Enabled: false, MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](6), CPUTarget: ptr.To[int32](70)}
	tests := []struct {
		name        string
		as          *kipperv1.AppAutoscale
		stopped     bool
		replicas    *int32
		deployment  *appsv1.Deployment
		body        string
		wantStatus  int
		wantError   string
		wantBlock   *kipperv1.AppAutoscale
		wantStored  *int32
		wantMoved   map[string]any
		wantWarning string
		wantNote    bool
		wantNoWrite bool
	}{
		{
			name: "an absent enabled field enables, as before", as: disabledBlock, replicas: int32Ptr(3),
			body:       `{"min_replicas":2,"max_replicas":6,"cpu_target":70}`,
			wantStatus: http.StatusOK, wantBlock: onPolicy(2, 6), wantStored: int32Ptr(3),
		},
		{
			name: "enabled true with a desired count is refused", as: disabledBlock, replicas: int32Ptr(3),
			body:       `{"enabled":true,"replicas":4,"min_replicas":2,"max_replicas":6,"cpu_target":70}`,
			wantStatus: http.StatusBadRequest, wantError: "autoscaling sets the count", wantNoWrite: true,
		},
		{
			name: "an absent enabled field with a desired count is refused", replicas: int32Ptr(3),
			body:       `{"replicas":4,"min_replicas":2,"max_replicas":6,"cpu_target":70}`,
			wantStatus: http.StatusBadRequest, wantError: "autoscaling sets the count", wantNoWrite: true,
		},
		{
			name: "switching off keeps the running count within the new bounds", as: onPolicy(2, 6), replicas: int32Ptr(2), deployment: liveDeployment(4),
			body:       `{"enabled":false,"min_replicas":3,"max_replicas":8,"cpu_target":60}`,
			wantStatus: http.StatusOK,
			wantBlock:  &kipperv1.AppAutoscale{Enabled: false, MinReplicas: ptr.To[int32](3), MaxReplicas: ptr.To[int32](8), CPUTarget: ptr.To[int32](60)},
			wantStored: int32Ptr(4),
		},
		{
			name: "switching off clamps the running count into the new bounds", as: onPolicy(2, 10), replicas: int32Ptr(2), deployment: liveDeployment(9),
			body:       `{"enabled":false,"min_replicas":2,"max_replicas":6,"cpu_target":70}`,
			wantStatus: http.StatusOK,
			wantBlock:  &kipperv1.AppAutoscale{Enabled: false, MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](6), CPUTarget: ptr.To[int32](70)},
			wantStored: int32Ptr(6), wantMoved: map[string]any{"from": float64(9), "to": float64(6)},
		},
		{
			name: "an explicit desired count wins over the running count", as: onPolicy(2, 6), replicas: int32Ptr(2), deployment: liveDeployment(4),
			body:       `{"enabled":false,"replicas":5,"min_replicas":2,"max_replicas":6,"cpu_target":70}`,
			wantStatus: http.StatusOK,
			wantBlock:  &kipperv1.AppAutoscale{Enabled: false, MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](6), CPUTarget: ptr.To[int32](70)},
			wantStored: int32Ptr(5),
		},
		{
			name: "a desired count outside the new bounds is refused", as: onPolicy(2, 6), replicas: int32Ptr(2), deployment: liveDeployment(4),
			body:       `{"enabled":false,"replicas":9,"min_replicas":2,"max_replicas":6}`,
			wantStatus: http.StatusBadRequest, wantError: "between 2 and 6", wantNoWrite: true,
		},
		{
			name: "a negative desired count is refused", replicas: int32Ptr(3),
			body:       `{"enabled":false,"replicas":-1}`,
			wantStatus: http.StatusBadRequest, wantError: "non-negative", wantNoWrite: true,
		},
		{
			name: "a minimum without a maximum is refused", as: disabledBlock, replicas: int32Ptr(3),
			body:       `{"enabled":false,"min_replicas":2}`,
			wantStatus: http.StatusBadRequest, wantError: "max_replicas", wantNoWrite: true,
		},
		{
			name: "a save without bounds never creates a block", replicas: int32Ptr(3),
			body:       `{"enabled":false}`,
			wantStatus: http.StatusOK, wantStored: int32Ptr(3), wantNoWrite: true,
		},
		{
			name: "a desired count without bounds is stored and creates no block", replicas: int32Ptr(3),
			body:       `{"enabled":false,"replicas":7}`,
			wantStatus: http.StatusOK, wantStored: int32Ptr(7),
		},
		{
			name: "a save without bounds removes the stored bounds", as: disabledBlock, replicas: int32Ptr(3),
			body:       `{"enabled":false}`,
			wantStatus: http.StatusOK, wantStored: int32Ptr(3),
		},
		{
			name: "switching off with the bounds removed keeps the running count", as: onPolicy(2, 6), replicas: int32Ptr(2), deployment: liveDeployment(5),
			body:       `{"enabled":false}`,
			wantStatus: http.StatusOK, wantStored: int32Ptr(5),
		},
		{
			name: "new bounds move a stored count when the policy was already off", as: disabledBlock, replicas: int32Ptr(2),
			body:       `{"enabled":false,"min_replicas":4,"max_replicas":6}`,
			wantStatus: http.StatusOK,
			wantBlock:  &kipperv1.AppAutoscale{Enabled: false, MinReplicas: ptr.To[int32](4), MaxReplicas: ptr.To[int32](6)},
			wantStored: int32Ptr(4), wantMoved: map[string]any{"from": float64(2), "to": float64(4)},
		},
		{
			name: "an unreadable running count keeps the stored count and says so", as: onPolicy(2, 6), replicas: int32Ptr(3),
			body:       `{"enabled":false,"min_replicas":2,"max_replicas":6,"cpu_target":70}`,
			wantStatus: http.StatusOK,
			wantBlock:  &kipperv1.AppAutoscale{Enabled: false, MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](6), CPUTarget: ptr.To[int32](70)},
			wantStored: int32Ptr(3), wantWarning: "could not be read",
		},
		{
			name: "an unreadable running count says the stored count moved into the new bounds", as: onPolicy(2, 6), replicas: int32Ptr(2),
			body:       `{"enabled":false,"min_replicas":4,"max_replicas":6}`,
			wantStatus: http.StatusOK,
			wantBlock:  &kipperv1.AppAutoscale{Enabled: false, MinReplicas: ptr.To[int32](4), MaxReplicas: ptr.To[int32](6)},
			wantStored: int32Ptr(4), wantMoved: map[string]any{"from": float64(2), "to": float64(4)},
			wantWarning: "the stored count of 2 was moved into the new bounds as 4",
		},
		{
			name: "a stopped app keeps its stored count and is told when it applies", as: onPolicy(2, 6), stopped: true, replicas: int32Ptr(3), deployment: liveDeployment(0),
			body:       `{"enabled":false,"min_replicas":2,"max_replicas":6,"cpu_target":70}`,
			wantStatus: http.StatusOK,
			wantBlock:  &kipperv1.AppAutoscale{Enabled: false, MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](6), CPUTarget: ptr.To[int32](70)},
			wantStored: int32Ptr(3), wantNote: true,
		},
		{
			name: "an unchanged disabled block writes nothing", as: disabledBlock, replicas: int32Ptr(3),
			body:       `{"enabled":false,"min_replicas":2,"max_replicas":6,"cpu_target":70}`,
			wantStatus: http.StatusOK, wantBlock: disabledBlock, wantStored: int32Ptr(3), wantNoWrite: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := capacityApp(tt.replicas, tt.as)
			if tt.stopped {
				app.Spec.Stopped = &kipperv1.AppStopped{Reason: "maintenance"}
			}
			client := fake.NewClientset()
			if tt.deployment != nil {
				client = fake.NewClientset(tt.deployment)
			}
			crClient := testCRClient(app)
			before := storedStagingApp(t, crClient)
			code, resp := serveAutoscale(t, &Autoscale{Client: client, CRClient: crClient}, "PUT", tt.body)

			if code != tt.wantStatus {
				t.Fatalf("expected %d, got %d: %v", tt.wantStatus, code, resp)
			}
			if tt.wantError != "" {
				if msg, _ := resp["error"].(string); !strings.Contains(msg, tt.wantError) {
					t.Errorf("error = %q, want it to contain %q", msg, tt.wantError)
				}
			}
			stored := storedStagingApp(t, crClient)
			if tt.wantNoWrite && stored.ResourceVersion != before.ResourceVersion {
				t.Errorf("expected no write, got %+v", stored.Spec)
			}
			if code != http.StatusOK {
				return
			}
			if !reflect.DeepEqual(stored.Spec.Autoscale, tt.wantBlock) {
				t.Errorf("stored block = %s, want %s", blockString(stored.Spec.Autoscale), blockString(tt.wantBlock))
			}
			if !reflect.DeepEqual(stored.Spec.Replicas, tt.wantStored) {
				t.Errorf("stored replicas = %v, want %v", ptr.Deref(stored.Spec.Replicas, -1), ptr.Deref(tt.wantStored, -1))
			}
			if tt.wantMoved == nil {
				if _, ok := resp["replicas_moved"]; ok {
					t.Errorf("unexpected replicas_moved: %v", resp["replicas_moved"])
				}
			} else if !reflect.DeepEqual(resp["replicas_moved"], tt.wantMoved) {
				t.Errorf("replicas_moved = %v, want %v", resp["replicas_moved"], tt.wantMoved)
			}
			warning, _ := resp["warning"].(string)
			if (tt.wantWarning == "") != (warning == "") || !strings.Contains(warning, tt.wantWarning) {
				t.Errorf("warning = %q, want %q", warning, tt.wantWarning)
			}
			if _, hasNote := resp["note"]; hasNote != tt.wantNote {
				t.Errorf("note = %v, want present: %v", resp["note"], tt.wantNote)
			}
			if tt.wantBlock == nil || !tt.wantBlock.Enabled {
				if resp["status"] != "disabled" || resp["replicas"] != float64(*tt.wantStored) {
					t.Errorf("expected status disabled and replicas %d, got %v", *tt.wantStored, resp)
				}
			}
		})
	}
}

func blockString(as *kipperv1.AppAutoscale) string {
	if as == nil {
		return "nil"
	}
	data, _ := json.Marshal(as)
	return string(data)
}

func TestAutoscaleSet_PassesOnTheAPIServersRefusal(t *testing.T) {
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(capacityApp(int32Ptr(3), nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				return apierrors.NewInvalid(schema.GroupKind{Group: "kipper.run", Kind: "App"}, "web", field.ErrorList{
					field.Invalid(field.NewPath("spec", "autoscale"), nil, "maxReplicas must be at least 1"),
				})
			},
		}).Build()

	for name, body := range map[string]string{
		"enable":  `{"min_replicas":2,"max_replicas":6,"cpu_target":70}`,
		"disable": `{"enabled":false,"min_replicas":2,"max_replicas":6}`,
	} {
		t.Run(name, func(t *testing.T) {
			code, resp := serveAutoscale(t, &Autoscale{Client: fake.NewClientset(), CRClient: crClient}, "PUT", body)
			if code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d: %v", code, resp)
			}
			if msg, _ := resp["error"].(string); !strings.Contains(msg, "maxReplicas must be at least 1") {
				t.Errorf("expected the refusal's reason, got %v", resp)
			}
		})
	}
}

func TestScale_PassesOnTheAPIServersRefusal(t *testing.T) {
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(capacityApp(int32Ptr(3), nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				return apierrors.NewInvalid(schema.GroupKind{Group: "kipper.run", Kind: "App"}, "web", field.ErrorList{
					field.Invalid(field.NewPath("spec", "replicas"), nil, "replicas must be between 2 and 6"),
				})
			},
		}).Build()

	code, resp := serveScale(t, &Apps{Client: fake.NewClientset(), CRClient: crClient}, `{"replicas":4}`)

	if code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %v", code, resp)
	}
	if msg, _ := resp["error"].(string); !strings.Contains(msg, "between 2 and 6") {
		t.Errorf("expected the refusal's reason, got %v", resp)
	}
}
