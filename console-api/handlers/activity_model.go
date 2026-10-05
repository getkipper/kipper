package handlers

import (
	"regexp"
	"strconv"
	"time"

	"github.com/getkipper/kipper/console-api/controllers"
)

type activityRange struct {
	name string
	span time.Duration
	step time.Duration
}

var activityRanges = []activityRange{
	{name: "1h", span: time.Hour, step: 30 * time.Second},
	{name: "6h", span: 6 * time.Hour, step: 2 * time.Minute},
	{name: "24h", span: 24 * time.Hour, step: 10 * time.Minute},
	{name: "3d", span: 72 * time.Hour, step: 30 * time.Minute},
}

func activityRangeFor(name string) (activityRange, bool) {
	for _, r := range activityRanges {
		if r.name == name {
			return r, true
		}
	}
	return activityRange{}, false
}

// End every range on the same 30s grid as change detection so recent markers
// fall inside the chart.
func (r activityRange) timestamps(now time.Time) []int64 {
	step := int64(r.step.Seconds())
	end := now.Unix() / int64(detectionStep.Seconds()) * int64(detectionStep.Seconds())
	start := end - int64(r.span.Seconds())
	out := make([]int64, 0, int(r.span/r.step)+1)
	for t := start; t <= end; t += step {
		out = append(out, t)
	}
	return out
}

// Overlap rate windows and allow at least four 30s scrape intervals to tolerate
// an occasional missed scrape.
func rateWindow(step time.Duration) time.Duration {
	w := step + 30*time.Second
	if w < 2*time.Minute {
		return 2 * time.Minute
	}
	return w
}

// statusClass groups a Traefik status code. 499 is a request the client gave
// up on; 1xx is left out.
func statusClass(code string) string {
	n, err := strconv.Atoi(code)
	if err != nil {
		return ""
	}
	switch {
	case n == 499:
		return "aborted"
	case n >= 200 && n < 300:
		return "ok"
	case n >= 300 && n < 400:
		return "redirect"
	case n >= 400 && n < 500:
		return "client_error"
	case n >= 500 && n < 600:
		return "server_error"
	}
	return ""
}

// attributed reports whether the input window [ts-lookback, ts] lies inside
// one span, so the value computed from it is the app's traffic alone.
func attributed(ts int64, lookback time.Duration, spans []controllers.AttributionSpan) bool {
	end := time.Unix(ts, 0)
	start := end.Add(-lookback)
	for _, s := range spans {
		if !start.Before(s.From) && !end.After(s.To) {
			return true
		}
	}
	return false
}

// replicaChange is a change of the Deployment's desired count, timed to the
// first 30s sample that shows the new count.
type replicaChange struct {
	Time int64
	From int
	To   int
}

// detectReplicaChanges finds where consecutive samples differ. A missing
// sample is a boundary, never a change.
func detectReplicaChanges(timestamps []int64, replicas []*float64) []replicaChange {
	var out []replicaChange
	for i := 1; i < len(replicas) && i < len(timestamps); i++ {
		prev, cur := replicas[i-1], replicas[i]
		if prev == nil || cur == nil || *prev == *cur {
			continue
		}
		out = append(out, replicaChange{Time: timestamps[i], From: int(*prev), To: int(*cur)})
	}
	return out
}

// Causes of a change, as far as the recorded data shows. Only causeAutoscaler
// names an actor, and only with a matching event.
const (
	causeToZero           = "to_zero"
	causeFromZero         = "from_zero"
	causeBoundsChange     = "bounds_change"
	causeAutoscaler       = "autoscaler"
	causeWhileAutoscaling = "while_autoscaling"
	causeUnknown          = "unknown"
)

// causeWindow is how far a bound change or an event may be from a replica
// change and still count as part of it.
const causeWindow = 60 * time.Second

// rescaleEvent is a SuccessfulRescale event of the app's autoscaler.
type rescaleEvent struct {
	Time    time.Time
	NewSize int
}

var rescalePattern = regexp.MustCompile(`New size: (\d+);`)

// rescaleSize reads the new count from a SuccessfulRescale event message.
func rescaleSize(message string) (int, bool) {
	m := rescalePattern.FindStringSubmatch(message)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// causeInputs are the 30s series a change's cause is read from: whether an
// autoscaler existed, and its bounds.
type causeInputs struct {
	timestamps  []int64
	hpa         []*float64
	minReplicas []*float64
	maxReplicas []*float64
}

func (in causeInputs) indexAt(ts int64) int {
	for i, t := range in.timestamps {
		if t == ts {
			return i
		}
	}
	return -1
}

func valueAt(series []*float64, i int) *float64 {
	if i < 0 || i >= len(series) {
		return nil
	}
	return series[i]
}

// boundChangedTo reports whether a bound series changed within causeWindow of
// the change and now equals to.
func (in causeInputs) boundChangedTo(series []*float64, i, to int) bool {
	window := int(causeWindow / (30 * time.Second))
	for j := i - window; j <= i+window; j++ {
		a, b := valueAt(series, j-1), valueAt(series, j)
		if a != nil && b != nil && *a != *b && int(*b) == to {
			return true
		}
	}
	return false
}

// autoscaledAcross reports whether the app had an autoscaler at the samples on
// both sides of a change.
func (in causeInputs) autoscaledAcross(c replicaChange) bool {
	i := in.indexAt(c.Time)
	return valueAt(in.hpa, i-1) != nil && valueAt(in.hpa, i) != nil
}

// Zero transitions and matching bound changes take precedence over event matching.
// used tracks matched event indexes so each event explains at most one change.
func (in causeInputs) cause(c replicaChange, events []rescaleEvent, used map[int]bool) string {
	switch {
	case c.To == 0:
		return causeToZero
	case c.From == 0:
		return causeFromZero
	}
	i := in.indexAt(c.Time)
	if in.boundChangedTo(in.minReplicas, i, c.To) || in.boundChangedTo(in.maxReplicas, i, c.To) {
		return causeBoundsChange
	}
	// Require HPA samples on both sides before attributing the count change.
	if !in.autoscaledAcross(c) {
		return causeUnknown
	}
	at := time.Unix(c.Time, 0)
	best, bestGap := -1, causeWindow
	for k, e := range events {
		gap := e.Time.Sub(at).Abs()
		if used[k] || e.NewSize != c.To || gap > bestGap || (best >= 0 && gap == bestGap) {
			continue
		}
		best, bestGap = k, gap
	}
	if best < 0 {
		return causeWhileAutoscaling
	}
	used[best] = true
	return causeAutoscaler
}

// Use a short lookback for scale-out and a longer one for scale-in to reflect
// the default HPA scale-down stabilization window.
func aroundWindow(c replicaChange) time.Duration {
	if c.To > c.From {
		return 2 * time.Minute
	}
	return maxAroundWindow
}

const maxAroundWindow = 5 * time.Minute
