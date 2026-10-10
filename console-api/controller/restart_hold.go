package controller

import (
	"context"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

// restartAlertHold allows startup grace plus five minutes for scale-in to
// reduce alerts from temporary CPU spikes after a container restart.
const restartAlertHold = startupGracePeriod + 5*time.Minute

// refreshRestarts tracks each app's latest container restart within the alert
// hold window. A failed pod list preserves the previous records.
func (rc *ResourceController) refreshRestarts(ctx context.Context, now time.Time) {
	pods, err := rc.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{LabelSelector: labels.KipperManagedSelector})
	if err != nil {
		log.Printf("resource controller: listing pods for restarts: %v", err)
		return
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	// Keep the hold active even if the restarted pod stops or is deleted.
	restarts := make(map[string]time.Time, len(rc.restarts))
	for key, at := range rc.restarts {
		if now.Sub(at) < restartAlertHold {
			restarts[key] = at
		}
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		app := pod.Labels["app"]
		at, ok := restartedAt(pod)
		if app == "" || !ok || now.Sub(at) >= restartAlertHold {
			continue
		}
		key := pod.Namespace + "/" + app
		if at.After(restarts[key]) {
			restarts[key] = at
		}
	}
	rc.restarts = restarts
}

// restartHeldLocked reports whether the app's scaling alerts are held after a
// restart. The caller holds rc.mu.
func (rc *ResourceController) restartHeldLocked(namespace, app string, now time.Time) bool {
	at, ok := rc.restarts[namespace+"/"+app]
	return ok && now.Sub(at) < restartAlertHold
}

func (rc *ResourceController) restartHeld(namespace, app string, now time.Time) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.restartHeldLocked(namespace, app, now)
}

func alertable(entries []ResourceLogEntry) []ResourceLogEntry {
	var out []ResourceLogEntry
	for _, e := range entries {
		if !e.Quiet {
			out = append(out, e)
		}
	}
	return out
}
