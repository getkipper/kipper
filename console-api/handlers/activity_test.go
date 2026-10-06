package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
	"github.com/getkipper/kipper/controller/pkg/routename"
)

var activityNow = time.Unix(1_791_151_200, 0)

// fakeProm answers range and instant queries by exact query string.
type fakeProm struct {
	mu      sync.Mutex
	ranges  map[string]func(start, end time.Time, step time.Duration) []PromSeries
	instant map[string][]PromVectorSample
	fail    map[string]bool
	failAll bool
	asked   []string
}

func newFakeProm() *fakeProm {
	return &fakeProm{ranges: map[string]func(time.Time, time.Time, time.Duration) []PromSeries{}, instant: map[string][]PromVectorSample{}, fail: map[string]bool{}}
}

func (p *fakeProm) rangeSeries(_ context.Context, query string, start, end time.Time, step time.Duration) ([]PromSeries, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, query)
	if p.fail[query] || p.failAll {
		return nil, errors.New("prometheus 500")
	}
	if fn, ok := p.ranges[query]; ok {
		return fn(start, end, step), nil
	}
	return nil, nil
}

func (p *fakeProm) instantVec(_ context.Context, query string, _ time.Time) ([]PromVectorSample, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, query)
	if p.fail[query] || p.failAll {
		return nil, errors.New("prometheus 500")
	}
	return p.instant[query], nil
}

func constant(v float64, labels map[string]string) func(time.Time, time.Time, time.Duration) []PromSeries {
	return func(start, end time.Time, step time.Duration) []PromSeries {
		var samples []PromSample
		for t := start; !t.After(end); t = t.Add(step) {
			samples = append(samples, PromSample{Time: t, Value: v})
		}
		return []PromSeries{{Labels: labels, Samples: samples}}
	}
}

func routedAppObject() *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec:       kipperv1.AppSpec{Image: "nginx", Port: 8080, Route: &kipperv1.AppRoute{Host: "web.example.com"}},
	}
}

func heldClaim(from time.Time) crclient.Object {
	cm := controllers.RouteNameClaimObject(routename.Key("shop", "web"), "shop", "uid-shop")
	cm.Data["intervals"] = `[{"from":"` + from.UTC().Format(time.RFC3339) + `","generation":"g1"}]`
	return cm
}

func freshMarker() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "route-name-claims", Namespace: "kipper-system"},
		Data:       map[string]string{"generation": "g1", "imageID": "img", "bootstrapped": "true", "heartbeat": activityNow.UTC().Format(time.RFC3339Nano)},
	}
}

func getActivity(t *testing.T, h *Activity, query string) (int, activityResponse) {
	t.Helper()
	r := chi.NewRouter()
	r.Get("/projects/{name}/apps/{app}/activity", h.Get)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/projects/shop/apps/web/activity"+query, nil))
	var resp activityResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return rec.Code, resp
}

func newActivity(prom *fakeProm, objs ...crclient.Object) *Activity {
	base := []crclient.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop", UID: types.UID("uid-shop")}}}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(append(base, objs...)...).Build()
	return &Activity{CRClient: c, Client: fake.NewClientset(), PromQueryRangeSeries: prom.rangeSeries, PromQueryInstantVec: prom.instantVec,
		Now: func() time.Time { return activityNow }}
}

func TestActivity_RefusesAnUnknownRange(t *testing.T) {
	code, _ := getActivity(t, newActivity(newFakeProm(), routedAppObject()), "?range=7d")
	assert.Equal(t, http.StatusBadRequest, code)
}

func TestActivity_SaysSoWhenMonitoringIsOff(t *testing.T) {
	pc := &kipperv1.PlatformConfig{ObjectMeta: metav1.ObjectMeta{Name: "platform"}, Spec: kipperv1.PlatformConfigSpec{Profile: "nano"}}
	code, resp := getActivity(t, newActivity(newFakeProm(), routedAppObject(), pc), "")
	require.Equal(t, http.StatusOK, code)
	assert.False(t, resp.Available)
	assert.NotEmpty(t, resp.Reason)
}

func TestActivity_SaysSoWhenPrometheusCannotBeReached(t *testing.T) {
	prom := newFakeProm()
	prom.failAll = true
	code, resp := getActivity(t, newActivity(prom, routedAppObject(), heldClaim(activityNow.Add(-3*time.Hour)), freshMarker()), "")
	require.Equal(t, http.StatusOK, code)
	assert.False(t, resp.Available)
	assert.NotEmpty(t, resp.Reason)
}

func TestActivity_ChartsTrafficAndResourcesForAnAppThatHoldsItsName(t *testing.T) {
	prom := newFakeProm()
	q := newActivityQueries("shop", "web")
	rng, _ := activityRangeFor("1h")
	window := rateWindow(rng.step)
	prom.ranges[q.cpuUsed(window)] = constant(0.18, nil)
	prom.ranges[q.requested("cpu")] = constant(0.36, nil)
	prom.ranges[q.traefikUp()] = constant(1, nil)
	prom.ranges[q.requestsByCode(window)] = func(s, e time.Time, st time.Duration) []PromSeries {
		return append(append(constant(10, map[string]string{"code": "200"})(s, e, st), constant(2, map[string]string{"code": "404"})(s, e, st)...),
			constant(1, map[string]string{"code": "499"})(s, e, st)...)
	}

	code, resp := getActivity(t, newActivity(prom, routedAppObject(), heldClaim(activityNow.Add(-3*time.Hour)), freshMarker()), "?range=1h")

	require.Equal(t, http.StatusOK, code)
	assert.True(t, resp.Available)
	assert.Equal(t, "available", resp.Traffic)
	last := len(resp.Timestamps) - 1
	require.NotNil(t, resp.CPUPctOfRequest[last])
	assert.InDelta(t, 50, *resp.CPUPctOfRequest[last], 0.001)
	assert.InDelta(t, 180, *resp.CPUUsedMillis[last], 0.001)
	require.NotNil(t, resp.RequestsPerMin.OK[last])
	assert.InDelta(t, 10, *resp.RequestsPerMin.OK[last], 0.001)
	assert.InDelta(t, 2, *resp.RequestsPerMin.ClientError[last], 0.001)
	assert.InDelta(t, 1, *resp.RequestsPerMin.Aborted[last], 0.001)
	assert.InDelta(t, 0, *resp.RequestsPerMin.ServerError[last], 0.001, "a class with no series is 0 while Traefik is scraped")
	assert.Empty(t, resp.Degraded)
}

func TestActivity_TrafficStates(t *testing.T) {
	t.Run("another namespace holds the name", func(t *testing.T) {
		claim := controllers.RouteNameClaimObject(routename.Key("shop", "web"), "elsewhere", "uid-x")
		_, resp := getActivity(t, newActivity(newFakeProm(), routedAppObject(), claim, freshMarker()), "")
		assert.Equal(t, "not_attributed", resp.Traffic)
		assert.Nil(t, resp.RequestsPerMin.OK[len(resp.Timestamps)-1])
	})
	t.Run("another project's app has the same traffic name", func(t *testing.T) {
		claim := controllers.RouteNameClaimObject(routename.Key("shop", "web"), "shop", "uid-shop")
		claim.Data["shared"] = `["shop-other"]`
		_, resp := getActivity(t, newActivity(newFakeProm(), routedAppObject(), claim, freshMarker()), "")
		assert.Equal(t, "not_attributed", resp.Traffic, "older consoles explain the missing figures")
		assert.Equal(t, "shared", resp.TrafficReason)
	})
	t.Run("no route and no history", func(t *testing.T) {
		app := routedAppObject()
		app.Spec.Route = nil
		_, resp := getActivity(t, newActivity(newFakeProm(), app), "")
		assert.Equal(t, "no_route", resp.Traffic)
	})
	t.Run("the requests query fails", func(t *testing.T) {
		prom := newFakeProm()
		rng, _ := activityRangeFor("1h")
		prom.fail[newActivityQueries("shop", "web").requestsByCode(rateWindow(rng.step))] = true
		_, resp := getActivity(t, newActivity(prom, routedAppObject(), heldClaim(activityNow.Add(-3*time.Hour)), freshMarker()), "")
		assert.Equal(t, "unavailable", resp.Traffic)
		assert.Contains(t, resp.Degraded, "requests")
		assert.True(t, resp.Available, "the rest of the response stands")
	})
	t.Run("the Traefik scrape check fails", func(t *testing.T) {
		prom := newFakeProm()
		prom.fail[newActivityQueries("shop", "web").traefikUp()] = true
		_, resp := getActivity(t, newActivity(prom, routedAppObject(), heldClaim(activityNow.Add(-3*time.Hour)), freshMarker()), "")
		assert.Equal(t, "unavailable", resp.Traffic)
	})
	t.Run("Traefik was not scraped in the range", func(t *testing.T) {
		prom := newFakeProm()
		prom.ranges[newActivityQueries("shop", "web").traefikUp()] = constant(0, nil)
		_, resp := getActivity(t, newActivity(prom, routedAppObject(), heldClaim(activityNow.Add(-3*time.Hour)), freshMarker()), "")
		assert.Equal(t, "unavailable", resp.Traffic)
	})
	t.Run("the claim cannot be read", func(t *testing.T) {
		h := newActivity(newFakeProm(), routedAppObject(), heldClaim(activityNow.Add(-3*time.Hour)), freshMarker())
		h.CRClient = interceptor.NewClient(h.CRClient.(crclient.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return errors.New("apiserver unavailable")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		_, resp := getActivity(t, h, "")
		assert.Equal(t, "unavailable", resp.Traffic)
	})
	t.Run("the claim is newer than the input window of every point", func(t *testing.T) {
		_, resp := getActivity(t, newActivity(newFakeProm(), routedAppObject(), heldClaim(activityNow.Add(time.Minute)), freshMarker()), "")
		assert.Equal(t, "not_attributed", resp.Traffic)
	})
}

func TestActivity_ExplainsAnAutoscalerChange(t *testing.T) {
	prom := newFakeProm()
	q := newActivityQueries("shop", "web")
	changeAt := activityNow.Add(-10 * time.Minute)
	prom.ranges[q.replicas()] = func(s, e time.Time, st time.Duration) []PromSeries {
		var samples []PromSample
		for t := s; !t.After(e); t = t.Add(st) {
			v := 2.0
			if !t.Before(changeAt) {
				v = 3
			}
			samples = append(samples, PromSample{Time: t, Value: v})
		}
		return []PromSeries{{Samples: samples}}
	}
	prom.ranges[q.hpaPresent()] = constant(1, nil)
	prom.ranges[q.hpaTarget("cpu")] = constant(70, nil)
	prom.ranges[q.traefikUp()] = constant(1, nil)
	win := 2 * time.Minute
	prom.instant[sharePeakOver(q.cpuShare(), win)] = []PromVectorSample{{Value: 97}}
	prom.instant[shareAverageOver(q.cpuShare(), win)] = []PromVectorSample{{Value: 41}}
	prom.instant[q.requestsIncrease(win)] = []PromVectorSample{
		{Labels: map[string]string{"code": "200"}, Value: 3}, {Labels: map[string]string{"code": "404"}, Value: 17}, {Labels: map[string]string{"code": "499"}, Value: 37},
	}
	h := newActivity(prom, routedAppObject(), heldClaim(activityNow.Add(-3*time.Hour)), freshMarker())
	h.Client = fake.NewClientset(&corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: "shop"},
		InvolvedObject: corev1.ObjectReference{Kind: "HorizontalPodAutoscaler", Name: "web"},
		Reason:         "SuccessfulRescale", Message: "New size: 3; reason: cpu resource utilization (percentage of request) above target",
		LastTimestamp: metav1.NewTime(changeAt.Add(10 * time.Second)),
	})

	_, resp := getActivity(t, h, "?range=6h")

	require.Len(t, resp.Changes, 1)
	c := resp.Changes[0]
	assert.Equal(t, 2, c.From)
	assert.Equal(t, 3, c.To)
	assert.Equal(t, causeAutoscaler, c.Cause)
	require.NotNil(t, c.Around)
	assert.Equal(t, 120, c.Around.WindowSeconds)
	require.NotNil(t, c.Around.CPU)
	assert.InDelta(t, 97, *c.Around.CPU.PeakPct, 0.001)
	assert.InDelta(t, 70, *c.Around.CPU.TargetPct, 0.001)
	assert.Nil(t, c.Around.Memory, "memory is not tracked")
	require.NotNil(t, c.Around.Requests)
	assert.InDelta(t, 57, c.Around.Requests.Total, 0.001)
	assert.InDelta(t, 17, c.Around.Requests.NotFound, 0.001)
	assert.InDelta(t, 37, c.Around.Requests.Aborted, 0.001)
	assert.Equal(t, 1, resp.ChangesDetailed)
}

func TestActivity_DetailsOnlyTheLatestTwentyChanges(t *testing.T) {
	prom := newFakeProm()
	q := newActivityQueries("shop", "web")
	prom.ranges[q.replicas()] = func(s, e time.Time, st time.Duration) []PromSeries {
		var samples []PromSample
		i := 0
		for t := s; !t.After(e); t = t.Add(st) {
			samples = append(samples, PromSample{Time: t, Value: float64(2 + i%2)})
			i++
		}
		return []PromSeries{{Samples: samples}}
	}
	prom.ranges[q.hpaPresent()] = constant(1, nil)

	_, resp := getActivity(t, newActivity(prom, routedAppObject()), "?range=1h")

	assert.Greater(t, len(resp.Changes), 100, "a flapping app")
	assert.Equal(t, maxDetailedChanges, resp.ChangesDetailed)
	assert.Nil(t, resp.Changes[0].Around, "older changes keep their marker without figures")
	assert.NotNil(t, resp.Changes[len(resp.Changes)-1].Around)
}

func TestActivity_AChangeBeforeRetentionHasNoFigures(t *testing.T) {
	prom := newFakeProm()
	q := newActivityQueries("shop", "web")
	changeAt := activityNow.Add(-50 * time.Minute)
	prom.ranges[q.replicas()] = func(s, e time.Time, st time.Duration) []PromSeries {
		var samples []PromSample
		for t := s; !t.After(e); t = t.Add(st) {
			v := 2.0
			if !t.Before(changeAt) {
				v = 3
			}
			samples = append(samples, PromSample{Time: t, Value: v})
		}
		return []PromSeries{{Samples: samples}}
	}
	prom.ranges[q.hpaPresent()] = constant(1, nil)
	prom.instant[lowestTimestampQuery] = []PromVectorSample{{Value: float64(changeAt.Add(-time.Minute).UnixMilli())}}

	_, resp := getActivity(t, newActivity(prom, routedAppObject()), "?range=1h")

	require.Len(t, resp.Changes, 1)
	assert.Nil(t, resp.Changes[0].Around)
	assert.NotEmpty(t, resp.Changes[0].AroundUnavailable)
}

func TestActivity_AChangeBeforeTheClaimShowsNoRequestCounts(t *testing.T) {
	prom := newFakeProm()
	q := newActivityQueries("shop", "web")
	changeAt := activityNow.Add(-10 * time.Minute)
	prom.ranges[q.replicas()] = func(s, e time.Time, st time.Duration) []PromSeries {
		var samples []PromSample
		for t := s; !t.After(e); t = t.Add(st) {
			v := 2.0
			if !t.Before(changeAt) {
				v = 3
			}
			samples = append(samples, PromSample{Time: t, Value: v})
		}
		return []PromSeries{{Samples: samples}}
	}
	prom.ranges[q.hpaPresent()] = constant(1, nil)
	prom.instant[q.requestsIncrease(2*time.Minute)] = []PromVectorSample{{Labels: map[string]string{"code": "200"}, Value: 9}}

	_, resp := getActivity(t, newActivity(prom, routedAppObject(), heldClaim(changeAt.Add(-time.Minute)), freshMarker()), "?range=1h")

	require.Len(t, resp.Changes, 1)
	require.NotNil(t, resp.Changes[0].Around)
	assert.Nil(t, resp.Changes[0].Around.Requests, "the window before the change is not wholly the app's own traffic")
}

func TestActivity_PointsBeforeTheClaimHaveNoTraffic(t *testing.T) {
	prom := newFakeProm()
	q := newActivityQueries("shop", "web")
	rng, _ := activityRangeFor("1h")
	window := rateWindow(rng.step)
	prom.ranges[q.traefikUp()] = constant(1, nil)
	prom.ranges[q.requestsByCode(window)] = constant(10, map[string]string{"code": "200"})
	claimFrom := activityNow.Add(-30 * time.Minute)

	_, resp := getActivity(t, newActivity(prom, routedAppObject(), heldClaim(claimFrom), freshMarker()), "?range=1h")

	require.Equal(t, "available", resp.Traffic)
	assert.Nil(t, resp.RequestsPerMin.OK[0], "a point whose window starts before the claim may hold another workload's traffic")
	require.NotNil(t, resp.RequestsPerMin.OK[len(resp.Timestamps)-1])
	for i, ts := range resp.Timestamps {
		if time.Unix(ts, 0).Add(-window).Before(claimFrom) {
			assert.Nil(t, resp.RequestsPerMin.OK[i], "point %d", i)
		}
	}
}

func TestActivity_AChangeWhileTraefikWasNotScrapedShowsNoRequestCounts(t *testing.T) {
	prom := newFakeProm()
	q := newActivityQueries("shop", "web")
	changeAt := activityNow.Add(-10 * time.Minute)
	prom.ranges[q.replicas()] = func(s, e time.Time, st time.Duration) []PromSeries {
		var samples []PromSample
		for t := s; !t.After(e); t = t.Add(st) {
			v := 2.0
			if !t.Before(changeAt) {
				v = 3
			}
			samples = append(samples, PromSample{Time: t, Value: v})
		}
		return []PromSeries{{Samples: samples}}
	}
	prom.ranges[q.hpaPresent()] = constant(1, nil)
	prom.ranges[q.traefikUp()] = func(s, e time.Time, st time.Duration) []PromSeries {
		var samples []PromSample
		for t := s; !t.After(e); t = t.Add(st) {
			v := 1.0
			if t.After(changeAt.Add(-time.Minute)) && t.Before(changeAt) {
				v = 0
			}
			samples = append(samples, PromSample{Time: t, Value: v})
		}
		return []PromSeries{{Samples: samples}}
	}

	_, resp := getActivity(t, newActivity(prom, routedAppObject(), heldClaim(activityNow.Add(-3*time.Hour)), freshMarker()), "?range=1h")

	require.Len(t, resp.Changes, 1)
	require.NotNil(t, resp.Changes[0].Around)
	assert.Nil(t, resp.Changes[0].Around.Requests, "a window with a failed Traefik scrape would undercount")
}

func TestActivity_DetailsThatRunOutOfTimeSaySo(t *testing.T) {
	var reps []*float64
	ts := make([]int64, 0, 60)
	for i := 0; i < 60; i++ {
		ts = append(ts, activityNow.Unix()-int64(60-i)*30)
		v := float64(2 + i%2)
		reps = append(reps, &v)
	}
	one := 1.0
	hpa := make([]*float64, len(ts))
	for i := range hpa {
		hpa[i] = &one
	}
	b := &activityBuilder{
		q: newActivityQueries("shop", "web"), now: activityNow,
		instantVec: func(ctx context.Context, _ string, _ time.Time) ([]PromVectorSample, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
		detect: map[string][]*float64{"detect_replicas": reps, "detect_hpa": hpa, "detect_cpu_target": hpa},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	out, detailed, _ := b.changes(ctx, ts)

	require.Equal(t, maxDetailedChanges, detailed)
	reasons := 0
	for _, c := range out {
		if c.AroundUnavailable != "" {
			reasons++
		}
	}
	assert.Equal(t, maxDetailedChanges, reasons, "every detailed change without figures gives a reason")
}

func TestScrapedThroughoutRefusesAWindowItCannotSee(t *testing.T) {
	one := 1.0
	ts := []int64{100, 130, 160, 190, 220}
	up := []*float64{&one, &one, &one, &one, &one}
	assert.True(t, scrapedThroughout(ts, up, 220, time.Minute))
	assert.False(t, scrapedThroughout(ts, up, 160, 2*time.Minute), "the window starts before the first sample")
}

func TestActivity_AChangeEarlyInTheRangeKeepsItsRequestCounts(t *testing.T) {
	prom := newFakeProm()
	q := newActivityQueries("shop", "web")
	rng, _ := activityRangeFor("1h")
	ts := rng.timestamps(activityNow)
	changeAt := time.Unix(ts[0], 0).Add(time.Minute)
	prom.ranges[q.replicas()] = func(s, e time.Time, st time.Duration) []PromSeries {
		var samples []PromSample
		for t := s; !t.After(e); t = t.Add(st) {
			v := 2.0
			if !t.Before(changeAt) {
				v = 3
			}
			samples = append(samples, PromSample{Time: t, Value: v})
		}
		return []PromSeries{{Samples: samples}}
	}
	prom.ranges[q.hpaPresent()] = constant(1, nil)
	prom.ranges[q.traefikUp()] = constant(1, nil)
	prom.instant[q.requestsIncrease(2*time.Minute)] = []PromVectorSample{{Labels: map[string]string{"code": "200"}, Value: 9}}

	_, resp := getActivity(t, newActivity(prom, routedAppObject(), heldClaim(activityNow.Add(-3*time.Hour)), freshMarker()), "?range=1h")

	require.Len(t, resp.Changes, 1)
	require.NotNil(t, resp.Changes[0].Around)
	require.NotNil(t, resp.Changes[0].Around.Requests, "the scrape check reaches back before the range")
	assert.InDelta(t, 9, resp.Changes[0].Around.Requests.Total, 0.001)
}

func TestActivity_NoPeakWhereTraefikWasNotScraped(t *testing.T) {
	prom := newFakeProm()
	q := newActivityQueries("shop", "web")
	rng, _ := activityRangeFor("1h")
	prom.ranges[q.traefikUp()] = func(s, e time.Time, st time.Duration) []PromSeries {
		var samples []PromSample
		for t := s; !t.After(e); t = t.Add(st) {
			v := 1.0
			if t.Equal(e) {
				v = 0
			}
			samples = append(samples, PromSample{Time: t, Value: v})
		}
		return []PromSeries{{Samples: samples}}
	}
	prom.ranges[q.requestsPeak(rng.step)] = constant(50, nil)

	_, resp := getActivity(t, newActivity(prom, routedAppObject(), heldClaim(activityNow.Add(-3*time.Hour)), freshMarker()), "?range=1h")

	last := len(resp.Timestamps) - 1
	assert.Nil(t, resp.RequestsPeakPerMin[last])
	assert.NotNil(t, resp.RequestsPeakPerMin[last-1])
}

func TestActivity_NoTrafficAfterTheLastVerifiedScan(t *testing.T) {
	prom := newFakeProm()
	q := newActivityQueries("shop", "web")
	rng, _ := activityRangeFor("1h")
	prom.ranges[q.traefikUp()] = constant(1, nil)
	prom.ranges[q.requestsByCode(rateWindow(rng.step))] = constant(10, map[string]string{"code": "200"})
	marker := freshMarker()
	marker.Data["heartbeat"] = activityNow.Add(-45 * time.Second).UTC().Format(time.RFC3339Nano)

	_, resp := getActivity(t, newActivity(prom, routedAppObject(), heldClaim(activityNow.Add(-3*time.Hour)), marker), "?range=1h")

	last := len(resp.Timestamps) - 1
	assert.Nil(t, resp.RequestsPerMin.OK[last], "a colliding route published after the scan could already be in this point")
	assert.NotNil(t, resp.RequestsPerMin.OK[last-2])
}

func TestActivity_ARecreatedProjectShowsNothingFromBeforeIt(t *testing.T) {
	prom := newFakeProm()
	q := newActivityQueries("shop", "web")
	rng, _ := activityRangeFor("1h")
	created := activityNow.Add(-20 * time.Minute)
	earlyChange := activityNow.Add(-40 * time.Minute)
	lateChange := created.Add(3 * time.Minute)
	prom.ranges[q.cpuUsed(rateWindow(rng.step))] = constant(0.1, nil)
	prom.ranges[q.requested("cpu")] = constant(0.2, nil)
	prom.ranges[q.replicas()] = func(s, e time.Time, st time.Duration) []PromSeries {
		var samples []PromSample
		for t := s; !t.After(e); t = t.Add(st) {
			v := 1.0
			if !t.Before(earlyChange) {
				v = 2
			}
			if !t.Before(lateChange) {
				v = 3
			}
			samples = append(samples, PromSample{Time: t, Value: v})
		}
		return []PromSeries{{Samples: samples}}
	}
	prom.ranges[q.hpaPresent()] = constant(1, nil)
	h := newActivity(prom, routedAppObject())
	var ns corev1.Namespace
	require.NoError(t, h.CRClient.Get(context.Background(), crclient.ObjectKey{Name: "shop"}, &ns))
	ns.CreationTimestamp = metav1.NewTime(created)
	require.NoError(t, h.CRClient.Update(context.Background(), &ns))

	_, resp := getActivity(t, h, "?range=1h")

	for i, ts := range resp.Timestamps {
		if time.Unix(ts, 0).Add(-rateWindow(rng.step)).Before(created) {
			assert.Nil(t, resp.CPUPctOfRequest[i], "point %d may hold the earlier project's pods", i)
			assert.Nil(t, resp.Replicas[i], "point %d", i)
		}
	}
	assert.NotNil(t, resp.CPUPctOfRequest[len(resp.Timestamps)-1])
	require.Len(t, resp.Changes, 1, "the earlier project's change is not this project's")
	assert.Equal(t, 3, resp.Changes[0].To)
}

func TestActivity_FailsWhenTheProjectCannotBeRead(t *testing.T) {
	h := newActivity(newFakeProm(), routedAppObject())
	h.CRClient = interceptor.NewClient(h.CRClient.(crclient.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if _, ok := obj.(*corev1.Namespace); ok {
				return errors.New("apiserver unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	code, _ := getActivity(t, h, "")

	assert.Equal(t, http.StatusInternalServerError, code, "without the project's start an earlier project's history could show")
}

func TestActivity_AFoldedEventExplainsTheLatestMatchingChange(t *testing.T) {
	ts := make([]int64, 0, 8)
	for i := 0; i < 8; i++ {
		ts = append(ts, activityNow.Unix()-int64(8-i)*30)
	}
	v := func(f float64) *float64 { return &f }
	one := v(1)
	reps := []*float64{v(2), v(3), v(2), v(3), v(3), v(3), v(3), v(3)}
	hpa := []*float64{one, one, one, one, one, one, one, one}
	b := &activityBuilder{
		q: newActivityQueries("shop", "web"), now: activityNow,
		instantVec: func(context.Context, string, time.Time) ([]PromVectorSample, error) { return nil, nil },
		detect:     map[string][]*float64{"detect_replicas": reps, "detect_hpa": hpa},
		// Model a repeated event using its latest occurrence.
		events: []rescaleEvent{{Time: time.Unix(ts[3]-5, 0), NewSize: 3}},
	}

	out, _, _ := b.changes(context.Background(), ts)

	require.Len(t, out, 3)
	assert.Equal(t, causeWhileAutoscaling, out[0].Cause)
	assert.Equal(t, causeAutoscaler, out[2].Cause)
}

func TestActivity_AnObservationNeedsItsRateWindowInsideTheProject(t *testing.T) {
	ts := make([]int64, 0, 12)
	for i := 0; i < 12; i++ {
		ts = append(ts, activityNow.Unix()-int64(12-i)*30)
	}
	v := func(f float64) *float64 { return &f }
	reps := make([]*float64, len(ts))
	hpa := make([]*float64, len(ts))
	for i := range ts {
		reps[i], hpa[i] = v(2), v(1)
		if i >= 8 {
			reps[i] = v(3)
		}
	}
	b := &activityBuilder{
		q: newActivityQueries("shop", "web"), now: activityNow,
		instantVec: func(context.Context, string, time.Time) ([]PromVectorSample, error) {
			return []PromVectorSample{{Value: 50}}, nil
		},
		detect: map[string][]*float64{"detect_replicas": reps, "detect_hpa": hpa, "detect_cpu_target": hpa},
		since:  time.Unix(ts[8], 0).Add(-135 * time.Second),
	}

	out, _, _ := b.changes(context.Background(), ts)

	require.Len(t, out, 1)
	assert.Nil(t, out[0].Around, "the 2-minute window fits, its 30s rate window does not")
	assert.NotEmpty(t, out[0].AroundUnavailable)
}

func TestActivity_AnEarlierProjectsBoundsDoNotExplainAChange(t *testing.T) {
	ts := make([]int64, 0, 12)
	for i := 0; i < 12; i++ {
		ts = append(ts, activityNow.Unix()-int64(12-i)*30)
	}
	v := func(f float64) *float64 { return &f }
	reps := make([]*float64, len(ts))
	mins := make([]*float64, len(ts))
	for i := range ts {
		mins[i] = v(2)
		if i >= 6 {
			mins[i] = v(3) // Model an old minimum changing inside the 60s scrape margin.
		}
		if i >= 7 {
			reps[i] = v(2)
		}
		if i >= 8 {
			reps[i] = v(3)
		}
	}
	b := &activityBuilder{
		q: newActivityQueries("shop", "web"), now: activityNow,
		instantVec: func(context.Context, string, time.Time) ([]PromVectorSample, error) { return nil, nil },
		detect:     map[string][]*float64{"detect_replicas": reps, "detect_min": mins},
		since:      time.Unix(ts[4], 0).Add(5 * time.Second),
	}

	out, _, _ := b.changes(context.Background(), ts)

	require.Len(t, out, 1)
	assert.Equal(t, causeUnknown, out[0].Cause)
}

func TestActivity_SaysSoWhenTheAutoscalerEventsCannotBeRead(t *testing.T) {
	h := newActivity(newFakeProm(), routedAppObject())
	cs := fake.NewClientset()
	cs.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	h.Client = cs

	_, resp := getActivity(t, h, "")

	assert.Contains(t, resp.Degraded, "events", "without events every autoscaler change would look unrecorded")
}

func TestActivity_AnObservationNeedsItsRateWindowInsideRetention(t *testing.T) {
	ts := make([]int64, 0, 12)
	for i := 0; i < 12; i++ {
		ts = append(ts, activityNow.Unix()-int64(12-i)*30)
	}
	v := func(f float64) *float64 { return &f }
	reps := make([]*float64, len(ts))
	hpa := make([]*float64, len(ts))
	for i := range ts {
		reps[i], hpa[i] = v(2), v(1)
		if i >= 8 {
			reps[i] = v(3)
		}
	}
	lowest := time.Unix(ts[8], 0).Add(-135 * time.Second)
	b := &activityBuilder{
		q: newActivityQueries("shop", "web"), now: activityNow,
		instantVec: func(_ context.Context, query string, _ time.Time) ([]PromVectorSample, error) {
			if query == lowestTimestampQuery {
				return []PromVectorSample{{Value: float64(lowest.UnixMilli())}}, nil
			}
			return []PromVectorSample{{Value: 50}}, nil
		},
		detect: map[string][]*float64{"detect_replicas": reps, "detect_hpa": hpa, "detect_cpu_target": hpa},
	}

	out, _, _ := b.changes(context.Background(), ts)

	require.Len(t, out, 1)
	assert.Nil(t, out[0].Around)
	assert.NotEmpty(t, out[0].AroundUnavailable)
}

func TestActivity_AnEarlierProjectsLastSampleIsNoChange(t *testing.T) {
	ts := make([]int64, 0, 12)
	for i := 0; i < 12; i++ {
		ts = append(ts, activityNow.Unix()-int64(12-i)*30)
	}
	v := func(f float64) *float64 { return &f }
	reps := make([]*float64, len(ts))
	for i := range ts {
		reps[i] = v(1)
		if i <= 6 {
			// Model an old sample still visible just after namespace creation.
			reps[i] = v(7)
		}
	}
	b := &activityBuilder{
		q: newActivityQueries("shop", "web"), now: activityNow,
		instantVec: func(context.Context, string, time.Time) ([]PromVectorSample, error) { return nil, nil },
		detect:     map[string][]*float64{"detect_replicas": reps},
		since:      time.Unix(ts[6], 0).Add(-10 * time.Second),
	}

	out, _, _ := b.changes(context.Background(), ts)

	assert.Empty(t, out, "the 7 was scraped from the earlier namespace")
}
