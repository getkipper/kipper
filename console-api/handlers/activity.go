package handlers

import (
	"context"
	"math"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
)

const (
	// activityDeadline covers every Prometheus query of one request.
	activityDeadline = 8 * time.Second
	// Keep change detection at 30s even when the chart uses coarser steps.
	detectionStep      = 30 * time.Second
	maxDetailedChanges = 20
	// Allow a 30s kube-state-metrics scrape interval plus 30s of jitter.
	ksmScrapeMargin = 60 * time.Second
)

// Activity serves the traffic and scaling view of one app.
type Activity struct {
	CRClient             crclient.Client
	Client               kubernetes.Interface
	PrometheusBaseURL    string
	PromQueryRangeSeries PromQueryRangeSeriesFunc
	PromQueryInstantVec  PromQueryInstantVecFunc
	Now                  func() time.Time
}

type activityRequests struct {
	OK          []*float64 `json:"ok"`
	Redirect    []*float64 `json:"redirect"`
	ClientError []*float64 `json:"client_error"`
	Aborted     []*float64 `json:"aborted"`
	ServerError []*float64 `json:"server_error"`
}

type activityPolicy struct {
	CPUTargetPct    []*float64 `json:"cpu_target_pct"`
	MemoryTargetPct []*float64 `json:"memory_target_pct"`
	Min             []*float64 `json:"min"`
	Max             []*float64 `json:"max"`
}

type aroundMetric struct {
	PeakPct   *float64 `json:"peak_pct"`
	AvgPct    *float64 `json:"avg_pct"`
	TargetPct *float64 `json:"target_pct"`
}

type aroundRequests struct {
	Total       float64 `json:"total"`
	NotFound    float64 `json:"not_found"`
	Aborted     float64 `json:"aborted"`
	ServerError float64 `json:"server_error"`
	Other       float64 `json:"other"`
}

type activityAround struct {
	WindowSeconds int             `json:"window_seconds"`
	CPU           *aroundMetric   `json:"cpu"`
	Memory        *aroundMetric   `json:"memory"`
	Requests      *aroundRequests `json:"requests"`
}

type activityChange struct {
	Time              int64           `json:"time"`
	From              int             `json:"from"`
	To                int             `json:"to"`
	Cause             string          `json:"cause"`
	Autoscaled        bool            `json:"autoscaled"`
	Around            *activityAround `json:"around"`
	AroundUnavailable string          `json:"around_unavailable,omitempty"`
}

type activityResponse struct {
	Available            bool              `json:"available"`
	Reason               string            `json:"reason,omitempty"`
	Range                string            `json:"range,omitempty"`
	StepSeconds          int64             `json:"step_seconds,omitempty"`
	Timestamps           []int64           `json:"timestamps,omitempty"`
	Traffic              string            `json:"traffic,omitempty"`
	RequestsPerMin       *activityRequests `json:"requests_per_min,omitempty"`
	RequestsPeakPerMin   []*float64        `json:"requests_peak_per_min,omitempty"`
	CPUPctOfRequest      []*float64        `json:"cpu_pct_of_request,omitempty"`
	CPUPeakPctOfRequest  []*float64        `json:"cpu_peak_pct_of_request,omitempty"`
	CPUUsedMillis        []*float64        `json:"cpu_used_millis,omitempty"`
	CPURequestedMillis   []*float64        `json:"cpu_requested_millis,omitempty"`
	MemoryPctOfRequest   []*float64        `json:"memory_pct_of_request,omitempty"`
	MemoryUsedBytes      []*float64        `json:"memory_used_bytes,omitempty"`
	MemoryRequestedBytes []*float64        `json:"memory_requested_bytes,omitempty"`
	Replicas             []*float64        `json:"replicas,omitempty"`
	Policy               *activityPolicy   `json:"policy,omitempty"`
	Changes              []activityChange  `json:"changes"`
	ChangesDetailed      int               `json:"changes_detailed"`
	Degraded             []string          `json:"degraded"`
}

// Get returns activity charts, detected pod-count changes and available
// measurements preceding recent changes.
func (h *Activity) Get(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "name")
	name := chi.URLParam(r, "app")
	rangeName := r.URL.Query().Get("range")
	if rangeName == "" {
		rangeName = "1h"
	}
	rng, ok := activityRangeFor(rangeName)
	if !ok {
		respondError(w, http.StatusBadRequest, "range must be one of 1h, 6h, 24h or 3d")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	var app kipperv1.App
	if err := h.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, &app); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, "app not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}
	var ns corev1.Namespace
	if err := h.CRClient.Get(ctx, crclient.ObjectKey{Name: namespace}, &ns); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read the project")
		return
	}
	since := ns.CreationTimestamp.Time
	if !prometheusComponentEnabled(ctx, h.CRClient) {
		respondJSON(w, http.StatusOK, activityResponse{Reason: "Monitoring is not enabled on this cluster.", Changes: []activityChange{}, Degraded: []string{}})
		return
	}

	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	attribution, err := controllers.RouteNameAttribution(ctx, h.CRClient, namespace, name)
	attributionFailed := err != nil
	if attributionFailed {
		attribution = controllers.Attribution{State: controllers.AttributionNone}
	}
	events, eventsErr := h.rescaleEvents(ctx, namespace, name)

	queryCtx, queryCancel := context.WithTimeout(ctx, activityDeadline)
	defer queryCancel()
	b := &activityBuilder{
		q: newActivityQueries(namespace, name), rng: rng, now: now,
		timestamps: rng.timestamps(now), rangeSeries: h.rangeSeries(), instantVec: h.instantVec(),
		attribution: attribution, attributionFailed: attributionFailed, hasRoute: app.Spec.Route != nil, events: events,
		since: since, eventsFailed: eventsErr != nil,
	}
	respondJSON(w, http.StatusOK, b.build(queryCtx))
}

func (h *Activity) rangeSeries() PromQueryRangeSeriesFunc {
	if h.PromQueryRangeSeries != nil {
		return h.PromQueryRangeSeries
	}
	return realPromQueryRangeSeries(&http.Client{Timeout: activityDeadline}, h.promBase())
}

func (h *Activity) instantVec() PromQueryInstantVecFunc {
	if h.PromQueryInstantVec != nil {
		return h.PromQueryInstantVec
	}
	return realPromQueryInstantVec(&http.Client{Timeout: activityDeadline}, h.promBase())
}

func (h *Activity) promBase() string {
	if h.PrometheusBaseURL != "" {
		return h.PrometheusBaseURL
	}
	return defaultPromURL
}

// rescaleEvents reads the SuccessfulRescale events of the app's autoscaler.
// Expired events leave older changes without a recorded actor.
func (h *Activity) rescaleEvents(ctx context.Context, namespace, name string) ([]rescaleEvent, error) {
	if h.Client == nil {
		return nil, nil
	}
	sel := fields.Set{"involvedObject.kind": "HorizontalPodAutoscaler", "involvedObject.name": name, "reason": "SuccessfulRescale"}.AsSelector().String()
	list, err := h.Client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: sel})
	if err != nil {
		return nil, err
	}
	var out []rescaleEvent
	for _, e := range list.Items {
		size, ok := rescaleSize(e.Message)
		if !ok {
			continue
		}
		out = append(out, rescaleEvent{Time: eventLatest(e), NewSize: size})
	}
	return out, nil
}

// Repeated events retain only their latest occurrence for matching changes.
func eventLatest(e corev1.Event) time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.FirstTimestamp.Time
}

type activityBuilder struct {
	q                 activityQueries
	rng               activityRange
	now               time.Time
	timestamps        []int64
	rangeSeries       PromQueryRangeSeriesFunc
	instantVec        PromQueryInstantVecFunc
	attribution       controllers.Attribution
	attributionFailed bool
	eventsFailed      bool
	// since is the namespace creation time, used to exclude an earlier
	// project's history when its name is reused.
	since    time.Time
	hasRoute bool
	events   []rescaleEvent

	mu     sync.Mutex
	single map[string][]*float64
	byCode map[string][]PromSeries
	detect map[string][]*float64
}

func (b *activityBuilder) start() time.Time { return time.Unix(b.timestamps[0], 0) }
func (b *activityBuilder) end() time.Time   { return time.Unix(b.timestamps[len(b.timestamps)-1], 0) }

// singleJob expects at most one series and aligns it to the supplied grid.
func (b *activityBuilder) singleJob(name, query string, step time.Duration, timestamps []int64, into map[string][]*float64) queryJob {
	return queryJob{name: name, run: func(ctx context.Context) error {
		series, err := b.rangeSeries(ctx, query, time.Unix(timestamps[0], 0), time.Unix(timestamps[len(timestamps)-1], 0), step)
		if err != nil {
			return err
		}
		var samples []PromSample
		if len(series) > 0 {
			samples = series[0].Samples
		}
		b.mu.Lock()
		into[name] = alignSamples(samples, timestamps)
		b.mu.Unlock()
		return nil
	}}
}

func (b *activityBuilder) build(ctx context.Context) activityResponse {
	step := b.rng.step
	window := rateWindow(step)
	b.single = map[string][]*float64{}
	b.byCode = map[string][]PromSeries{}
	b.detect = map[string][]*float64{}
	detectTS := activityRange{span: b.rng.span, step: detectionStep}.timestamps(b.now)

	jobs := []queryJob{
		b.singleJob("cpu_used", b.q.cpuUsed(window), step, b.timestamps, b.single),
		b.singleJob("cpu_requested", b.q.requested("cpu"), step, b.timestamps, b.single),
		b.singleJob("cpu_peak", b.q.cpuPeakPercent(step), step, b.timestamps, b.single),
		b.singleJob("memory_used", b.q.memoryUsed(), step, b.timestamps, b.single),
		b.singleJob("memory_requested", b.q.requested("memory"), step, b.timestamps, b.single),
		b.singleJob("replicas", b.q.replicas(), step, b.timestamps, b.single),
		b.singleJob("cpu_target", b.q.hpaTarget("cpu"), step, b.timestamps, b.single),
		b.singleJob("memory_target", b.q.hpaTarget("memory"), step, b.timestamps, b.single),
		b.singleJob("min", b.q.hpaMin(), step, b.timestamps, b.single),
		b.singleJob("max", b.q.hpaMax(), step, b.timestamps, b.single),
		b.singleJob("detect_replicas", b.q.replicas(), detectionStep, detectTS, b.detect),
		b.singleJob("detect_hpa", b.q.hpaPresent(), detectionStep, detectTS, b.detect),
		b.singleJob("detect_min", b.q.hpaMin(), detectionStep, detectTS, b.detect),
		b.singleJob("detect_max", b.q.hpaMax(), detectionStep, detectTS, b.detect),
		b.singleJob("detect_cpu_target", b.q.hpaTarget("cpu"), detectionStep, detectTS, b.detect),
		b.singleJob("detect_memory_target", b.q.hpaTarget("memory"), detectionStep, detectTS, b.detect),
	}
	if b.attribution.State == controllers.AttributionHeld {
		jobs = append(jobs,
			b.singleJob("traefik_up", b.q.traefikUp(), step, b.timestamps, b.single),
			b.singleJob("detect_traefik_up", b.q.traefikUp(), detectionStep, b.scrapeTimestamps(detectTS), b.detect),
			b.singleJob("requests_peak", b.q.requestsPeak(step), step, b.timestamps, b.single),
			queryJob{name: "requests", run: func(ctx context.Context) error {
				series, err := b.rangeSeries(ctx, b.q.requestsByCode(window), b.start(), b.end(), step)
				if err != nil {
					return err
				}
				b.mu.Lock()
				b.byCode["requests"] = series
				b.mu.Unlock()
				return nil
			}},
		)
	}
	degraded := runQueryPool(ctx, jobs)
	if len(degraded) == len(jobs) {
		return activityResponse{Reason: "Prometheus could not be reached.", Changes: []activityChange{}, Degraded: degraded}
	}

	resp := activityResponse{
		Available: true, Range: b.rng.name, StepSeconds: int64(step.Seconds()), Timestamps: b.timestamps,
		CPUUsedMillis: scaled(b.series("cpu_used"), 1000), CPURequestedMillis: scaled(b.series("cpu_requested"), 1000),
		CPUPeakPctOfRequest: b.series("cpu_peak"), MemoryUsedBytes: b.series("memory_used"), MemoryRequestedBytes: b.series("memory_requested"),
		Replicas: b.series("replicas"),
		Policy: &activityPolicy{CPUTargetPct: b.series("cpu_target"), MemoryTargetPct: b.series("memory_target"),
			Min: b.series("min"), Max: b.series("max")},
		Degraded: degraded, Changes: []activityChange{},
	}
	resp.CPUPctOfRequest = ratioPercent(b.series("cpu_used"), b.series("cpu_requested"))
	resp.MemoryPctOfRequest = ratioPercent(b.series("memory_used"), b.series("memory_requested"))
	b.fillTraffic(&resp, window, b.attributionFailed || slices.Contains(degraded, "requests"))
	b.maskBefore(&resp, window)

	changes, detailed, extraDegraded := b.changes(ctx, detectTS)
	resp.Changes, resp.ChangesDetailed = changes, detailed
	if b.eventsFailed {
		extraDegraded = append(extraDegraded, "events")
	}
	resp.Degraded = mergeSorted(resp.Degraded, extraDegraded)
	return resp
}

// scrapeTimestamps extends the detection grid back by the longest
// observation window, so a change early in the range has its whole window
// checked.
func (b *activityBuilder) scrapeTimestamps(detectTS []int64) []int64 {
	step := int64(detectionStep.Seconds())
	var out []int64
	for t := detectTS[0] - int64(maxAroundWindow.Seconds()); t < detectTS[0]; t += step {
		out = append(out, t)
	}
	return append(out, detectTS...)
}

// lookback covers the rate window or a chart step plus the peak query's
// inner rate window, whichever is longer.
func (b *activityBuilder) lookback(window time.Duration) time.Duration {
	if p := b.rng.step + time.Minute; p > window {
		return p
	}
	return window
}

// maskBefore applies the longest input window to all chart series, excluding
// points that could include an earlier project's data.
func (b *activityBuilder) maskBefore(resp *activityResponse, window time.Duration) {
	if b.since.IsZero() {
		return
	}
	lookback := b.lookback(window)
	series := [][]*float64{resp.RequestsPeakPerMin, resp.CPUPctOfRequest, resp.CPUPeakPctOfRequest, resp.CPUUsedMillis,
		resp.CPURequestedMillis, resp.MemoryPctOfRequest, resp.MemoryUsedBytes, resp.MemoryRequestedBytes, resp.Replicas}
	if r := resp.RequestsPerMin; r != nil {
		series = append(series, r.OK, r.Redirect, r.ClientError, r.Aborted, r.ServerError)
	}
	if p := resp.Policy; p != nil {
		series = append(series, p.CPUTargetPct, p.MemoryTargetPct, p.Min, p.Max)
	}
	for i, ts := range b.timestamps {
		if !time.Unix(ts, 0).Add(-lookback).Before(b.since) {
			continue
		}
		for _, s := range series {
			if i < len(s) {
				s[i] = nil
			}
		}
	}
}

func (b *activityBuilder) series(name string) []*float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.single[name]; ok {
		return s
	}
	return make([]*float64, len(b.timestamps))
}

// fillTraffic sets the request series and the traffic state. A point counts
// only when its whole input window lies inside an attribution span.
func (b *activityBuilder) fillTraffic(resp *activityResponse, window time.Duration, failed bool) {
	n := len(b.timestamps)
	reqs := &activityRequests{OK: make([]*float64, n), Redirect: make([]*float64, n), ClientError: make([]*float64, n),
		Aborted: make([]*float64, n), ServerError: make([]*float64, n)}
	peak := make([]*float64, n)
	resp.RequestsPerMin, resp.RequestsPeakPerMin = reqs, peak

	lookback := b.lookback(window)
	spanInRange := false
	for _, s := range b.attribution.Spans {
		if s.To.After(b.start()) && s.From.Before(b.end()) {
			spanInRange = true
		}
	}
	switch {
	case failed:
		resp.Traffic = "unavailable"
		return
	case b.attribution.State != controllers.AttributionHeld || !spanInRange:
		if !b.hasRoute && !spanInRange {
			resp.Traffic = "no_route"
		} else {
			resp.Traffic = "not_attributed"
		}
		return
	}

	b.mu.Lock()
	series := b.byCode["requests"]
	up := b.single["traefik_up"]
	peakRaw := b.single["requests_peak"]
	b.mu.Unlock()
	classes := map[string][]*float64{"ok": reqs.OK, "redirect": reqs.Redirect, "client_error": reqs.ClientError,
		"aborted": reqs.Aborted, "server_error": reqs.ServerError}
	attributedAny, scrapedAny := false, false
	for i, ts := range b.timestamps {
		if !attributed(ts, lookback, b.attribution.Spans) {
			continue
		}
		attributedAny = true
		scraped := i < len(up) && up[i] != nil && *up[i] == 1
		if !scraped {
			continue
		}
		scrapedAny = true
		for _, c := range classes {
			zero := 0.0
			c[i] = &zero
		}
		if i < len(peakRaw) && peakRaw[i] != nil {
			v := *peakRaw[i]
			peak[i] = &v
		}
	}
	for _, s := range series {
		class := statusClass(s.Labels["code"])
		target, ok := classes[class]
		if !ok {
			continue
		}
		aligned := alignSamples(s.Samples, b.timestamps)
		for i, v := range aligned {
			if v == nil || target[i] == nil {
				continue
			}
			sum := *target[i] + *v
			target[i] = &sum
		}
	}
	switch {
	case scrapedAny:
		resp.Traffic = "available"
	case attributedAny:
		resp.Traffic = "unavailable"
	default:
		resp.Traffic = "not_attributed"
	}
}

// changes finds the pod-count changes, decides their causes, and computes the
// observations for the latest autoscaler-era ones.
func (b *activityBuilder) changes(ctx context.Context, detectTS []int64) ([]activityChange, int, []string) {
	b.mu.Lock()
	own := func(name string) []*float64 { return b.sinceOnly(b.detect[name], detectTS) }
	replicas, hpa := own("detect_replicas"), own("detect_hpa")
	in := causeInputs{timestamps: detectTS, hpa: hpa, minReplicas: own("detect_min"), maxReplicas: own("detect_max")}
	cpuTarget, memTarget := own("detect_cpu_target"), own("detect_memory_target")
	traefikUp := b.detect["detect_traefik_up"]
	b.mu.Unlock()

	found := detectReplicaChanges(detectTS, replicas)
	out := make([]activityChange, 0, len(found))
	for _, c := range found {
		out = append(out, activityChange{Time: c.Time, From: c.From, To: c.To, Autoscaled: in.autoscaledAcross(c)})
	}
	// Match newest changes first because eventLatest uses the latest occurrence.
	used := map[int]bool{}
	for i := len(out) - 1; i >= 0; i-- {
		out[i].Cause = in.cause(replicaChange{Time: out[i].Time, From: out[i].From, To: out[i].To}, b.events, used)
	}

	var detail []int
	for i := len(out) - 1; i >= 0 && len(detail) < maxDetailedChanges; i-- {
		if out[i].Cause == causeAutoscaler || out[i].Cause == causeWhileAutoscaling {
			detail = append(detail, i)
		}
	}
	if len(detail) == 0 {
		return out, 0, nil
	}

	lowest := b.lowestTimestamp(ctx)
	var degraded []string
	var mu sync.Mutex
	jobs := make([]queryJob, 0, len(detail))
	for _, idx := range detail {
		idx := idx
		jobs = append(jobs, queryJob{name: "around", run: func(ctx context.Context) error {
			c := replicaChange{Time: out[idx].Time, From: out[idx].From, To: out[idx].To}
			win := aroundWindow(c)
			// CPU shares need a further 30s of input before the observation window.
			if !b.since.IsZero() && time.Unix(c.Time, 0).Add(-win-30*time.Second).Before(b.since) {
				mu.Lock()
				out[idx].AroundUnavailable = "The figures from before this change reach back before the project existed."
				mu.Unlock()
				return nil
			}
			if !lowest.IsZero() && time.Unix(c.Time, 0).Add(-win-30*time.Second).Before(lowest) {
				mu.Lock()
				out[idx].AroundUnavailable = "Prometheus no longer holds the data from before this change."
				mu.Unlock()
				return nil
			}
			around, err := b.around(ctx, c, win, targetAt(in, cpuTarget, c.Time), targetAt(in, memTarget, c.Time), scrapedThroughout(b.scrapeTimestamps(detectTS), traefikUp, c.Time, win))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				out[idx].AroundUnavailable = "The figures from before this change could not be read in time."
				return err
			}
			out[idx].Around = around
			return nil
		}})
	}
	if failed := runQueryPool(ctx, jobs); len(failed) > 0 {
		degraded = append(degraded, "around")
	}
	// A job still queued at the deadline never ran, so it left no reason.
	for _, idx := range detail {
		if out[idx].Around == nil && out[idx].AroundUnavailable == "" {
			out[idx].AroundUnavailable = "The figures from before this change could not be read in time."
		}
	}
	return out, len(detail), degraded
}

// scrapedThroughout requires an up value of 1 at every grid point in the window.
// This checks sampled scrape availability, not request-count completeness.
func scrapedThroughout(timestamps []int64, up []*float64, ts int64, win time.Duration) bool {
	from := ts - int64(win.Seconds())
	if len(timestamps) == 0 || from < timestamps[0] {
		return false
	}
	seen := false
	for i, t := range timestamps {
		if t < from || t > ts {
			continue
		}
		if i >= len(up) || up[i] == nil || *up[i] != 1 {
			return false
		}
		seen = true
	}
	return seen
}

// sinceOnly masks samples until ksmScrapeMargin after namespace creation.
// Detection queries identify projects by name, so allow time for a fresh
// scrape to replace samples from an earlier namespace with that name.
func (b *activityBuilder) sinceOnly(series []*float64, timestamps []int64) []*float64 {
	out := make([]*float64, len(series))
	from := b.since.Add(ksmScrapeMargin)
	for i, v := range series {
		if b.since.IsZero() || (i < len(timestamps) && !time.Unix(timestamps[i], 0).Before(from)) {
			out[i] = v
		}
	}
	return out
}

func targetAt(in causeInputs, series []*float64, ts int64) *float64 {
	return valueAt(series, in.indexAt(ts))
}

// lowestTimestamp reads the TSDB retention boundary, or returns zero if unavailable.
func (b *activityBuilder) lowestTimestamp(ctx context.Context) time.Time {
	vec, err := b.instantVec(ctx, lowestTimestampQuery, b.now)
	if err != nil || len(vec) == 0 || math.IsNaN(vec[0].Value) {
		return time.Time{}
	}
	return time.UnixMilli(int64(vec[0].Value))
}

// around reads what was measured in the window before a change: the peak and
// average of each tracked metric as a share of its request, and the requests
// by status when the window's traffic is the app's alone.
func (b *activityBuilder) around(ctx context.Context, c replicaChange, win time.Duration, cpuTarget, memTarget *float64, traefikScraped bool) (*activityAround, error) {
	at := time.Unix(c.Time, 0)
	out := &activityAround{WindowSeconds: int(win.Seconds())}
	instant := func(query string) (*float64, error) {
		vec, err := b.instantVec(ctx, query, at)
		if err != nil {
			return nil, err
		}
		if len(vec) == 0 || math.IsNaN(vec[0].Value) || math.IsInf(vec[0].Value, 0) {
			return nil, nil
		}
		v := vec[0].Value
		return &v, nil
	}
	metric := func(share string, target *float64) (*aroundMetric, error) {
		peak, err := instant(sharePeakOver(share, win))
		if err != nil {
			return nil, err
		}
		avg, err := instant(shareAverageOver(share, win))
		if err != nil {
			return nil, err
		}
		return &aroundMetric{PeakPct: peak, AvgPct: avg, TargetPct: target}, nil
	}
	var err error
	if cpuTarget != nil {
		if out.CPU, err = metric(b.q.cpuShare(), cpuTarget); err != nil {
			return nil, err
		}
	}
	if memTarget != nil {
		if out.Memory, err = metric(b.q.memoryShare(), memTarget); err != nil {
			return nil, err
		}
	}
	if traefikScraped && b.attribution.State == controllers.AttributionHeld && attributed(c.Time, win, b.attribution.Spans) {
		vec, err := b.instantVec(ctx, b.q.requestsIncrease(win), at)
		if err != nil {
			return nil, err
		}
		reqs := &aroundRequests{}
		for _, s := range vec {
			if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
				continue
			}
			code := s.Labels["code"]
			if statusClass(code) == "" {
				continue
			}
			reqs.Total += s.Value
			switch {
			case code == "404":
				reqs.NotFound += s.Value
			case code == "499":
				reqs.Aborted += s.Value
			case statusClass(code) == "server_error":
				reqs.ServerError += s.Value
			default:
				reqs.Other += s.Value
			}
		}
		out.Requests = reqs
	}
	return out, nil
}

func scaled(series []*float64, factor float64) []*float64 {
	out := make([]*float64, len(series))
	for i, v := range series {
		if v != nil {
			s := *v * factor
			out[i] = &s
		}
	}
	return out
}

// ratioPercent divides used by requested at each point, nil where either is
// missing or nothing is requested.
func ratioPercent(used, requested []*float64) []*float64 {
	out := make([]*float64, len(used))
	for i := range used {
		if i >= len(requested) || used[i] == nil || requested[i] == nil || *requested[i] == 0 {
			continue
		}
		v := *used[i] / *requested[i] * 100
		out[i] = &v
	}
	return out
}

func mergeSorted(a, b []string) []string {
	set := map[string]bool{}
	for _, v := range append(append([]string{}, a...), b...) {
		set[v] = true
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
