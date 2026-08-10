package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Ollama implements Provider using the Ollama REST API.
type Ollama struct {
	URL   string
	Model string
}

func (o *Ollama) Chat(ctx context.Context, system string, messages []Message) (<-chan StreamChunk, error) {
	// Prepend system message
	apiMessages := make([]map[string]string, 0, len(messages)+1)
	apiMessages = append(apiMessages, map[string]string{"role": "system", "content": system})
	for _, m := range messages {
		apiMessages = append(apiMessages, map[string]string{"role": m.Role, "content": m.Content})
	}

	body := map[string]interface{}{
		"model":    o.Model,
		"messages": apiMessages,
		"stream":   true,
	}

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshalling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", o.URL+"/api/chat", bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling Ollama API: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("ollama API returned %d", resp.StatusCode)
	}

	ch := make(chan StreamChunk, 64)

	go func() {
		defer close(ch)
		defer func() { _ = resp.Body.Close() }()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			var event struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				Done bool `json:"done"`
			}

			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				continue
			}

			if event.Message.Content != "" {
				ch <- StreamChunk{Content: event.Message.Content}
			}
			if event.Done {
				ch <- StreamChunk{Done: true}
				return
			}
		}

		if err := scanner.Err(); err != nil {
			ch <- StreamChunk{Err: fmt.Errorf("reading stream: %w", err)}
		}
	}()

	return ch, nil
}
