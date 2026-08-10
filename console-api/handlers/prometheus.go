package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// defaultPromURL is the in-cluster service the kube-prometheus-stack
// chart's prometheus-operator creates for every Prometheus CR. Sidesteps
// the chart-version-dependent service name (which has changed twice
// upstream) by going through the operator's stable headless service.
const defaultPromURL = "http://prometheus-operated.monitoring.svc.cluster.local:9090"

// PromSample is one (timestamp, value) point from a PromQL response.
type PromSample struct {
	Time  time.Time
	Value float64
}

// PromQueryRangeFunc fetches a time-series over [start, end] at the given
// step. Returns a flat list of samples, or an error if the query fails or
// Prometheus is unreachable.
type PromQueryRangeFunc func(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]PromSample, error)

// PromQueryInstantFunc evaluates a PromQL expression at a single point in
// time. Returns NaN when the expression yields no data.
type PromQueryInstantFunc func(ctx context.Context, query string, at time.Time) (float64, error)

// PromSeries is one labelled time-series from a matrix response.
type PromSeries struct {
	Labels  map[string]string
	Samples []PromSample
}

// PromQueryRangeSeriesFunc fetches a time-series over [start, end] and keeps
// every series with its labels, for queries that group by label (e.g. one
// series per workload) rather than summing to a single line.
type PromQueryRangeSeriesFunc func(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]PromSeries, error)

// PromVectorSample is one labelled value from an instant vector response.
type PromVectorSample struct {
	Labels map[string]string
	Value  float64
}

// PromQueryInstantVecFunc evaluates a PromQL expression at a single point in
// time and keeps every result with its labels, for grouped queries (e.g.
// sum by (service)).
type PromQueryInstantVecFunc func(ctx context.Context, query string, at time.Time) ([]PromVectorSample, error)

// realPromQueryRange hits Prometheus' /api/v1/query_range with the
// standard JSON contract. The handler short-circuits the call when
// PrometheusBaseURL is empty.
func realPromQueryRange(client *http.Client, baseURL string) PromQueryRangeFunc {
	return func(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]PromSample, error) {
		params := url.Values{}
		params.Set("query", query)
		params.Set("start", strconv.FormatInt(start.Unix(), 10))
		params.Set("end", strconv.FormatInt(end.Unix(), 10))
		params.Set("step", strconv.FormatInt(int64(step.Seconds()), 10)+"s")

		reqURL := baseURL + "/api/v1/query_range?" + params.Encode()
		return doMatrixQuery(ctx, client, reqURL)
	}
}

// realPromQueryInstant hits /api/v1/query — used for the throttling
// ratio, which is a single scalar at "now".
func realPromQueryInstant(client *http.Client, baseURL string) PromQueryInstantFunc {
	return func(ctx context.Context, query string, at time.Time) (float64, error) {
		params := url.Values{}
		params.Set("query", query)
		params.Set("time", strconv.FormatInt(at.Unix(), 10))

		reqURL := baseURL + "/api/v1/query?" + params.Encode()
		return doScalarQuery(ctx, client, reqURL)
	}
}

// realPromQueryRangeSeries hits /api/v1/query_range and keeps every returned
// series, for label-grouped queries such as per-workload usage.
func realPromQueryRangeSeries(client *http.Client, baseURL string) PromQueryRangeSeriesFunc {
	return func(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]PromSeries, error) {
		params := url.Values{}
		params.Set("query", query)
		params.Set("start", strconv.FormatInt(start.Unix(), 10))
		params.Set("end", strconv.FormatInt(end.Unix(), 10))
		params.Set("step", strconv.FormatInt(int64(step.Seconds()), 10)+"s")

		reqURL := baseURL + "/api/v1/query_range?" + params.Encode()
		return doMatrixSeriesQuery(ctx, client, reqURL)
	}
}

// realPromQueryInstantVec hits /api/v1/query and keeps every returned vector
// element with its labels.
func realPromQueryInstantVec(client *http.Client, baseURL string) PromQueryInstantVecFunc {
	return func(ctx context.Context, query string, at time.Time) ([]PromVectorSample, error) {
		params := url.Values{}
		params.Set("query", query)
		params.Set("time", strconv.FormatInt(at.Unix(), 10))

		reqURL := baseURL + "/api/v1/query?" + params.Encode()
		return doVectorQuery(ctx, client, reqURL)
	}
}

func doVectorQuery(ctx context.Context, client *http.Client, reqURL string) ([]PromVectorSample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("prometheus %d: %s", resp.StatusCode, string(body))
	}

	var parsed promResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("prometheus status %q", parsed.Status)
	}

	out := make([]PromVectorSample, 0, len(parsed.Data.Result))
	for _, series := range parsed.Data.Result {
		if len(series.Value) != 2 {
			continue
		}
		valStr, _ := series.Value[1].(string)
		v, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		out = append(out, PromVectorSample{Labels: series.Metric, Value: v})
	}
	return out, nil
}

func doMatrixSeriesQuery(ctx context.Context, client *http.Client, reqURL string) ([]PromSeries, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("prometheus %d: %s", resp.StatusCode, string(body))
	}

	var parsed promResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("prometheus status %q", parsed.Status)
	}

	out := make([]PromSeries, 0, len(parsed.Data.Result))
	for _, series := range parsed.Data.Result {
		samples := make([]PromSample, 0, len(series.Values))
		for _, pair := range series.Values {
			if len(pair) != 2 {
				continue
			}
			ts, _ := pair[0].(float64)
			valStr, _ := pair[1].(string)
			v, err := strconv.ParseFloat(valStr, 64)
			if err != nil {
				continue
			}
			samples = append(samples, PromSample{Time: time.Unix(int64(ts), 0), Value: v})
		}
		out = append(out, PromSeries{Labels: series.Metric, Samples: samples})
	}
	return out, nil
}

func doMatrixQuery(ctx context.Context, client *http.Client, reqURL string) ([]PromSample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("prometheus %d: %s", resp.StatusCode, string(body))
	}

	var parsed promResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("prometheus status %q", parsed.Status)
	}
	if len(parsed.Data.Result) == 0 {
		return nil, nil
	}
	// We sum across series in the handler, so for a matrix response the
	// caller usually expects the first series — they ran a sum() query.
	series := parsed.Data.Result[0]
	out := make([]PromSample, 0, len(series.Values))
	for _, pair := range series.Values {
		if len(pair) != 2 {
			continue
		}
		ts, _ := pair[0].(float64)
		valStr, _ := pair[1].(string)
		v, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		out = append(out, PromSample{
			Time:  time.Unix(int64(ts), 0),
			Value: v,
		})
	}
	return out, nil
}

func doScalarQuery(ctx context.Context, client *http.Client, reqURL string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("prometheus %d: %s", resp.StatusCode, string(body))
	}

	var parsed promResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return 0, err
	}
	if parsed.Status != "success" || len(parsed.Data.Result) == 0 {
		return 0, nil
	}
	first := parsed.Data.Result[0]
	if len(first.Value) != 2 {
		return 0, nil
	}
	valStr, _ := first.Value[1].(string)
	v, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// promResponse mirrors the shape both query and query_range return. The
// resultType is "matrix" for range queries (Values) and "vector" or
// "scalar" for instant queries (Value).
type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string       `json:"resultType"`
		Result     []promSeries `json:"result"`
	} `json:"data"`
}

type promSeries struct {
	Metric map[string]string `json:"metric"`
	Value  []interface{}     `json:"value,omitempty"`
	Values [][]interface{}   `json:"values,omitempty"`
}
