package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOllamaChatStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("expected path /api/chat, got %s", r.URL.Path)
		}

		// Verify request body structure
		body, _ := io.ReadAll(r.Body)
		var reqBody map[string]interface{}
		if err := json.Unmarshal(body, &reqBody); err != nil {
			t.Errorf("failed to parse request body: %v", err)
		}
		if reqBody["model"] != "llama3.1" {
			t.Errorf("expected model llama3.1, got %v", reqBody["model"])
		}
		if reqBody["stream"] != true {
			t.Errorf("expected stream=true")
		}

		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)

		lines := []string{
			`{"message":{"content":"Hello"},"done":false}`,
			`{"message":{"content":" world"},"done":false}`,
			`{"message":{"content":""},"done":true}`,
		}

		for _, line := range lines {
			_, _ = w.Write([]byte(line + "\n"))
		}
	}))
	defer server.Close()

	ollama := &Ollama{URL: server.URL, Model: "llama3.1"}
	messages := []Message{{Role: "user", Content: "say hello"}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := ollama.Chat(ctx, "you are helpful", messages)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var content string
	var gotDone bool
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		if chunk.Done {
			gotDone = true
			break
		}
		content += chunk.Content
	}

	if content != "Hello world" {
		t.Errorf("expected content %q, got %q", "Hello world", content)
	}
	if !gotDone {
		t.Error("expected done signal")
	}
}

func TestOllamaChatNonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ollama := &Ollama{URL: server.URL, Model: "llama3.1"}
	_, err := ollama.Chat(context.Background(), "system", []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("expected error for non-OK status")
	}
	if err.Error() != "ollama API returned 503" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestOllamaChatPrependsSystemMessage(t *testing.T) {
	var capturedMessages []interface{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var reqBody map[string]interface{}
		_ = json.Unmarshal(body, &reqBody)
		capturedMessages = reqBody["messages"].([]interface{})

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"message":{"content":"ok"},"done":true}` + "\n"))
	}))
	defer server.Close()

	ollama := &Ollama{URL: server.URL, Model: "test"}
	messages := []Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}

	stream, err := ollama.Chat(context.Background(), "be helpful", messages)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for range stream {
		// drain
	}

	if len(capturedMessages) != 3 {
		t.Fatalf("expected 3 messages (system + 2 user), got %d", len(capturedMessages))
	}

	firstMsg := capturedMessages[0].(map[string]interface{})
	if firstMsg["role"] != "system" {
		t.Errorf("expected first message role to be 'system', got %q", firstMsg["role"])
	}
	if firstMsg["content"] != "be helpful" {
		t.Errorf("expected system content 'be helpful', got %q", firstMsg["content"])
	}
}

func TestOllamaChatConnectionRefused(t *testing.T) {
	ollama := &Ollama{URL: "http://localhost:1", Model: "test"}
	_, err := ollama.Chat(context.Background(), "system", []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("expected error when server is unreachable")
	}
}
