package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/servicecatalog"
)

const dumpFaultAction = "dump volume faulted"

// checkDumpVolumes returns warning batches for Services with DumpVolumeReady=False.
// Each episode is marked as reported once its alert is stored. A new transition
// time or Service UID starts a new episode, so a fault after recovery is eligible
// even when recovery happens between checks. A missing client or list error
// returns no batches.
func (rc *ResourceController) checkDumpVolumes(ctx context.Context) []alertBatch {
	if rc.crClient == nil {
		return nil
	}
	var services kipperv1.ServiceList
	if err := rc.crClient.List(ctx, &services); err != nil {
		return nil
	}
	nowStr := time.Now().UTC().Format(time.RFC3339)

	rc.mu.Lock()
	present := make(map[string]bool, len(services.Items))
	var due []alertEpisode
	for i := range services.Items {
		svc := &services.Items[i]
		key := svc.Namespace + "/" + svc.Name + "/" + string(svc.UID)
		present[key] = true
		c := meta.FindStatusCondition(svc.Status.Conditions, servicecatalog.ConditionDumpVolumeReady)
		if c == nil || c.Status != metav1.ConditionFalse {
			delete(rc.dumpFaultAlerted, key)
			continue
		}
		since := c.LastTransitionTime.Time
		if alerted, ok := rc.dumpFaultAlerted[key]; ok && alerted.Equal(since) {
			continue
		}
		due = append(due, alertEpisode{since: since, batch: alertBatch{
			entry: ResourceLogEntry{
				Time:      nowStr,
				App:       svc.Name,
				Namespace: svc.Namespace,
				Action:    dumpFaultAction,
				Reason:    c.Message,
				Severity:  "warning",
			},
			apply: []func(){func() { rc.dumpFaultAlerted[key] = since }},
		}})
	}
	for key := range rc.dumpFaultAlerted {
		if !present[key] {
			delete(rc.dumpFaultAlerted, key)
		}
	}
	rc.mu.Unlock()
	return rc.unstoredEpisodes(ctx, due)
}
