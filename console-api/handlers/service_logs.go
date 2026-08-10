package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"k8s.io/client-go/kubernetes"
)

// ServiceLogs provides log querying for services via Loki.
type ServiceLogs struct {
	Client kubernetes.Interface
}

// Query returns logs from Loki for a service.
// GET /api/v1/services/{name}/logs?namespace={ns}&search=&since=1h&limit=500
func (sl *ServiceLogs) Query(w http.ResponseWriter, r *http.Request) {
	name, namespace, ok := requireService(w, r)
	if !ok {
		return
	}

	search := r.URL.Query().Get("search")
	since := r.URL.Query().Get("since")
	limit := r.URL.Query().Get("limit")

	if since == "" {
		since = "1h"
	}
	if limit == "" {
		limit = "500"
	}

	// Build LogQL query
	query := fmt.Sprintf(`{namespace=%q, app=%q}`, namespace, name)
	if search != "" {
		query += fmt.Sprintf(` |= %q`, search)
	}

	duration, err := time.ParseDuration(since)
	if err != nil {
		duration = 1 * time.Hour
	}
	start := time.Now().Add(-duration)
	end := time.Now()
	step := duration / 1000
	if step < time.Second {
		step = time.Second
	}

	params := url.Values{
		"query":     {query},
		"start":     {fmt.Sprintf("%d", start.UnixNano())},
		"end":       {fmt.Sprintf("%d", end.UnixNano())},
		"limit":     {limit},
		"direction": {"backward"},
		"step":      {fmt.Sprintf("%d", int(step.Seconds()))},
	}

	lokiReqURL := fmt.Sprintf("%s/loki/api/v1/query_range?%s", lokiURL, params.Encode())

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(lokiReqURL) //nolint:gosec // internal cluster URL
	if err != nil {
		respondError(w, http.StatusBadGateway, fmt.Sprintf("failed to query Loki: %v", err))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		respondError(w, http.StatusBadGateway, fmt.Sprintf("Loki returned %d: %s", resp.StatusCode, string(body)))
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		respondError(w, http.StatusBadGateway, "failed to read Loki response")
		return
	}

	var lokiResp lokiQueryResponse
	if err := json.Unmarshal(body, &lokiResp); err != nil {
		respondError(w, http.StatusBadGateway, fmt.Sprintf("failed to parse Loki response: %v (body: %.200s)", err, string(body)))
		return
	}

	if lokiResp.Status != "success" {
		respondError(w, http.StatusBadGateway, fmt.Sprintf("Loki query failed: %s", lokiResp.Status))
		return
	}

	var entries []logEntry
	for _, stream := range lokiResp.Data.Result {
		pod := stream.Stream["pod"]
		for _, val := range stream.Values {
			if len(val) < 2 {
				continue
			}
			entries = append(entries, logEntry{
				Timestamp: val[0],
				Line:      val[1],
				Pod:       pod,
			})
		}
	}

	if entries == nil {
		entries = []logEntry{}
	}

	respondJSON(w, http.StatusOK, entries)
}
