package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/getkipper/kipper/console-api/ai"
)

const logAnalysisPrompt = `You are the Kipper AI log analyst. The user is viewing logs from a Kubernetes application and needs help understanding what went wrong.

Analyse the logs and provide:
1. A clear summary of what happened (1-2 sentences)
2. The root cause if identifiable
3. A suggested fix with specific actions

Be concise and direct. Use bullet points. If the logs look normal, say so.
Do not repeat the logs back. Focus on actionable insights.

Application: %s
Namespace: %s`

// AILogs handles AI-powered log analysis.
type AILogs struct {
	Settings *AISettings
}

type analyseLogsRequest struct {
	Logs      string `json:"logs"`
	AppName   string `json:"app_name"`
	Namespace string `json:"namespace"`
}

// AnalyseLogs streams an AI analysis of application logs via SSE.
// POST /api/v1/ai/analyse-logs
func (a *AILogs) AnalyseLogs(w http.ResponseWriter, r *http.Request) {
	var req analyseLogsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Logs == "" {
		respondError(w, http.StatusBadRequest, "logs required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	cfg, err := a.Settings.GetRaw(ctx)
	if err != nil || cfg.Provider == "" {
		respondError(w, http.StatusBadRequest, "AI not configured — go to Settings to add a provider")
		return
	}

	provider, err := ai.NewProvider(ai.Config{
		Provider:  cfg.Provider,
		APIKey:    cfg.APIKey,
		Model:     cfg.Model,
		OllamaURL: cfg.OllamaURL,
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create AI provider: %v", err))
		return
	}

	system := fmt.Sprintf(logAnalysisPrompt, req.AppName, req.Namespace)

	// Truncate logs to avoid exceeding token limits
	logs := req.Logs
	if len(logs) > 15000 {
		logs = logs[len(logs)-15000:]
	}

	messages := []ai.Message{
		{Role: "user", Content: "Analyse these logs:\n\n```\n" + logs + "\n```"},
	}

	stream, err := provider.Chat(ctx, system, messages)
	if err != nil {
		respondError(w, http.StatusBadGateway, fmt.Sprintf("AI provider error: %v", err))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		respondError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	for chunk := range stream {
		if chunk.Err != nil {
			_, _ = fmt.Fprintf(w, "data: {\"error\": %q}\n\n", chunk.Err.Error())
			flusher.Flush()
			return
		}

		if chunk.Done {
			_, _ = fmt.Fprintf(w, "data: {\"done\": true}\n\n")
			flusher.Flush()
			return
		}

		data, _ := json.Marshal(map[string]string{"content": chunk.Content})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
}
