package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getkipper/kipper/console-api/middleware"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func requestWithUser(r *http.Request, email string) *http.Request {
	claims := &middleware.Claims{Email: email}
	ctx := context.WithValue(r.Context(), middleware.UserContextKey, claims)
	return r.WithContext(ctx)
}

func TestAlerts_List_Empty(t *testing.T) {
	client := fake.NewClientset()
	handler := &Alerts{Client: client}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/alerts", nil)

	handler.List(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp alertsListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Alerts) != 0 {
		t.Errorf("expected 0 alerts, got %d", len(resp.Alerts))
	}
}

func TestAlerts_List_WithEntries(t *testing.T) {
	alerts := []Alert{
		{ID: "1", Time: "2026-03-22T10:00:00Z", App: "frontend", Namespace: "default", Action: "increased memory", Severity: "info", Reason: "usage at 85%"},
		{ID: "2", Time: "2026-03-22T11:00:00Z", App: "backend", Namespace: "default", Action: "doubled memory limit", Severity: "critical", Reason: "OOMKilled detected"},
	}
	data, _ := json.Marshal(alerts)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      alertsConfigMapName,
			Namespace: alertsNamespace,
		},
		Data: map[string]string{"alerts": string(data)},
	}

	client := fake.NewClientset(cm)
	handler := &Alerts{Client: client}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/alerts", nil)

	handler.List(w, r)

	var resp alertsListResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if len(resp.Alerts) != 2 {
		t.Fatalf("expected 2 alerts, got %d", len(resp.Alerts))
	}

	// Newest first
	if resp.Alerts[0].ID != "2" {
		t.Errorf("expected newest alert first, got id %q", resp.Alerts[0].ID)
	}
}

func TestAlerts_UnreadCount(t *testing.T) {
	alerts := []Alert{
		{ID: "1", Time: "2026-03-22T10:00:00Z", App: "frontend", Namespace: "default", Action: "increased memory", Severity: "info", Reason: "usage at 85%"},
		{ID: "2", Time: "2026-03-22T11:00:00Z", App: "backend", Namespace: "default", Action: "doubled memory", Severity: "critical", Reason: "OOMKilled"},
		{ID: "3", Time: "2026-03-22T12:00:00Z", App: "api", Namespace: "default", Action: "decreased CPU", Severity: "info", Reason: "low usage"},
	}
	data, _ := json.Marshal(alerts)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      alertsConfigMapName,
			Namespace: alertsNamespace,
		},
		Data: map[string]string{
			"alerts":                 string(data),
			"dismiss_dev_at_test_io": "2026-03-22T10:30:00Z",
		},
	}

	client := fake.NewClientset(cm)
	handler := &Alerts{Client: client}

	w := httptest.NewRecorder()
	r := requestWithUser(httptest.NewRequest(http.MethodGet, "/api/v1/alerts/unread-count", nil), "dev@test.io")

	handler.UnreadCount(w, r)

	var resp unreadCountResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)

	// Alerts 2 and 3 are after the dismiss time
	if resp.Count != 2 {
		t.Errorf("expected 2 unread, got %d", resp.Count)
	}
}

func TestAlerts_Dismiss(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      alertsConfigMapName,
			Namespace: alertsNamespace,
		},
		Data: map[string]string{"alerts": "[]"},
	}

	client := fake.NewClientset(cm)
	handler := &Alerts{Client: client}

	w := httptest.NewRecorder()
	r := requestWithUser(httptest.NewRequest(http.MethodPost, "/api/v1/alerts/dismiss", nil), "dev@test.io")

	handler.Dismiss(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify the dismiss timestamp was stored
	updated, err := client.CoreV1().ConfigMaps(alertsNamespace).Get(context.Background(), alertsConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get configmap: %v", err)
	}

	dismissVal, ok := updated.Data["dismiss_dev_at_test_io"]
	if !ok {
		t.Fatal("expected dismiss timestamp to be stored")
	}
	if dismissVal == "" {
		t.Fatal("expected non-empty dismiss timestamp")
	}
}

func TestAddAlerts_BatchWriteAndCap(t *testing.T) {
	client := fake.NewClientset()
	ctx := context.Background()
	a := &Alerts{Client: client}

	_ = AddAlerts(ctx, client, []Alert{
		{ID: "1", Time: "2026-07-07T00:00:00Z", Action: "a"},
		{ID: "2", Time: "2026-07-07T00:00:01Z", Action: "b"},
	})
	if got := a.readAlerts(ctx); len(got) != 2 {
		t.Fatalf("expected 2 alerts after a batch write, got %d", len(got))
	}

	// A single mass-crash write is bounded to MaxAlertsPerWrite so it cannot
	// evict all prior history: the two earlier alerts must survive.
	many := make([]Alert, 0, maxAlerts+10)
	for i := 0; i < maxAlerts+10; i++ {
		many = append(many, Alert{ID: fmt.Sprintf("x%d", i), Action: "flood"})
	}
	_ = AddAlerts(ctx, client, many)
	got := a.readAlerts(ctx)
	if len(got) != 2+MaxAlertsPerWrite {
		t.Fatalf("expected 2 preserved + %d newest = %d alerts, got %d", MaxAlertsPerWrite, 2+MaxAlertsPerWrite, len(got))
	}
	survived := 0
	for _, al := range got {
		if al.ID == "1" || al.ID == "2" {
			survived++
		}
	}
	if survived != 2 {
		t.Fatalf("mass-crash write evicted earlier history: %d of the 2 original alerts survived", survived)
	}
}

func TestAddAlerts_RetriesOnConflict(t *testing.T) {
	client := fake.NewClientset()
	ctx := context.Background()
	a := &Alerts{Client: client}

	// Seed the ConfigMap so the write path is Get+Update, not Create.
	_ = AddAlerts(ctx, client, []Alert{{ID: "seed", Action: "seed"}})

	updates := 0
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Resource: "configmaps"}, alertsConfigMapName, errors.New("conflict"))
		}
		return false, nil, nil // let the default tracker handle the retry
	})

	_ = AddAlerts(ctx, client, []Alert{{ID: "after-conflict", Action: "x"}})

	got := a.readAlerts(ctx)
	found := false
	for _, al := range got {
		if al.ID == "after-conflict" {
			found = true
		}
	}
	if !found {
		t.Fatalf("alert was not persisted after a conflict retry; got %d alerts", len(got))
	}
	if updates < 2 {
		t.Fatalf("expected the update to be retried after a conflict, updates=%d", updates)
	}
}
