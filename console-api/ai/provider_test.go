package ai

import (
	"testing"
)

func TestNewProvider(t *testing.T) {
	tests := []struct {
		name        string
		cfg         Config
		expectType  string
		expectError string
	}{
		{
			name:       "claude with explicit model",
			cfg:        Config{Provider: "claude", APIKey: "sk-test", Model: "claude-sonnet-4-5-20250514"},
			expectType: "*ai.Claude",
		},
		{
			name:       "claude with default model",
			cfg:        Config{Provider: "claude", APIKey: "sk-test"},
			expectType: "*ai.Claude",
		},
		{
			name:       "openai with explicit model",
			cfg:        Config{Provider: "openai", APIKey: "sk-test", Model: "gpt-4o-mini"},
			expectType: "*ai.OpenAI",
		},
		{
			name:       "openai with default model",
			cfg:        Config{Provider: "openai", APIKey: "sk-test"},
			expectType: "*ai.OpenAI",
		},
		{
			name:       "ollama with explicit url and model",
			cfg:        Config{Provider: "ollama", OllamaURL: "http://ollama:11434", Model: "mistral"},
			expectType: "*ai.Ollama",
		},
		{
			name:       "ollama with defaults",
			cfg:        Config{Provider: "ollama"},
			expectType: "*ai.Ollama",
		},
		{
			name:        "unknown provider returns error",
			cfg:         Config{Provider: "gemini"},
			expectError: `unknown AI provider: "gemini"`,
		},
		{
			name:        "empty provider returns error",
			cfg:         Config{},
			expectError: `unknown AI provider: ""`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, err := NewProvider(tt.cfg)

			if tt.expectError != "" {
				if err == nil {
					t.Fatalf("expected error %q, got nil", tt.expectError)
				}
				if err.Error() != tt.expectError {
					t.Fatalf("expected error %q, got %q", tt.expectError, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			typeName := ""
			switch provider.(type) {
			case *Claude:
				typeName = "*ai.Claude"
			case *OpenAI:
				typeName = "*ai.OpenAI"
			case *Ollama:
				typeName = "*ai.Ollama"
			}

			if typeName != tt.expectType {
				t.Errorf("expected type %s, got %s", tt.expectType, typeName)
			}
		})
	}
}

func TestNewProviderDefaultModels(t *testing.T) {
	tests := []struct {
		name          string
		provider      string
		expectedModel string
	}{
		{"claude default", "claude", "claude-sonnet-4-5-20250514"},
		{"openai default", "openai", "gpt-4o"},
		{"ollama default", "ollama", "llama3.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := NewProvider(Config{Provider: tt.provider, APIKey: "test"})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var model string
			switch v := p.(type) {
			case *Claude:
				model = v.Model
			case *OpenAI:
				model = v.Model
			case *Ollama:
				model = v.Model
			}

			if model != tt.expectedModel {
				t.Errorf("expected default model %q, got %q", tt.expectedModel, model)
			}
		})
	}
}

func TestNewProviderOllamaDefaultURL(t *testing.T) {
	p, err := NewProvider(Config{Provider: "ollama"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ollama, ok := p.(*Ollama)
	if !ok {
		t.Fatal("expected *Ollama type")
	}

	if ollama.URL != "http://localhost:11434" {
		t.Errorf("expected default URL http://localhost:11434, got %q", ollama.URL)
	}
}
