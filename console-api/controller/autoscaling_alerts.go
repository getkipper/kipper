package controller

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/handlers"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
	"github.com/getkipper/kipper/controller/pkg/labels"
)

const (
	atMaximumAction = "autoscaling at maximum"
	notReadyAction  = "autoscaling not ready"

	// firstSightWindow is how old an autoscaler's last scale may be on the
	// first look and still be recorded. It covers the previous leader's last
	// interval, the lease handover and this controller's first interval.
	firstSightWindow = 3 * checkInterval
)

// autoscalingEpisode is an autoscaling warning that is due unless the store
// already holds it from before a restart.
type autoscalingEpisode struct {
	batch alertBatch
	// since is when the episode began. A stored alert from then on covers it.
	since time.Time
}

// checkAutoscaling warns when an autoscaled app is held at its maximum and when
// an App's AutoscalingReady condition is False. The at-maximum warning fires
// again only after the app has left its maximum, the readiness warning again
// only for a new reason or after the condition recovers.
func (rc *ResourceController) checkAutoscaling(ctx context.Context) []alertBatch {
	now := time.Now()
	nowStr := now.UTC().Format(time.RFC3339)
	due := append(rc.atMaximumEpisodes(ctx, now, nowStr), rc.notReadyEpisodes(ctx, nowStr)...)
	if len(due) == 0 {
		return nil
	}

	// A failed read reports again rather than risk staying silent.
	stored, err := handlers.StoredAlerts(ctx, rc.client)
	if err != nil {
		log.Printf("resource controller: reading stored alerts: %v", err)
	}
	var batches []alertBatch
	for _, ep := range due {
		if storedSince(stored, ep.batch.entry, ep.since) {
			rc.commitBatches([]alertBatch{{marks: ep.batch.marks, apply: ep.batch.apply}})
			continue
		}
		batches = append(batches, ep.batch)
	}
	return batches
}

func (rc *ResourceController) atMaximumEpisodes(ctx context.Context, now time.Time, nowStr string) []autoscalingEpisode {
	hpas, err := rc.client.AutoscalingV2().HorizontalPodAutoscalers("").List(ctx, metav1.ListOptions{
		LabelSelector: labels.KipperManagedSelector,
	})
	if err != nil {
		return nil
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()
	present := make(map[string]bool, len(hpas.Items))
	var due []autoscalingEpisode
	for i := range hpas.Items {
		hpa := &hpas.Items[i]
		key := hpaKey(hpa)
		present[key] = true
		if hpa.Status.CurrentReplicas < hpa.Spec.MaxReplicas {
			delete(rc.atMaxAlerted, key)
			rc.belowMaxAt[key] = now
			continue
		}
		limit := maximumLimit(hpa)
		if _, alerted := rc.atMaxAlerted[key]; alerted || limit == nil {
			continue
		}
		// Leave the episode unmarked so it can be reported after the restart hold.
		if rc.restartHeldLocked(hpa.Namespace, hpa.Name, now) {
			continue
		}
		// The episode began no earlier than the autoscaler, its last scale, the
		// limit's last transition, or the last time this controller saw the app
		// below its maximum. The maximum can be lowered onto the running count
		// without a scale.
		since := hpa.CreationTimestamp.Time
		for _, t := range []time.Time{limit.LastTransitionTime.Time, rc.belowMaxAt[key]} {
			if t.After(since) {
				since = t
			}
		}
		if last := hpa.Status.LastScaleTime; last != nil && last.After(since) {
			since = last.Time
		}
		due = append(due, autoscalingEpisode{since: since, batch: alertBatch{
			entry: ResourceLogEntry{
				Time:      nowStr,
				App:       hpa.Name,
				Namespace: hpa.Namespace,
				Action:    atMaximumAction,
				Reason: fmt.Sprintf("the app runs %d pods, the maximum of %d, and the autoscaler wants more. %s",
					hpa.Status.CurrentReplicas, hpa.Spec.MaxReplicas, atMaximumAdvice(hpa)),
				Severity: "warning",
			},
			marks: []pendingMark{{dst: rc.atMaxAlerted, key: key, at: now}},
		}})
	}
	for _, m := range []map[string]time.Time{rc.atMaxAlerted, rc.belowMaxAt} {
		for key := range m {
			if !present[key] {
				delete(m, key)
			}
		}
	}
	return due
}

// atMaximumAdvice suggests raising the maximum or the request of each metric
// the autoscaler tracks, since a larger request lowers its utilization.
func atMaximumAdvice(hpa *autoscalingv2.HorizontalPodAutoscaler) string {
	t := resourcebounds.AutoscalerMetrics(hpa)
	switch {
	case t.CPU && t.Memory:
		return "Raise the maximum, or the CPU and memory requests, when the load is expected"
	case t.CPU:
		return "Raise the maximum, or the CPU request, when the load is expected"
	case t.Memory:
		return "Raise the maximum, or the memory request, when the load is expected"
	default:
		return "Raise the maximum when the load is expected"
	}
}

// maximumLimit returns the autoscaler's ScalingLimited condition when it says
// that the maximum stops it from adding pods, and nil otherwise.
func maximumLimit(hpa *autoscalingv2.HorizontalPodAutoscaler) *autoscalingv2.HorizontalPodAutoscalerCondition {
	for i := range hpa.Status.Conditions {
		c := &hpa.Status.Conditions[i]
		if c.Type == autoscalingv2.ScalingLimited {
			if c.Status == corev1.ConditionTrue && c.Reason == "TooManyReplicas" {
				return c
			}
			return nil
		}
	}
	return nil
}

// hpaKey identifies one autoscaler object, so a replacement under the same
// name starts with no state.
func hpaKey(hpa *autoscalingv2.HorizontalPodAutoscaler) string {
	return hpa.Namespace + "/" + hpa.Name + "/" + string(hpa.UID)
}

func (rc *ResourceController) notReadyEpisodes(ctx context.Context, nowStr string) []autoscalingEpisode {
	if rc.crClient == nil {
		return nil
	}
	var apps kipperv1.AppList
	if err := rc.crClient.List(ctx, &apps); err != nil {
		return nil
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()
	present := make(map[string]bool, len(apps.Items))
	var due []autoscalingEpisode
	for i := range apps.Items {
		app := &apps.Items[i]
		// The UID makes a replacement under the same name a new episode.
		key := app.Namespace + "/" + app.Name + "/" + string(app.UID)
		present[key] = true
		c := meta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionAutoscalingReady)
		if c == nil || c.Status != metav1.ConditionFalse {
			delete(rc.notReadyAlerted, key)
			continue
		}
		if rc.notReadyAlerted[key] == c.Reason {
			continue
		}
		reason := c.Reason
		due = append(due, autoscalingEpisode{since: c.LastTransitionTime.Time, batch: alertBatch{
			entry: ResourceLogEntry{
				Time:      nowStr,
				App:       app.Name,
				Namespace: app.Namespace,
				Action:    notReadyAction,
				Reason:    fmt.Sprintf("%s: %s", c.Reason, c.Message),
				Severity:  "warning",
			},
			apply: []func(){func() { rc.notReadyAlerted[key] = reason }},
		}})
	}
	for key := range rc.notReadyAlerted {
		if !present[key] {
			delete(rc.notReadyAlerted, key)
		}
	}
	return due
}

// storedSince reports whether the store holds the alert entry would raise,
// written at or after since. The readiness warning matches on its reason as
// well, because a new reason is a new episode.
func storedSince(stored []handlers.Alert, entry ResourceLogEntry, since time.Time) bool {
	for _, a := range stored {
		if a.Namespace != entry.Namespace || a.App != entry.App || a.Action != entry.Action {
			continue
		}
		if entry.Action == notReadyAction && reasonOf(a.Reason) != reasonOf(entry.Reason) {
			continue
		}
		if at, err := time.Parse(time.RFC3339, a.Time); err == nil && !at.Before(since.Truncate(time.Second)) {
			return true
		}
	}
	return false
}

func reasonOf(text string) string {
	reason, _, _ := strings.Cut(text, ":")
	return reason
}
