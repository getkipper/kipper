package handlers

import (
	"context"
	"math"
	"sort"
	"strconv"
	"sync"
)

// Limit concurrent jobs per request; detail jobs issue their queries sequentially.
const maxConcurrentQueries = 6

func promString(s string) string {
	return strconv.Quote(s)
}

// alignSamples places samples on the shared timestamps. A timestamp without a
// sample, or with a NaN or infinite value, is nil.
func alignSamples(samples []PromSample, timestamps []int64) []*float64 {
	byTime := make(map[int64]float64, len(samples))
	for _, s := range samples {
		byTime[s.Time.Unix()] = s.Value
	}
	out := make([]*float64, len(timestamps))
	for i, ts := range timestamps {
		v, ok := byTime[ts]
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		value := v
		out[i] = &value
	}
	return out
}

// A job may issue several queries; name identifies its group in degraded results.
type queryJob struct {
	name string
	run  func(ctx context.Context) error
}

// runQueryPool limits concurrency and passes ctx to each job. It returns sorted,
// distinct names for jobs that report errors or are cancelled while waiting.
func runQueryPool(ctx context.Context, jobs []queryJob) []string {
	sem := make(chan struct{}, maxConcurrentQueries)
	var mu sync.Mutex
	failed := map[string]bool{}
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func(job queryJob) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				failed[job.name] = true
				mu.Unlock()
				return
			}
			defer func() { <-sem }()
			if err := job.run(ctx); err != nil {
				mu.Lock()
				failed[job.name] = true
				mu.Unlock()
			}
		}(job)
	}
	wg.Wait()
	names := make([]string, 0, len(failed))
	for n := range failed {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
