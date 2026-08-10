package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// rollupKeyField is the cache index on UsageRollup spec.keyPrefix.
const rollupKeyField = "spec.keyPrefix"

// Counter names one of the per-key request counters.
type Counter int

const (
	// CounterAllowed counts admitted requests.
	CounterAllowed Counter = iota
	// CounterDeniedRate counts token-bucket rejections.
	CounterDeniedRate
	// CounterDeniedQuota counts period-quota rejections.
	CounterDeniedQuota
)

type usageKey struct {
	namespace string
	prefix    string
	day       string
}

type usageCounts struct {
	allowed     int64
	deniedRate  int64
	deniedQuota int64
}

// UsageBuffer accumulates per-key counters in memory. The request path only
// touches this buffer; the flusher drains it to UsageRollup CRs in batches.
// Losing a buffer to a crash costs at most one flush window of samples,
// which is the accepted accuracy model.
type UsageBuffer struct {
	mu     sync.Mutex
	counts map[usageKey]*usageCounts
	// floors holds the absolute allowed total this replica last wrote to
	// each rollup. The informer cache observes our own writes with a lag;
	// until it catches up, the quota check would see neither the drained
	// buffer nor the updated rollup and under-count. Reading
	// max(cachedRollup, floor) closes that window; writes are monotonic,
	// so the floor self-heals once the cache catches up.
	floors map[usageKey]int64
}

// NewUsageBuffer returns an empty buffer.
func NewUsageBuffer() *UsageBuffer {
	return &UsageBuffer{
		counts: make(map[usageKey]*usageCounts),
		floors: make(map[usageKey]int64),
	}
}

// Add increments one counter for the key and day.
func (b *UsageBuffer) Add(namespace, prefix, day string, counter Counter) {
	k := usageKey{namespace: namespace, prefix: prefix, day: day}
	b.mu.Lock()
	c, ok := b.counts[k]
	if !ok {
		c = &usageCounts{}
		b.counts[k] = c
	}
	switch counter {
	case CounterAllowed:
		c.allowed++
	case CounterDeniedRate:
		c.deniedRate++
	case CounterDeniedQuota:
		c.deniedQuota++
	}
	b.mu.Unlock()
}

// UnflushedAllowed sums this replica's not-yet-flushed allowed counts for
// the key on or after start. The quota check adds this to the flushed
// rollup totals so a replica cannot over-admit by its own buffer.
func (b *UsageBuffer) UnflushedAllowed(namespace, prefix string, start time.Time) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	var total int64
	for k, c := range b.counts {
		if k.namespace != namespace || k.prefix != prefix {
			continue
		}
		if day, err := time.Parse("2006-01-02", k.day); err == nil && !day.Before(start) {
			total += c.allowed
		}
	}
	return total
}

// floorRetention bounds how long a floor is kept. Floors only bridge the
// informer cache-lag window for the current quota period (the longest is a
// month), so a day older than this can never affect a live quota check. Well
// past a month gives margin without letting the map grow for the life of the
// process — which would also slow every quota check, since AllowedFloors scans
// the whole map under the lock.
const floorRetention = 40 * 24 * time.Hour

// pruneFloors drops floor entries whose day is older than before, or is
// unparseable.
func (b *UsageBuffer) pruneFloors(before time.Time) {
	b.mu.Lock()
	for k := range b.floors {
		if day, err := time.Parse("2006-01-02", k.day); err != nil || day.Before(before) {
			delete(b.floors, k)
		}
	}
	b.mu.Unlock()
}

// setFloor records the absolute allowed total this replica last wrote for
// the rollup.
func (b *UsageBuffer) setFloor(k usageKey, allowedTotal int64) {
	b.mu.Lock()
	if allowedTotal > b.floors[k] {
		b.floors[k] = allowedTotal
	}
	b.mu.Unlock()
}

// AllowedFloors returns the written-but-possibly-unobserved allowed totals
// for the key on or after start, keyed by day.
func (b *UsageBuffer) AllowedFloors(namespace, prefix string, start time.Time) map[string]int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]int64{}
	for k, floor := range b.floors {
		if k.namespace != namespace || k.prefix != prefix {
			continue
		}
		if day, err := time.Parse("2006-01-02", k.day); err == nil && !day.Before(start) {
			out[k.day] = floor
		}
	}
	return out
}

// drain removes and returns all buffered counts.
func (b *UsageBuffer) drain() map[usageKey]*usageCounts {
	b.mu.Lock()
	out := b.counts
	b.counts = make(map[usageKey]*usageCounts)
	b.mu.Unlock()
	return out
}

// restore merges counts back into the buffer after a failed flush so the
// samples are retried instead of dropped.
func (b *UsageBuffer) restore(counts map[usageKey]*usageCounts) {
	b.mu.Lock()
	for k, c := range counts {
		existing, ok := b.counts[k]
		if !ok {
			b.counts[k] = c
			continue
		}
		existing.allowed += c.allowed
		existing.deniedRate += c.deniedRate
		existing.deniedQuota += c.deniedQuota
	}
	b.mu.Unlock()
}

// Flusher drains the usage buffer to UsageRollup CRs on an interval.
// Increments use optimistic concurrency: read, add, update, retry on
// conflict — so concurrent replicas never lose each other's counts.
type Flusher struct {
	client   client.Client
	reader   client.Reader // uncached: read-modify-write must see the latest counters
	buffer   *UsageBuffer
	interval time.Duration
}

// NewFlusher builds a flusher over the buffer. reader must bypass the
// informer cache; a cached read behind the write path loops on conflicts
// against its own stale view.
func NewFlusher(c client.Client, reader client.Reader, buffer *UsageBuffer, interval time.Duration) *Flusher {
	return &Flusher{client: c, reader: reader, buffer: buffer, interval: interval}
}

// Run flushes on the interval until the context ends, with a final flush on
// shutdown.
func (f *Flusher) Run(ctx context.Context) {
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			f.Flush(flushCtx)
			cancel()
			return
		case <-ticker.C:
			f.Flush(ctx)
		}
	}
}

// Flush writes all buffered counts out. Entries that fail are put back for
// the next round.
func (f *Flusher) Flush(ctx context.Context) {
	// Drop floors that have aged out of every quota window before anything
	// else, so the map stays bounded even when no counts are pending.
	f.buffer.pruneFloors(time.Now().Add(-floorRetention))

	counts := f.buffer.drain()
	if len(counts) == 0 {
		return
	}
	failed := make(map[usageKey]*usageCounts)
	for k, c := range counts {
		if err := f.flushOne(ctx, k, c); err != nil {
			log.Printf("authz: flushing rollup %s/%s %s: %v", k.namespace, k.prefix, k.day, err)
			failed[k] = c
			metricFlushFailures.Inc()
		}
	}
	if len(failed) > 0 {
		f.buffer.restore(failed)
	}
}

// RollupName is the deterministic UsageRollup object name for a key and
// day: rollup-<prefix>-<yyyymmdd>. The prefix alphabet is lowercase
// alphanumeric, so the name is always DNS-safe.
func RollupName(prefix, day string) string {
	return fmt.Sprintf("rollup-%s-%s", prefix, dayCompact(day))
}

func dayCompact(day string) string {
	out := make([]byte, 0, len(day))
	for i := 0; i < len(day); i++ {
		if day[i] != '-' {
			out = append(out, day[i])
		}
	}
	return string(out)
}

func (f *Flusher) flushOne(ctx context.Context, k usageKey, c *usageCounts) error {
	name := RollupName(k.prefix, k.day)
	for attempt := 0; attempt < 5; attempt++ {
		var rollup kipperv1.UsageRollup
		err := f.reader.Get(ctx, client.ObjectKey{Namespace: k.namespace, Name: name}, &rollup)
		if errors.IsNotFound(err) {
			rollup = kipperv1.UsageRollup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: k.namespace,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "kipper",
						"kipper.run/api-key":           k.prefix,
					},
				},
				Spec: kipperv1.UsageRollupSpec{
					KeyPrefix:   k.prefix,
					Day:         k.day,
					Allowed:     c.allowed,
					DeniedRate:  c.deniedRate,
					DeniedQuota: c.deniedQuota,
				},
			}
			if createErr := f.client.Create(ctx, &rollup); createErr != nil {
				if errors.IsAlreadyExists(createErr) {
					continue // another replica raced the create; add to it
				}
				return createErr
			}
			f.buffer.setFloor(k, rollup.Spec.Allowed)
			return nil
		}
		if err != nil {
			return err
		}

		rollup.Spec.Allowed += c.allowed
		rollup.Spec.DeniedRate += c.deniedRate
		rollup.Spec.DeniedQuota += c.deniedQuota
		err = f.client.Update(ctx, &rollup)
		if err == nil {
			f.buffer.setFloor(k, rollup.Spec.Allowed)
			return nil
		}
		if !errors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("gave up after repeated conflicts")
}
