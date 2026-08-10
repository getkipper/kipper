package ai

import (
	"context"
	"fmt"
)

// Message represents a single message in a conversation.
type Message struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

// StreamChunk is a piece of a streaming response.
type StreamChunk struct {
	Content string
	Done    bool
	Err     error
}

// Provider abstracts an AI model API that supports streaming chat.
type Provider interface {
	Chat(ctx context.Context, system string, messages []Message) (<-chan StreamChunk, error)
}

// Config holds the settings needed to create a provider.
type Config struct {
	Provider  string
	APIKey    string
	Model     string
	OllamaURL string
}

// NewProvider creates a Provider from the given config.
func NewProvider(cfg Config) (Provider, error) {
	switch cfg.Provider {
	case "claude":
		model := cfg.Model
		if model == "" {
			model = "claude-sonnet-4-5-20250514"
		}
		return &Claude{APIKey: cfg.APIKey, Model: model}, nil

	case "openai":
		model := cfg.Model
		if model == "" {
			model = "gpt-4o"
		}
		return &OpenAI{APIKey: cfg.APIKey, Model: model}, nil

	case "ollama":
		url := cfg.OllamaURL
		if url == "" {
			url = "http://localhost:11434"
		}
		model := cfg.Model
		if model == "" {
			model = "llama3.1"
		}
		return &Ollama{URL: url, Model: model}, nil

	default:
		return nil, fmt.Errorf("unknown AI provider: %q", cfg.Provider)
	}
}
