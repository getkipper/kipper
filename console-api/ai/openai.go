package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// OpenAI implements Provider using the OpenAI Chat Completions API.
type OpenAI struct {
	APIKey string
	Model  string
}

func (o *OpenAI) Chat(ctx context.Context, system string, messages []Message) (<-chan StreamChunk, error) {
	// Prepend system message
	apiMessages := make([]map[string]string, 0, len(messages)+1)
	apiMessages = append(apiMessages, map[string]string{"role": "system", "content": system})
	for _, m := range messages {
		apiMessages = append(apiMessages, map[string]string{"role": m.Role, "content": m.Content})
	}

	body := map[string]interface{}{
		"model":      o.Model,
		"max_tokens": 16384,
		"stream":     true,
		"messages":   apiMessages,
	}

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshalling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.APIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling OpenAI API: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("openai API returned %d", resp.StatusCode)
	}

	ch := make(chan StreamChunk, 64)

	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()

			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := line[6:]
			if data == "[DONE]" {
				ch <- StreamChunk{Done: true}
				return
			}

			var event struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
			}

			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}

			if len(event.Choices) > 0 {
				if event.Choices[0].Delta.Content != "" {
					ch <- StreamChunk{Content: event.Choices[0].Delta.Content}
				}
				if event.Choices[0].FinishReason != nil {
					if *event.Choices[0].FinishReason == "length" {
						ch <- StreamChunk{Content: "\n\n**Output truncated** — the response hit the token limit. Try a simpler prompt or break your request into smaller parts."}
					}
					ch <- StreamChunk{Done: true}
					return
				}
			}
		}

		if err := scanner.Err(); err != nil {
			ch <- StreamChunk{Err: fmt.Errorf("reading stream: %w", err)}
		}
	}()

	return ch, nil
}
