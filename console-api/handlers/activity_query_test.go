package handlers

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPromString(t *testing.T) {
	assert.Equal(t, `"shop-prod"`, promString("shop-prod"))
	assert.Equal(t, `"a\"b\\c"`, promString(`a"b\c`))
}

func TestAlignSamples(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	timestamps := []int64{base.Unix(), base.Add(30 * time.Second).Unix(), base.Add(60 * time.Second).Unix(), base.Add(90 * time.Second).Unix()}
	samples := []PromSample{
		{Time: base, Value: 1.5},
		{Time: base.Add(60 * time.Second), Value: math.NaN()},
		{Time: base.Add(90 * time.Second), Value: math.Inf(1)},
	}

	got := alignSamples(samples, timestamps)

	require.Len(t, got, 4)
	require.NotNil(t, got[0])
	assert.Equal(t, 1.5, *got[0])
	assert.Nil(t, got[1], "no sample is a null")
	assert.Nil(t, got[2], "NaN is a null")
	assert.Nil(t, got[3], "Inf is a null")
}

func TestQueryPoolBoundsConcurrencyAndDegradesFailures(t *testing.T) {
	var running, peak atomic.Int32
	jobs := make([]queryJob, 0, 20)
	for i := 0; i < 20; i++ {
		name := "ok"
		if i == 3 {
			name = "broken"
		}
		jobs = append(jobs, queryJob{name: name, run: func(ctx context.Context) error {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			running.Add(-1)
			if name == "broken" {
				return errors.New("prometheus 500")
			}
			return nil
		}})
	}

	degraded := runQueryPool(context.Background(), jobs)

	assert.LessOrEqual(t, peak.Load(), int32(maxConcurrentQueries))
	assert.Equal(t, []string{"broken"}, degraded)
}

func TestQueryPoolStopsAtTheDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	jobs := []queryJob{{name: "slow", run: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}}

	assert.Equal(t, []string{"slow"}, runQueryPool(ctx, jobs))
}
