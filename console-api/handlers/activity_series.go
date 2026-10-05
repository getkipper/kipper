package handlers

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/getkipper/kipper/controller/pkg/routename"
)

// activityQueries builds per-app PromQL with quoted label values.
type activityQueries struct {
	namespace string
	app       string
	key       string
}

func newActivityQueries(namespace, app string) activityQueries {
	return activityQueries{namespace: namespace, app: app, key: routename.Key(namespace, app)}
}

func seconds(d time.Duration) string {
	return strconv.FormatInt(int64(d.Seconds()), 10) + "s"
}

// podLabels selects the app's pods by label at each evaluation time, so pods
// that scaled in stay in the history.
func (q activityQueries) podLabels() string {
	return fmt.Sprintf(`max by (namespace, pod) (kube_pod_labels{namespace=%s, label_app=%s})`, promString(q.namespace), promString(q.app))
}

func (q activityQueries) runningPods() string {
	return fmt.Sprintf(`max by (namespace, pod) (kube_pod_status_phase{namespace=%s, phase="Running"} == 1)`, promString(q.namespace))
}

// cpuUsed is the app's CPU use in cores over window.
func (q activityQueries) cpuUsed(window time.Duration) string {
	return fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total{namespace=%s, container!="", container!="POD"}[%s]) * on (namespace, pod) group_left() %s)`,
		promString(q.namespace), seconds(window), q.podLabels())
}

// Use requests from Running pods as the utilization denominator.
func (q activityQueries) requested(resource string) string {
	return fmt.Sprintf(`sum(kube_pod_container_resource_requests{namespace=%s, resource=%s} * on (namespace, pod) group_left() %s * on (namespace, pod) group_left() %s)`,
		promString(q.namespace), promString(resource), q.podLabels(), q.runningPods())
}

func (q activityQueries) memoryUsed() string {
	return fmt.Sprintf(`sum(container_memory_working_set_bytes{namespace=%s, container!="", container!="POD"} * on (namespace, pod) group_left() %s)`,
		promString(q.namespace), q.podLabels())
}

// cpuShare is CPU use over 30s windows as a fraction of the CPU requested.
func (q activityQueries) cpuShare() string {
	return fmt.Sprintf(`(%s / %s)`, q.cpuUsed(30*time.Second), q.requested("cpu"))
}

func (q activityQueries) memoryShare() string {
	return fmt.Sprintf(`(%s / %s)`, q.memoryUsed(), q.requested("memory"))
}

// Sample overlapping 30s CPU windows every 10s to retain short peaks.
func (q activityQueries) cpuPeakPercent(step time.Duration) string {
	return fmt.Sprintf(`max_over_time(%s[%s:10s]) * 100`, q.cpuShare(), seconds(step))
}

// Support both service-label layouts without double-counting a series. Apply
// the range function before combining selectors; ranges require selectors.
func (q activityQueries) trafficRange(fn, metric, window string) string {
	re := strconv.Quote(regexp.QuoteMeta(q.key) + `-[0-9]+@kubernetes`)
	return fmt.Sprintf(`(%[1]s(%[2]s{exported_service=~%[3]s}[%[4]s]) or %[1]s(%[2]s{service=~%[3]s, exported_service=""}[%[4]s]))`, fn, metric, re, window)
}

// requestsByCode is requests per minute by status code, averaged over window.
func (q activityQueries) requestsByCode(window time.Duration) string {
	return fmt.Sprintf(`sum by (code) %s * 60`, q.trafficRange("rate", "traefik_service_requests_total", seconds(window)))
}

// requestsPeak is the highest per-minute request rate inside each step, from
// irate over the last two samples at a 15s inner step, finer than the 30s
// Traefik scrape.
func (q activityQueries) requestsPeak(step time.Duration) string {
	return fmt.Sprintf(`max_over_time(sum(%s)[%s:15s]) * 60`, q.trafficRange("irate", "traefik_service_requests_total", "1m"), seconds(step))
}

// traefikUp requires all reported Traefik targets to be up, so a known scrape
// failure suppresses traffic totals that could be incomplete.
func (q activityQueries) traefikUp() string {
	return `min(up{job="traefik-metrics"})`
}

func (q activityQueries) replicas() string {
	return fmt.Sprintf(`max(kube_deployment_spec_replicas{namespace=%s, deployment=%s})`, promString(q.namespace), promString(q.app))
}

func (q activityQueries) hpaMetric(metric string) string {
	return fmt.Sprintf(`max(%s{namespace=%s, horizontalpodautoscaler=%s})`, metric, promString(q.namespace), promString(q.app))
}

func (q activityQueries) hpaPresent() string {
	return q.hpaMetric("kube_horizontalpodautoscaler_info")
}

func (q activityQueries) hpaMin() string {
	return q.hpaMetric("kube_horizontalpodautoscaler_spec_min_replicas")
}

func (q activityQueries) hpaMax() string {
	return q.hpaMetric("kube_horizontalpodautoscaler_spec_max_replicas")
}

func (q activityQueries) hpaTarget(resource string) string {
	return fmt.Sprintf(`max(kube_horizontalpodautoscaler_spec_target_metric{namespace=%s, horizontalpodautoscaler=%s, metric_name=%s, metric_target_type="utilization"})`,
		promString(q.namespace), promString(q.app), promString(resource))
}

// sharePeakOver and shareAverageOver read a share over the window before an
// instant, at a 10s inner step.
func sharePeakOver(share string, window time.Duration) string {
	return fmt.Sprintf(`max_over_time(%s[%s:10s]) * 100`, share, seconds(window))
}

func shareAverageOver(share string, window time.Duration) string {
	return fmt.Sprintf(`avg_over_time(%s[%s:10s]) * 100`, share, seconds(window))
}

// requestsIncrease estimates counts by status code. Each series needs at least
// two samples; requests preceding its first sample cannot be recovered.
func (q activityQueries) requestsIncrease(window time.Duration) string {
	return fmt.Sprintf(`sum by (code) %s`, q.trafficRange("increase", "traefik_service_requests_total", seconds(window)))
}

const lowestTimestampQuery = `min(prometheus_tsdb_lowest_timestamp)`
