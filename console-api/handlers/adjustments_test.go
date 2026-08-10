package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func telemetryOnPC() *kipperv1.PlatformConfig {
	pc := newPlatformConfig("medium", nil, kipperv1.PlatformConfigStatus{})
	pc.Spec.Telemetry = &kipperv1.TelemetrySpec{RecordResourceAdjustments: true}
	return pc
}

func TestAdjustments_RecordWritesCRWhenTelemetryOn(t *testing.T) {
	crClient := testCRClient(telemetryOnPC())
	a := &Adjustments{CRClient: crClient}

	a.Record(context.Background(),
		"platform", "", "grafana", "memory",
		"128Mi", "256Mi", "gauge was 90%+", "admin@kipper.local")

	var list kipperv1.ResourceAdjustmentList
	require.NoError(t, crClient.List(context.Background(), &list))
	require.Len(t, list.Items, 1)
	got := list.Items[0]
	assert.Equal(t, "grafana", got.Spec.Component)
	assert.Equal(t, "platform", got.Spec.Scope)
	assert.Equal(t, "memory", got.Spec.Kind)
	assert.Equal(t, "128Mi", got.Spec.From)
	assert.Equal(t, "256Mi", got.Spec.To)
	assert.Equal(t, "gauge was 90%+", got.Spec.Reason)
	assert.Equal(t, "admin@kipper.local", got.Spec.AppliedBy)
	assert.NotEmpty(t, got.Name, "GenerateName should give the CR a unique name")
}

func TestAdjustments_RecordSkippedWhenTelemetryOff(t *testing.T) {
	// PlatformConfig exists but the telemetry block is nil — default state.
	crClient := testCRClient(newPlatformConfig("medium", nil, kipperv1.PlatformConfigStatus{}))
	a := &Adjustments{CRClient: crClient}

	a.Record(context.Background(),
		"platform", "", "grafana", "memory",
		"128Mi", "256Mi", "", "admin@kipper.local")

	var list kipperv1.ResourceAdjustmentList
	require.NoError(t, crClient.List(context.Background(), &list))
	assert.Empty(t, list.Items, "no record should be written when telemetry is off")
}

func TestAdjustments_RecordSkippedForNoOpUpdates(t *testing.T) {
	// Telemetry is on but to == from (slider clicked Apply on an
	// unchanged value, or the limit wasn't actually changed).
	crClient := testCRClient(telemetryOnPC())
	a := &Adjustments{CRClient: crClient}

	a.Record(context.Background(),
		"app", "staging", "api", "memory",
		"512Mi", "512Mi", "", "alice@kipper.local")

	var list kipperv1.ResourceAdjustmentList
	require.NoError(t, crClient.List(context.Background(), &list))
	assert.Empty(t, list.Items)
}

func TestAdjustments_RecordIsNilSafe(t *testing.T) {
	// Helper functions used by handlers may call Record on a nil
	// receiver if a struct was constructed without wiring; the recorder
	// must not panic — telemetry is best-effort.
	var a *Adjustments
	assert.NotPanics(t, func() {
		a.Record(context.Background(), "platform", "", "loki", "memory", "1Gi", "2Gi", "", "")
	})
}

func TestAdjustments_ListReturnsNewestFirst(t *testing.T) {
	older := &kipperv1.ResourceAdjustment{
		ObjectMeta: metav1.ObjectMeta{Name: "radj-older"},
		Spec: kipperv1.ResourceAdjustmentSpec{
			Component: "loki", Scope: "platform", Kind: "memory",
			From: "512Mi", To: "1Gi",
			At: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
		},
	}
	newer := &kipperv1.ResourceAdjustment{
		ObjectMeta: metav1.ObjectMeta{Name: "radj-newer"},
		Spec: kipperv1.ResourceAdjustmentSpec{
			Component: "grafana", Scope: "platform", Kind: "memory",
			From: "128Mi", To: "256Mi",
			At: metav1.NewTime(time.Now().Add(-5 * time.Minute)),
		},
	}
	crClient := testCRClient(telemetryOnPC(), older, newer)
	a := &Adjustments{CRClient: crClient}

	req := httptest.NewRequest("GET", "/api/v1/resources/adjustments", nil)
	w := httptest.NewRecorder()
	a.List(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp adjustmentsListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Items, 2)
	assert.True(t, resp.Telemetry)
	assert.Equal(t, "grafana", resp.Items[0].Component, "newest first")
	assert.Equal(t, "loki", resp.Items[1].Component)
}

func TestAdjustments_ListHonorsLimit(t *testing.T) {
	objs := []crclient.Object{telemetryOnPC()}
	base := time.Now()
	for i := 0; i < 5; i++ {
		objs = append(objs, &kipperv1.ResourceAdjustment{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("radj-%d", i)},
			Spec: kipperv1.ResourceAdjustmentSpec{
				Component: "x", Scope: "platform", Kind: "memory",
				At: metav1.NewTime(base.Add(time.Duration(i) * time.Minute)),
			},
		})
	}
	crClient := testCRClient(objs...)
	a := &Adjustments{CRClient: crClient}

	req := httptest.NewRequest("GET", "/api/v1/resources/adjustments?limit=2", nil)
	w := httptest.NewRecorder()
	a.List(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp adjustmentsListResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.Items, 2)
}
