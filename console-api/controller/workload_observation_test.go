package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// An episode belongs to a workload, and a Deployment has several pods. Once the
// key stopped naming a pod, every per-pod observation started writing to shared
// state: two replicas each raised the first alert, each committed a whole
// episode snapshot over the other's, and their unrelated lifetime restart counts
// fought over one baseline so the recovery clock could never run down.
//
// One observation per workload per tick is what these pin.

func replicaPod(namespace, app, name string, state corev1.ContainerState, restarts int32, ready bool) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": app, "app.kubernetes.io/managed-by": "kipper"},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:         "app",
			RestartCount: restarts,
			Ready:        ready,
			State:        state,
		}}},
	}
}

func crashLooping() corev1.ContainerState {
	return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
}

func running() corev1.ContainerState {
	return corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
}

// Two replicas failing is one incident, so it is one alert. Ten replicas failing
// would otherwise consume most of the twenty-five-alert write allowance with
// copies of the same message.
func TestTwoFailingReplicasRaiseOneAlert(t *testing.T) {
	rc := NewResourceController(fake.NewClientset(
		replicaPod("shop-prod", "api", "api-1", crashLooping(), 8, false),
		replicaPod("shop-prod", "api", "api-2", crashLooping(), 3, false),
	), nil)
	rc.readPreviousLog = func(context.Context, string, string, string) string { return "" }

	batches := rc.checkPodProblems(context.Background())

	crashLoops := 0
	for _, b := range batches {
		if b.entry.Action == "CrashLoopBackOff" {
			crashLoops++
		}
	}
	assert.Equal(t, 1, crashLoops, "two replicas of one workload failing is one incident, got %d alerts", crashLoops)
}

// Healthy replicas carry unrelated lifetime restart counts. Written to one
// shared baseline they take turns failing each other's cleanliness check, and
// each failure zeroes the recovery clock, so ten clean minutes never elapse and
// the all-clear never goes out.
func TestHealthyReplicasWithDifferentRestartCountsStillRecover(t *testing.T) {
	rc := NewResourceController(fake.NewClientset(
		replicaPod("shop-prod", "api", "api-1", running(), 10, true),
		replicaPod("shop-prod", "api", "api-2", running(), 5, true),
	), nil)

	key := episodeKey("shop-prod", "api-1", "api", "app", mainContainer)
	start := time.Now().Add(-30 * time.Minute)
	rc.crashLoopEpisode[key] = episode{
		firstSeen:   start.Add(-7 * time.Hour),
		escalatedAt: start.Add(-time.Hour),
		restarts:    15,
	}

	// Several ticks, as the controller runs them.
	var first time.Time
	for tick := 0; tick < 5; tick++ {
		rc.commitBatches(rc.checkPodProblems(context.Background()))
		ep := rc.crashLoopEpisode[key]
		require.False(t, ep.readySince.IsZero(),
			"tick %d zeroed the recovery clock while every replica was healthy", tick)
		if tick == 0 {
			first = ep.readySince
		} else {
			assert.Equal(t, first, ep.readySince,
				"tick %d restarted the recovery run, so it can never reach ten minutes", tick)
		}
	}
}

// A workload is recovering only when none of its replicas is failing.
func TestOneFailingReplicaHoldsTheWorkloadOpen(t *testing.T) {
	rc := NewResourceController(fake.NewClientset(
		replicaPod("shop-prod", "api", "api-1", crashLooping(), 8, false),
		replicaPod("shop-prod", "api", "api-2", running(), 2, true),
	), nil)
	rc.readPreviousLog = func(context.Context, string, string, string) string { return "" }

	key := episodeKey("shop-prod", "api-1", "api", "app", mainContainer)
	rc.crashLoopEpisode[key] = episode{
		firstSeen:   time.Now().Add(-7 * time.Hour),
		escalatedAt: time.Now().Add(-time.Hour),
	}
	rc.crashLoopAlerted[key] = time.Now()

	rc.commitBatches(rc.checkPodProblems(context.Background()))

	ep, tracked := rc.crashLoopEpisode[key]
	require.True(t, tracked)
	assert.True(t, ep.readySince.IsZero(),
		"a recovery run started while a replica of the same workload is failing")
}

// A crash in any replica is a crash. The baseline is the workload's total, so a
// second replica starting to fail is still seen after the first has settled.
func TestRestartBaselineCountsTheWholeWorkload(t *testing.T) {
	rc := NewResourceController(fake.NewClientset(
		replicaPod("blog-test", "api", "api-1", running(), 10, true),
		replicaPod("blog-test", "api", "api-2", running(), 5, true),
	), nil)

	key := episodeKey("blog-test", "api-1", "api", "app", mainContainer)
	rc.crashLoopEpisode[key] = episode{firstSeen: time.Now().Add(-time.Hour), restarts: 15}

	rc.commitBatches(rc.checkPodProblems(context.Background()))

	assert.EqualValues(t, 15, rc.crashLoopEpisode[key].restarts,
		"the baseline is the workload's total restarts, not whichever pod was seen last")
}

// A service's init container mounts the same data volume the database does, so
// a read-only volume can stop the pod before its main container ever starts.
// Only the main containers were scanned, so that pod sat in Init:CrashLoopBackOff
// with nothing said about it.
func TestAnInitContainerCrashLoopIsSeen(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db-0",
			Namespace: "shop-test",
			Labels:    map[string]string{"app": "db", "app.kubernetes.io/managed-by": "kipper"},
		},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name:         "remove-lost-found",
				RestartCount: 40,
				State:        crashLooping(),
			}},
		},
	}
	rc := NewResourceController(fake.NewClientset(pod), nil)
	rc.readPreviousLog = func(context.Context, string, string, string) string { return "" }

	batches := rc.checkPodProblems(context.Background())

	require.Len(t, batches, 1, "an init container crash-looping is a pod that never starts")
	assert.Equal(t, "CrashLoopBackOff", batches[0].entry.Action)
	assert.Contains(t, batches[0].entry.Reason, "remove-lost-found")
}

// An init container that has done its job is Terminated, never Running, so the
// ordinary readiness test would hold its episode open for good and never send
// the all-clear.
func TestACompletedInitContainerRecovers(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db-0",
			Namespace: "shop-test",
			Labels:    map[string]string{"app": "db", "app.kubernetes.io/managed-by": "kipper"},
		},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name:         "remove-lost-found",
				RestartCount: 40,
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"},
				},
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "postgres", Ready: true, State: running(),
			}},
		},
	}
	rc := NewResourceController(fake.NewClientset(pod), nil)

	key := episodeKey("shop-test", "db-0", "db", "remove-lost-found", initContainer)
	rc.crashLoopEpisode[key] = episode{
		firstSeen:   time.Now().Add(-7 * time.Hour),
		escalatedAt: time.Now().Add(-time.Hour),
		restarts:    40,
	}

	rc.commitBatches(rc.checkPodProblems(context.Background()))

	ep := rc.crashLoopEpisode[key]
	assert.False(t, ep.readySince.IsZero(),
		"an init container that completed has done everything it is ever going to do; its episode has to be able to close")
}

// Aggregating by workload must not merge two workloads. Two apps failing in one
// namespace are two incidents, each with its own episode and its own alert.
func TestTwoWorkloadsFailingAreTwoAlerts(t *testing.T) {
	rc := NewResourceController(fake.NewClientset(
		replicaPod("shop-prod", "api", "api-1", crashLooping(), 8, false),
		replicaPod("shop-prod", "api", "api-2", crashLooping(), 3, false),
		replicaPod("shop-prod", "worker", "worker-1", crashLooping(), 5, false),
	), nil)
	rc.readPreviousLog = func(context.Context, string, string, string) string { return "" }

	batches := rc.checkPodProblems(context.Background())

	apps := map[string]int{}
	for _, b := range batches {
		if b.entry.Action == "CrashLoopBackOff" {
			apps[b.entry.App]++
		}
	}
	assert.Equal(t, map[string]int{"api": 1, "worker": 1}, apps,
		"one alert per workload: the two api replicas are one incident, worker is another")
}

// Replicas can be waiting for different reasons. A crash loop is the one with
// the escalation ladder and the recovery command behind it, so it must not be
// masked by a replica that merely cannot pull an image, whichever the pod list
// happens to return first.
func TestACrashLoopOutranksAnotherReplicasImagePull(t *testing.T) {
	pulling := replicaPod("shop-prod", "api", "api-1", corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"},
	}, 0, false)
	looping := replicaPod("shop-prod", "api", "api-2", crashLooping(), 30, false)

	for _, order := range [][]*corev1.Pod{{pulling, looping}, {looping, pulling}} {
		obs := observeWorkloads([]corev1.Pod{*order[0], *order[1]})
		require.Len(t, obs, 1)
		assert.Equal(t, "CrashLoopBackOff", obs[0].waiting,
			"a crash loop is the more serious signal and must win whichever pod is listed first")
		assert.Equal(t, "api-2", obs[0].pod.Name,
			"and the alert must quote the replica that is actually crash-looping")
	}
}
