package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
)

func markStopped(d *appsv1.Deployment) *appsv1.Deployment {
	if d.Annotations == nil {
		d.Annotations = map[string]string{}
	}
	d.Annotations[labels.AnnoStopped] = "true"
	return d
}

// A Deployment keeps a ProgressDeadlineExceeded from a rollout that failed
// before the app was stopped. A stopped app is not a stuck rollout.
func TestCheckStuckRollouts_SkipsAStoppedApp(t *testing.T) {
	stuck := *markStopped(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging", Labels: map[string]string{"app": "web"}},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
		}}},
	})
	rc := NewResourceController(nil, nil)
	rc.rolloutAlerted["staging/web"] = time.Now().Add(-time.Minute)

	if got := entriesOf(rc.checkStuckRollouts([]appsv1.Deployment{stuck}, func(*appsv1.Deployment) string { return "" })); len(got) != 0 {
		t.Fatalf("a stopped app alerted as a stuck rollout: %+v", got)
	}
	if _, kept := rc.rolloutAlerted["staging/web"]; kept {
		t.Error("the cooldown outlived the stop, so a failed rollout after the start would stay silent")
	}
}

// Clear pre-stop samples, recommendations and crash-loop episodes so a restart
// begins with fresh observations.
func TestForgetStoppedApps(t *testing.T) {
	controller := true
	rec := &kipperv1.ResourceTuning{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app-" + workloadName, Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "App", Name: workloadName,
				UID: types.UID("uid-" + workloadName), Controller: &controller,
			}},
		},
		Spec: kipperv1.ResourceTuningSpec{Kind: "App", Name: workloadName},
		Status: kipperv1.ResourceTuningStatus{
			Recommendation: kipperv1.TunedResources{MemoryRequest: "1Gi", MemoryLimit: "1Gi"},
			RecommendedAt:  &metav1.Time{Time: time.Now()},
		},
	}
	crClient := tuningCRClient(automaticApp(), rec)
	deploy := markStopped(ownedDeployment("App", memoryContainer("512Mi", "512Mi")))
	rc := NewResourceController(fake.NewClientset(deploy), crClient)
	wk := workloadKey{Namespace: "default", Name: workloadName}
	rc.history[wk] = []usageObservation{{CPUMillis: 10}}
	rc.historySize[wk] = "512Mi"
	rc.crashLoopEpisode["default/web/app:web"] = episode{firstSeen: time.Now().Add(-time.Hour), lastSeen: time.Now()}
	rc.crashLoopEpisode["default/other/app:other"] = episode{firstSeen: time.Now(), lastSeen: time.Now()}

	rc.forgetStoppedApps(context.Background(), []appsv1.Deployment{*deploy})

	if got := tuningOf(t, crClient, "App").Status; got.Recommendation != (kipperv1.TunedResources{}) || got.RecommendedAt != nil {
		t.Errorf("the recommendation from before the stop survived: %+v", got)
	}
	if _, kept := rc.history[wk]; kept {
		t.Error("samples from before the stop survived")
	}
	if _, kept := rc.crashLoopEpisode["default/web/app:web"]; kept {
		t.Error("the crash-loop episode from before the stop survived, so a start would escalate at once")
	}
	if _, kept := rc.crashLoopEpisode["default/other/app:other"]; !kept {
		t.Error("another app's episode was dropped")
	}

	if entries := rc.processDeployment(context.Background(), deploy.DeepCopy(), usage(20, "20Mi"), nil); len(entries) != 0 {
		t.Errorf("a stopped app was tuned: %+v", entries)
	}
	if _, sampled := rc.history[wk]; sampled {
		t.Error("a stopped app was sampled")
	}
}

// A stop takes a moment to reach the Deployment and longer to reach the pods.
// Pods of an app whose App is already stopped raise nothing, and no episode is
// staged for them that a later commit would put back.
func TestStoppedAppPodsRaiseNoAlert(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Image: "api:1", Stopped: &kipperv1.AppStopped{}},
	}
	rc := NewResourceController(fake.NewClientset(crashLoopingReplica("shop-prod", "api", "api-1")), tuningCRClient(app))
	rc.readPreviousLog = func(context.Context, string, string, string) string { return "" }

	rc.forgetStoppedApps(context.Background(), nil)
	batches := rc.checkPodProblems(context.Background())
	rc.commitBatches(batches)

	if len(batches) != 0 {
		t.Errorf("a stopped app raised %+v", entriesOf(batches))
	}
	for key := range rc.crashLoopEpisode {
		if strings.HasPrefix(key, "shop-prod/api/") {
			t.Errorf("an episode for the stopped app was recorded: %s", key)
		}
	}
}

// A stop and a start can both land between two passes, leaving no marker to
// see. The stamp the stop leaves behind is what says it happened.
func TestAStopAndStartBetweenPassesStillStartsAfresh(t *testing.T) {
	deploy := ownedDeployment("App", memoryContainer("512Mi", "512Mi"))
	deploy.Annotations = map[string]string{labels.AnnoLastStop: "2026-10-03T09:00:00Z"}
	rc := NewResourceController(fake.NewClientset(deploy), tuningCRClient(automaticApp()))
	wk := workloadKey{Namespace: "default", Name: workloadName}
	rc.forgetStoppedApps(context.Background(), []appsv1.Deployment{*deploy})

	rc.history[wk] = []usageObservation{{CPUMillis: 10}}
	rc.crashLoopEpisode["default/web/app:web"] = episode{firstSeen: time.Now().Add(-time.Hour), lastSeen: time.Now()}
	rc.forgetStoppedApps(context.Background(), []appsv1.Deployment{*deploy})
	if _, kept := rc.history[wk]; !kept {
		t.Fatal("state was dropped without a new stop")
	}

	deploy.Annotations[labels.AnnoLastStop] = "2026-10-03T09:05:00Z"
	rc.forgetStoppedApps(context.Background(), []appsv1.Deployment{*deploy})
	if _, kept := rc.history[wk]; kept {
		t.Error("samples from before the stop survived")
	}
	if _, kept := rc.crashLoopEpisode["default/web/app:web"]; kept {
		t.Error("the crash-loop episode from before the stop survived")
	}
}
