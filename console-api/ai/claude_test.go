package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClaudeChatStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("expected x-api-key header to be test-key, got %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("expected anthropic-version 2023-06-01, got %q", r.Header.Get("anthropic-version"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type application/json, got %q", r.Header.Get("Content-Type"))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		chunks := []string{
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}`,
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":" world"}}`,
			`data: {"type":"message_stop"}`,
		}

		for _, chunk := range chunks {
			_, _ = w.Write([]byte(chunk + "\n"))
		}
	}))
	defer server.Close()

	// Override the API URL by using a custom client that rewrites the URL.
	// Since Claude uses http.DefaultClient and a hardcoded URL, we use a
	// test transport to redirect requests to our test server.
	origTransport := http.DefaultTransport
	http.DefaultTransport = &rewriteTransport{target: server.URL, wrapped: origTransport}
	defer func() { http.DefaultTransport = origTransport }()

	claude := &Claude{APIKey: "test-key", Model: "claude-sonnet-4-5-20250514"}
	messages := []Message{{Role: "user", Content: "say hello"}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := claude.Chat(ctx, "you are helpful", messages)
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

func TestClaudeChatNonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = &rewriteTransport{target: server.URL, wrapped: origTransport}
	defer func() { http.DefaultTransport = origTransport }()

	claude := &Claude{APIKey: "test-key", Model: "test-model"}
	messages := []Message{{Role: "user", Content: "hello"}}

	_, err := claude.Chat(context.Background(), "system", messages)
	if err == nil {
		t.Fatal("expected error for non-OK status")
	}
	if err.Error() != "anthropic API returned 429" {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestClaudeChatDoneMarker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer server.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = &rewriteTransport{target: server.URL, wrapped: origTransport}
	defer func() { http.DefaultTransport = origTransport }()

	claude := &Claude{APIKey: "key", Model: "model"}
	stream, err := claude.Chat(context.Background(), "sys", []Message{{Role: "user", Content: "hi"}})
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

func TestClaudeChatTruncationWarning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		lines := []string{
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"partial code"}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`,
			`data: {"type":"message_stop"}`,
		}
		for _, line := range lines {
			_, _ = w.Write([]byte(line + "\n"))
		}
	}))
	defer server.Close()

	origTransport := http.DefaultTransport
	http.DefaultTransport = &rewriteTransport{target: server.URL, wrapped: origTransport}
	defer func() { http.DefaultTransport = origTransport }()

	claude := &Claude{APIKey: "key", Model: "model"}
	stream, err := claude.Chat(context.Background(), "sys", []Message{{Role: "user", Content: "hi"}})
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

// rewriteTransport redirects all HTTP requests to a test server.
type rewriteTransport struct {
	target  string
	wrapped http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = t.target[len("http://"):]
	return t.wrapped.RoundTrip(req)
}
