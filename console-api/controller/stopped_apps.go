package controller

import (
	"context"
	"log"
	"strings"

	appsv1 "k8s.io/api/apps/v1"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
)

// deploymentStopped reports whether the Deployment carries Kipper's stop marker.
func deploymentStopped(d *appsv1.Deployment) bool {
	return d.Annotations[labels.AnnoStopped] != ""
}

// forgetStoppedApps resets usage history and crash-loop episodes for apps marked
// stopped on either the App or Deployment. Deployment markers also trigger
// recommendation cleanup. The last-stop timestamp catches stop/start cycles
// between passes, so stale samples and episodes cannot affect the restarted app.
func (rc *ResourceController) forgetStoppedApps(ctx context.Context, deployments []appsv1.Deployment) {
	stopped := map[string]bool{}
	if rc.crClient != nil {
		var apps kipperv1.AppList
		if err := rc.crClient.List(ctx, &apps); err == nil {
			for i := range apps.Items {
				if apps.Items[i].Spec.Stopped != nil {
					stopped[apps.Items[i].Namespace+"/"+apps.Items[i].Name] = true
				}
			}
		}
	}

	stamps := make(map[string]string, len(deployments))
	var fresh []string
	for i := range deployments {
		d := &deployments[i]
		key := d.Namespace + "/" + appNameOf(d)
		if deploymentStopped(d) {
			stopped[key] = true
			rc.clearRecommendationOf(ctx, d)
		}
		if stamp := d.Annotations[labels.AnnoLastStop]; stamp != "" {
			stamps[key] = stamp
		}
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()
	for key, stamp := range stamps {
		if rc.lastStop[key] != stamp {
			fresh = append(fresh, key)
		}
	}
	rc.lastStop = stamps
	rc.stoppedApps = stopped
	for key := range stopped {
		rc.forgetLocked(key)
	}
	for _, key := range fresh {
		rc.forgetLocked(key)
	}
}

// forgetLocked drops the samples and crash-loop episodes of one app, keyed
// namespace/app. The caller holds rc.mu.
func (rc *ResourceController) forgetLocked(key string) {
	namespace, name, _ := strings.Cut(key, "/")
	wk := workloadKey{Namespace: namespace, Name: name}
	delete(rc.history, wk)
	delete(rc.historySize, wk)
	for episode := range rc.crashLoopEpisode {
		if strings.HasPrefix(episode, key+"/") {
			delete(rc.crashLoopEpisode, episode)
		}
	}
}

// appStoppedLocked reports whether the last pass found the app, keyed
// namespace/app, stopped. The caller holds rc.mu.
func (rc *ResourceController) appStoppedLocked(key string) bool {
	return rc.stoppedApps[key]
}

func appNameOf(d *appsv1.Deployment) string {
	if name := d.Labels["app"]; name != "" {
		return name
	}
	return d.Name
}

func (rc *ResourceController) clearRecommendationOf(ctx context.Context, d *appsv1.Deployment) {
	owner, ok := ownerOf(d)
	if !ok || rc.crClient == nil {
		return
	}
	rt, isNew, err := rc.loadTuning(ctx, d.Namespace, owner)
	if err != nil || isNew {
		return
	}
	if rt.Status.Recommendation == (kipperv1.TunedResources{}) && rt.Status.RecommendedAt == nil {
		return
	}
	rt.Status.Recommendation = kipperv1.TunedResources{}
	rt.Status.RecommendedAt = nil
	if err := rc.crClient.Status().Update(ctx, rt); err != nil {
		log.Printf("resource controller: clearing the recommendation of stopped %s/%s: %v", d.Namespace, d.Name, err)
	}
}
