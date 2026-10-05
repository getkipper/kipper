package handlers

import (
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestActivityQueriesQuoteEveryLabelValue(t *testing.T) {
	q := newActivityQueries(`shop"prod`, "web")

	assert.Contains(t, q.replicas(), `namespace="shop\"prod"`)
	assert.Contains(t, q.podLabels(), `label_app="web"`)
}

func TestTrafficSelectorCoversBothLabelLayoutsAndAnyPort(t *testing.T) {
	q := newActivityQueries("team", "prod--web")

	sel := q.trafficRange("rate", "traefik_service_requests_total", "2m")

	assert.Contains(t, sel, `rate(traefik_service_requests_total{exported_service=~"team-prod-web-[0-9]+@kubernetes"}[2m])`, "the key is normalised like Traefik's")
	assert.Contains(t, sel, `rate(traefik_service_requests_total{service=~"team-prod-web-[0-9]+@kubernetes", exported_service=""}[2m])`)
}

func TestTrafficSelectorEscapesTheKey(t *testing.T) {
	q := activityQueries{namespace: "a", app: "b", key: "a.b"}
	assert.Contains(t, q.trafficRange("rate", "m", "2m"), `a\\.b-[0-9]+`)
}

func TestPeakQueriesUseAnInnerStepFinerThanTheScrape(t *testing.T) {
	q := newActivityQueries("shop", "web")
	assert.Contains(t, q.requestsPeak(2*time.Minute), `[120s:15s]`)
	assert.Contains(t, q.cpuPeakPercent(2*time.Minute), `[120s:10s]`)
	assert.Contains(t, q.cpuPeakPercent(2*time.Minute), `[30s]`)
}

func TestRequestedCountsOnlyRunningPods(t *testing.T) {
	q := newActivityQueries("shop", "web")
	got := q.requested("cpu")
	assert.Contains(t, got, `resource="cpu"`)
	assert.Contains(t, got, `phase="Running"} == 1`)
}

// rangeOnExpression matches a plain range such as [2m] after a closing
// parenthesis, which Prometheus refuses; only a subquery [2m:15s] may follow one.
var rangeOnExpression = regexp.MustCompile(`\)\[[0-9]+[smh]\]`)

func TestTrafficQueriesPutRangesOnSelectorsOnly(t *testing.T) {
	q := newActivityQueries("shop", "web")
	for name, query := range map[string]string{
		"requestsByCode":   q.requestsByCode(2 * time.Minute),
		"requestsPeak":     q.requestsPeak(2 * time.Minute),
		"requestsIncrease": q.requestsIncrease(2 * time.Minute),
		"cpuPeakPercent":   q.cpuPeakPercent(2 * time.Minute),
		"sharePeakOver":    sharePeakOver(q.cpuShare(), 2*time.Minute),
	} {
		assert.NotRegexp(t, rangeOnExpression, query, name)
	}
}

func TestTrafficCountsOnlyWhileEveryTraefikPodWasScraped(t *testing.T) {
	assert.Equal(t, `min(up{job="traefik-metrics"})`, newActivityQueries("shop", "web").traefikUp())
}
