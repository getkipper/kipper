package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestModeGetDefaultsToAuto(t *testing.T) {
	client := fake.NewClientset()
	handler := &Mode{Client: client}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/settings/mode", nil)

	handler.Get(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp modeResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Mode != "auto" {
		t.Errorf("expected mode 'auto', got %q", resp.Mode)
	}
}

func TestModeGetReadsConfigMap(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modeConfigMapName,
			Namespace: modeConfigMapNamespace,
		},
		Data: map[string]string{"mode": "expert"},
	}

	client := fake.NewClientset(cm)
	handler := &Mode{Client: client}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/settings/mode", nil)

	handler.Get(w, r)

	var resp modeResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if resp.Mode != "expert" {
		t.Errorf("expected mode 'expert', got %q", resp.Mode)
	}
}

func TestModeUpdateCreatesConfigMap(t *testing.T) {
	client := fake.NewClientset()
	handler := &Mode{Client: client}

	body, _ := json.Marshal(modeRequest{Mode: "expert"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/settings/mode", bytes.NewReader(body))

	handler.Update(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp modeResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if resp.Mode != "expert" {
		t.Errorf("expected mode 'expert', got %q", resp.Mode)
	}
}

func TestModeUpdateExistingConfigMap(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modeConfigMapName,
			Namespace: modeConfigMapNamespace,
		},
		Data: map[string]string{"mode": "auto"},
	}

	client := fake.NewClientset(cm)
	handler := &Mode{Client: client}

	body, _ := json.Marshal(modeRequest{Mode: "expert"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/settings/mode", bytes.NewReader(body))

	handler.Update(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp modeResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if resp.Mode != "expert" {
		t.Errorf("expected mode 'expert', got %q", resp.Mode)
	}
}

func TestModeUpdateRejectsInvalidMode(t *testing.T) {
	client := fake.NewClientset()
	handler := &Mode{Client: client}

	body, _ := json.Marshal(modeRequest{Mode: "invalid"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/settings/mode", bytes.NewReader(body))

	handler.Update(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestResourceLogEmptyByDefault(t *testing.T) {
	client := fake.NewClientset()
	handler := &Mode{Client: client}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/settings/resource-log", nil)

	handler.GetResourceLog(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp resourceLogResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if len(resp.Entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(resp.Entries))
	}
}

func TestResourceLogReturnsEntries(t *testing.T) {
	entries := []ResourceLogEntry{
		{Time: "2026-03-22T10:00:00Z", App: "frontend", Namespace: "default", Action: "increased memory", From: "128Mi", To: "192Mi", Reason: "usage at 85%"},
	}
	data, _ := json.Marshal(entries)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceLogConfigMap,
			Namespace: modeConfigMapNamespace,
		},
		Data: map[string]string{"entries": string(data)},
	}

	client := fake.NewClientset(cm)
	handler := &Mode{Client: client}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/settings/resource-log", nil)

	handler.GetResourceLog(w, r)

	var resp resourceLogResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if len(resp.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(resp.Entries))
	}
	if resp.Entries[0].App != "frontend" {
		t.Errorf("expected app 'frontend', got %q", resp.Entries[0].App)
	}
}
