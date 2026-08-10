package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOpenAIChatStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("expected Authorization header, got %q", r.Header.Get("Authorization"))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		chunks := []string{
			`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
			`data: {"choices":[{"delta":{"content":" world"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		}

		for _, chunk := range chunks {
			_, _ = w.Write([]byte(chunk + "\n"))
		}
	}))
	defer server.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = &rewriteTransport{target: server.URL, wrapped: origTransport}
	defer func() { http.DefaultTransport = origTransport }()

	openai := &OpenAI{APIKey: "test-key", Model: "gpt-4o"}
	messages := []Message{{Role: "user", Content: "say hello"}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := openai.Chat(ctx, "you are helpful", messages)
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

func TestOpenAIChatNonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = &rewriteTransport{target: server.URL, wrapped: origTransport}
	defer func() { http.DefaultTransport = origTransport }()

	openai := &OpenAI{APIKey: "bad-key", Model: "gpt-4o"}
	_, err := openai.Chat(context.Background(), "system", []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("expected error for non-OK status")
	}
	if err.Error() != "openai API returned 401" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestOpenAIChatTruncationWarning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		lines := []string{
			`data: {"choices":[{"delta":{"content":"partial code"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
		}
		for _, line := range lines {
			_, _ = w.Write([]byte(line + "\n"))
		}
	}))
	defer server.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = &rewriteTransport{target: server.URL, wrapped: origTransport}
	defer func() { http.DefaultTransport = origTransport }()

	openai := &OpenAI{APIKey: "key", Model: "model"}
	stream, err := openai.Chat(context.Background(), "sys", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var chunks []StreamChunk
	for c := range stream {
		chunks = append(chunks, c)
	}

	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks (content, warning, done), got %d", len(chunks))
	}
	if chunks[0].Content != "partial code" {
		t.Errorf("expected first chunk to be content, got %q", chunks[0].Content)
	}
	if chunks[1].Content == "" || chunks[1].Done {
		t.Error("expected second chunk to be truncation warning content")
	}
	if !chunks[2].Done {
		t.Error("expected third chunk to be done signal")
	}
}

func TestOpenAIChatDoneDataMarker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer server.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = &rewriteTransport{target: server.URL, wrapped: origTransport}
	defer func() { http.DefaultTransport = origTransport }()

	openai := &OpenAI{APIKey: "key", Model: "model"}
	stream, err := openai.Chat(context.Background(), "sys", []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var chunks []StreamChunk
	for c := range stream {
		chunks = append(chunks, c)
	}

	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if chunks[0].Content != "hi" {
		t.Errorf("expected first chunk content 'hi', got %q", chunks[0].Content)
	}
	if !chunks[1].Done {
		t.Error("expected second chunk to be done")
	}
}
