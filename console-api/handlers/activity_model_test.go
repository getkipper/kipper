package handlers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/console-api/controllers"
)

func f(v float64) *float64 { return &v }

func TestActivityRanges(t *testing.T) {
	for name, want := range map[string]time.Duration{"1h": 30 * time.Second, "6h": 2 * time.Minute, "24h": 10 * time.Minute, "3d": 30 * time.Minute} {
		r, ok := activityRangeFor(name)
		require.True(t, ok, name)
		assert.Equal(t, want, r.step, name)
		assert.LessOrEqual(t, len(r.timestamps(time.Unix(1_791_151_234, 0))), 181, name)
	}
	_, ok := activityRangeFor("7d")
	assert.False(t, ok)
}

func TestActivityTimestampsAlignToTheStep(t *testing.T) {
	r, _ := activityRangeFor("1h")
	ts := r.timestamps(time.Unix(1_791_151_234, 0))
	assert.Equal(t, int64(0), ts[len(ts)-1]%30, "the last point sits on the step")
	assert.Equal(t, int64(3600), ts[len(ts)-1]-ts[0])
}

func TestEveryRangeEndsWhereDetectionEnds(t *testing.T) {
	now := time.Unix(1_791_151_234, 0)
	detect := activityRange{span: 72 * time.Hour, step: detectionStep}.timestamps(now)
	for _, r := range activityRanges {
		ts := r.timestamps(now)
		assert.Equal(t, detect[len(detect)-1], ts[len(ts)-1], "%s: a recent change must fall inside the chart", r.name)
		assert.Equal(t, int64(r.span.Seconds()), ts[len(ts)-1]-ts[0], r.name)
	}
}

func TestRateWindow(t *testing.T) {
	assert.Equal(t, 2*time.Minute, rateWindow(30*time.Second))
	assert.Equal(t, 30*time.Minute+30*time.Second, rateWindow(30*time.Minute))
}

func TestStatusClass(t *testing.T) {
	cases := map[string]string{"200": "ok", "204": "ok", "301": "redirect", "404": "client_error", "499": "aborted", "500": "server_error", "101": "", "abc": ""}
	for code, want := range cases {
		assert.Equal(t, want, statusClass(code), code)
	}
}

func TestAttributedNeedsTheWholeInputWindowInsideOneSpan(t *testing.T) {
	base := time.Unix(1_791_000_000, 0)
	spans := []controllers.AttributionSpan{{From: base, To: base.Add(time.Hour)}, {From: base.Add(2 * time.Hour), To: base.Add(3 * time.Hour)}}
	assert.True(t, attributed(base.Add(30*time.Minute).Unix(), 10*time.Minute, spans))
	assert.False(t, attributed(base.Add(5*time.Minute).Unix(), 10*time.Minute, spans), "its window starts before the span")
	assert.False(t, attributed(base.Add(2*time.Hour+5*time.Minute).Unix(), 10*time.Minute, spans), "its window crosses a gap")
	assert.False(t, attributed(base.Add(4*time.Hour).Unix(), time.Minute, spans))
}

func TestDetectReplicaChanges(t *testing.T) {
	ts := []int64{0, 30, 60, 90, 120, 150, 180}
	replicas := []*float64{f(2), f(2), f(3), f(3), nil, f(2), f(2)}

	got := detectReplicaChanges(ts, replicas)

	require.Len(t, got, 1, "a gap is a boundary, never a change")
	assert.Equal(t, int64(60), got[0].Time)
	assert.Equal(t, 2, got[0].From)
	assert.Equal(t, 3, got[0].To)
}

func TestChangeCause(t *testing.T) {
	ts := []int64{0, 30, 60, 90, 120}
	hpa := []*float64{f(1), f(1), f(1), f(1), f(1)}
	noHPA := []*float64{nil, nil, f(1), f(1), f(1)}
	minAt := func(v ...float64) []*float64 {
		out := make([]*float64, len(v))
		for i := range v {
			out[i] = f(v[i])
		}
		return out
	}
	steady := minAt(2, 2, 2, 2, 2)
	maxes := minAt(5, 5, 5, 5, 5)
	in := causeInputs{timestamps: ts, hpa: hpa, minReplicas: steady, maxReplicas: maxes}

	assert.Equal(t, causeToZero, in.cause(replicaChange{Time: 60, From: 2, To: 0}, nil, map[int]bool{}))
	assert.Equal(t, causeFromZero, in.cause(replicaChange{Time: 60, From: 0, To: 2}, nil, map[int]bool{}))

	raised := in
	raised.minReplicas = minAt(2, 2, 3, 3, 3)
	assert.Equal(t, causeBoundsChange, raised.cause(replicaChange{Time: 60, From: 2, To: 3}, nil, map[int]bool{}))

	events := []rescaleEvent{{Time: time.Unix(75, 0), NewSize: 3}}
	assert.Equal(t, causeAutoscaler, in.cause(replicaChange{Time: 60, From: 2, To: 3}, events, map[int]bool{}))
	assert.Equal(t, causeWhileAutoscaling, in.cause(replicaChange{Time: 60, From: 2, To: 4}, events, map[int]bool{}), "an event naming another count is no evidence")
	assert.Equal(t, causeWhileAutoscaling, in.cause(replicaChange{Time: 60, From: 2, To: 3}, nil, map[int]bool{}))

	without := in
	without.hpa = noHPA
	assert.Equal(t, causeUnknown, without.cause(replicaChange{Time: 60, From: 2, To: 3}, events, map[int]bool{}), "no autoscaler on one side")
}

func TestParseRescaleEvent(t *testing.T) {
	n, ok := rescaleSize("New size: 3; reason: cpu resource utilization (percentage of request) above target")
	assert.True(t, ok)
	assert.Equal(t, 3, n)
	_, ok = rescaleSize("something else")
	assert.False(t, ok)
}

func TestAroundWindow(t *testing.T) {
	assert.Equal(t, 2*time.Minute, aroundWindow(replicaChange{From: 2, To: 3}))
	assert.Equal(t, 5*time.Minute, aroundWindow(replicaChange{From: 3, To: 2}))
}

func TestABoundChangeMoreThanAMinuteBeforeIsNotTheCause(t *testing.T) {
	v := func(f float64) *float64 { return &f }
	ts := []int64{0, 30, 60, 90, 120, 150, 180}
	one := v(1)
	in := causeInputs{
		timestamps:  ts,
		hpa:         []*float64{one, one, one, one, one, one, one},
		minReplicas: []*float64{v(2), v(3), v(3), v(3), v(3), v(3), v(3)},
		maxReplicas: []*float64{v(5), v(5), v(5), v(5), v(5), v(5), v(5)},
	}
	assert.Equal(t, causeWhileAutoscaling, in.cause(replicaChange{Time: 120, From: 2, To: 3}, nil, map[int]bool{}), "the minimum changed 90s before")
	assert.Equal(t, causeBoundsChange, in.cause(replicaChange{Time: 90, From: 2, To: 3}, nil, map[int]bool{}), "the minimum changed 60s before")
}

func TestAutoscaledAcrossNeedsTheAutoscalerOnBothSides(t *testing.T) {
	one := 1.0
	in := causeInputs{timestamps: []int64{0, 30, 60}, hpa: []*float64{nil, &one, &one}}
	assert.False(t, in.autoscaledAcross(replicaChange{Time: 30, From: 0, To: 2}), "the autoscaler appeared with the change")
	assert.True(t, in.autoscaledAcross(replicaChange{Time: 60, From: 2, To: 0}))
}

func TestOneEventExplainsOneChange(t *testing.T) {
	one := 1.0
	ts := []int64{0, 30, 60, 90, 120}
	in := causeInputs{timestamps: ts, hpa: []*float64{&one, &one, &one, &one, &one}}
	events := []rescaleEvent{{Time: time.Unix(60, 0), NewSize: 3}}
	used := map[int]bool{}
	first := in.cause(replicaChange{Time: 30, From: 2, To: 3}, events, used)
	second := in.cause(replicaChange{Time: 90, From: 2, To: 3}, events, used)
	assert.ElementsMatch(t, []string{causeAutoscaler, causeWhileAutoscaling}, []string{first, second})
}
