package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRespondJSON(t *testing.T) {
	w := httptest.NewRecorder()
	respondJSON(w, http.StatusOK, map[string]string{"hello": "world"})

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %s", ct)
	}
	if !strings.Contains(w.Body.String(), `"hello":"world"`) {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

func TestRespondError(t *testing.T) {
	w := httptest.NewRecorder()
	respondError(w, http.StatusBadRequest, "something went wrong")

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "something went wrong") {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

func TestDecodeJSON(t *testing.T) {
	body := strings.NewReader(`{"name":"staging"}`)
	r := httptest.NewRequest(http.MethodPost, "/", body)

	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Name != "staging" {
		t.Errorf("expected staging, got %s", req.Name)
	}
}

func TestDecodeJSONInvalidBody(t *testing.T) {
	body := strings.NewReader(`{{{invalid`)
	r := httptest.NewRequest(http.MethodPost, "/", body)

	var req struct{ Name string }
	if err := decodeJSON(r, &req); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}
