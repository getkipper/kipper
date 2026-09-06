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

// Reducing every replica to one observation per workload stopped them writing
// over each other, but the reduction has to carry what each decision needs.
// Picking one replica to speak for the rest makes the answer depend on the order
// the API returned the pods, which is not a property anybody wants in an alert.
//
// Three things must not come from the representative: whether the workload is
// clean, whether every crash is an OOM, and whether any replica has read-only
// evidence.

func podWith(namespace, app, name string, statuses ...corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": app, "app.kubernetes.io/managed-by": "kipper"},
		},
		Status: corev1.PodStatus{ContainerStatuses: statuses},
	}
}

// Every caller names the main container "app", which is what the reducer keys
// on alongside the workload.
func readyStatus(restarts int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name: "app", Ready: true, RestartCount: restarts,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}
}

func unreadyStatus(name string, restarts int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name: name, Ready: false, RestartCount: restarts,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}
}

// A replica running but never becoming ready is not clean, and nothing about it
// is Waiting. Deciding cleanliness from whichever replica came first would
// announce recovery for a workload half of which is broken.
func TestAWorkloadIsCleanOnlyWhenEveryReplicaIs(t *testing.T) {
	ready := podWith("shop-prod", "api", "api-1", readyStatus(3))
	unready := podWith("shop-prod", "api", "api-2", unreadyStatus("app", 3))

	for _, order := range [][]corev1.Pod{{*ready, *unready}, {*unready, *ready}} {
		obs := observeWorkloads(order)
		require.Len(t, obs, 1)
		assert.False(t, obs[0].allClean,
			"one replica is running but not ready, so the workload is not clean whichever order it is listed in")
	}

	obs := observeWorkloads([]corev1.Pod{*ready, *podWith("shop-prod", "api", "api-3", readyStatus(1))})
	assert.True(t, obs[0].allClean, "every replica ready is a clean workload")
}

// A Terminated replica that exited badly is not clean either, and it is not
// Waiting while it sits there.
func TestATerminatedFailureIsNotClean(t *testing.T) {
	failed := podWith("shop-prod", "api", "api-2", corev1.ContainerStatus{
		Name: "app",
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
		},
	})
	obs := observeWorkloads([]corev1.Pod{
		*podWith("shop-prod", "api", "api-1", readyStatus(0)),
		*failed,
	})
	assert.False(t, obs[0].allClean)
}

// An unready replica must stop a workload announcing recovery, whichever replica
// the reducer chose to speak for it.
func TestAnUnreadyReplicaStopsTheAllClear(t *testing.T) {
	rc := NewResourceController(fake.NewClientset(
		podWith("shop-prod", "api", "api-1", readyStatus(5)),
		podWith("shop-prod", "api", "api-2", unreadyStatus("app", 5)),
	), nil)

	key := episodeKey("shop-prod", "api-1", "api", "app", mainContainer)
	rc.crashLoopEpisode[key] = episode{
		firstSeen:   time.Now().Add(-7 * time.Hour),
		escalatedAt: time.Now().Add(-time.Hour),
	}

	rc.commitBatches(rc.checkPodProblems(context.Background()))

	ep, tracked := rc.crashLoopEpisode[key]
	require.True(t, tracked, "the episode closed while a replica was not ready")
	assert.True(t, ep.readySince.IsZero(), "the recovery run started with a replica unready")
}

// OOM crash loops belong to the memory path, which raises the limit. Skipping
// the whole workload because the first replica happened to be OOM-killed hides
// a different failure in another replica.
func TestOOMOnOneReplicaDoesNotSilenceAnother(t *testing.T) {
	oom := podWith("shop-prod", "api", "api-1", corev1.ContainerStatus{
		Name:         "app",
		RestartCount: 4,
		State:        crashLooping(),
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137},
		},
	})
	other := podWith("shop-prod", "api", "api-2", corev1.ContainerStatus{
		Name:         "app",
		RestartCount: 9,
		State:        crashLooping(),
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
		},
	})

	for _, order := range []([]runtimeObj){{oom, other}, {other, oom}} {
		rc := NewResourceController(fake.NewClientset(order[0], order[1]), nil)
		rc.readPreviousLog = func(context.Context, string, string, string) string { return "" }

		batches := rc.checkPodProblems(context.Background())

		alerted := false
		for _, b := range batches {
			if b.entry.Action == "CrashLoopBackOff" {
				alerted = true
			}
		}
		assert.True(t, alerted, "a replica failing for its own reason must be alerted whatever the order")
	}
}

// Every replica being OOM-killed is still the memory path's business.
func TestEveryReplicaOOMStaysWithTheMemoryPath(t *testing.T) {
	oom := func(name string) *corev1.Pod {
		return podWith("shop-prod", "api", name, corev1.ContainerStatus{
			Name: "app", RestartCount: 4, State: crashLooping(),
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137},
			},
		})
	}
	rc := NewResourceController(fake.NewClientset(oom("api-1"), oom("api-2")), nil)
	rc.readPreviousLog = func(context.Context, string, string, string) string { return "" }

	for _, b := range rc.checkPodProblems(context.Background()) {
		assert.NotEqual(t, "CrashLoopBackOff", b.entry.Action,
			"every replica is OOM-killed, which the memory path handles by raising the limit")
	}
}

// Read-only evidence lives in one replica's log. Reading only the representative
// would make the diagnosis depend on pod order.
func TestReadOnlyEvidenceIsFoundInAnyFailingReplica(t *testing.T) {
	claim := func(name string) *corev1.Pod {
		p := podWith("shop-test", "db", name, corev1.ContainerStatus{
			Name: "postgres", RestartCount: 40, State: crashLooping(),
		})
		p.Spec = corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-" + name},
				},
			}},
			Containers: []corev1.Container{{
				Name:         "postgres",
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/postgresql"}},
			}},
		}
		return p
	}

	for _, order := range []([]runtimeObj){{claim("db-0"), claim("db-1")}, {claim("db-1"), claim("db-0")}} {
		rc := NewResourceController(fake.NewClientset(order[0], order[1]), nil)
		// Only one replica's disk went read-only.
		rc.readPreviousLog = func(_ context.Context, _, pod, _ string) string {
			if pod == "db-1" {
				return `FATAL:  could not remove old lock file "postmaster.pid": Read-only file system`
			}
			return "FATAL: connection refused"
		}

		batches := rc.checkPodProblems(context.Background())
		require.Len(t, batches, 1)
		assert.Equal(t, "VolumeReadOnly", batches[0].entry.Action,
			"the evidence is in one replica's log; which one must not depend on list order")
		assert.Contains(t, batches[0].entry.Reason, "data-db-1",
			"and the claim named must be the one belonging to the replica that has the evidence")
	}
}

type runtimeObj = *corev1.Pod

// Kubernetes allows an init container and a main container to share a name.
// Keyed only by name they become one episode, and the init container completing
// successfully would mask the main container failing.
func TestInitAndMainContainersOfOneNameAreSeparate(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-0", Namespace: "shop-prod",
			Labels: map[string]string{"app": "api", "app.kubernetes.io/managed-by": "kipper"},
		},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  "api",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
			}},
			ContainerStatuses: []corev1.ContainerStatus{unreadyStatus("api", 12)},
		},
	}

	obs := observeWorkloads([]corev1.Pod{*pod})
	require.Len(t, obs, 2, "an init container and a main container of the same name are two things")

	for _, o := range obs {
		if o.status.State.Terminated != nil {
			assert.True(t, o.allClean, "the init container did its job")
			continue
		}
		assert.False(t, o.allClean, "the main container is running but not ready")
	}
}

// One replica being replaced drops the workload's total restart count. Compared
// against that total, a different replica crashing afterwards reads as a lower
// number and the crash is missed. The baseline is per pod for that reason.
func TestACrashIsSeenAfterAnotherReplicaWasReplaced(t *testing.T) {
	ep := episode{
		firstSeen:     time.Now().Add(-time.Hour),
		restartsByPod: map[string]int32{"pod-1": 5, "pod-2": 5},
	}

	// pod-2 is gone, replaced by pod-3 starting from zero, and pod-1 has
	// crashed once more. The workload's total fell from 10 to 6.
	assert.False(t, ep.stillClean(map[string]int32{"pod-1": 6, "pod-3": 0}),
		"pod-1 crashed again; a total that fell because another pod was replaced must not hide it")

	assert.True(t, ep.stillClean(map[string]int32{"pod-1": 5, "pod-3": 0}),
		"nothing crashed: one pod was replaced and the other is where it was")
}

// A pod nobody has seen before brings its own history, which is not a crash that
// happened on our watch.
func TestANewPodsHistoryIsNotANewCrash(t *testing.T) {
	ep := episode{firstSeen: time.Now().Add(-time.Hour), restartsByPod: map[string]int32{"pod-1": 5}}

	arrived := map[string]int32{"pod-1": 5, "pod-2": 12}
	assert.True(t, ep.stillClean(arrived),
		"pod-2 arrived carrying twelve restarts; none of them happened since the last look")

	// The tick that saw it records it, and from then on it is watched like any
	// other.
	ep.observeClean(time.Now(), arrived, 17)
	assert.False(t, ep.stillClean(map[string]int32{"pod-1": 5, "pod-2": 13}),
		"but the thirteenth happened on our watch")
}

// The per-pod baseline must not accumulate every pod a workload has ever had.
// A Deployment redeployed daily for a year would otherwise carry a year of dead
// pod names in an episode that never closes.
func TestTheBaselineHoldsOnlyTheCurrentPods(t *testing.T) {
	ep := episode{
		firstSeen:     time.Now().Add(-time.Hour),
		restartsByPod: map[string]int32{"old-1": 3, "old-2": 4, "old-3": 5},
	}

	ep.observeClean(time.Now(), map[string]int32{"new-1": 0}, 0)

	assert.Equal(t, map[string]int32{"new-1": 0}, ep.restartsByPod,
		"the baseline is this tick's pods, not every pod the workload has had")
}

// And the same on the alerting side, which is where a long crash loop spends
// its time.
func TestTheBaselineIsReplacedWhileCrashLooping(t *testing.T) {
	rc := NewResourceController(nil, nil)
	key := "shop-prod/api/app:app"
	rc.crashLoopEpisode[key] = episode{
		firstSeen:     time.Now().Add(-time.Hour),
		restartsByPod: map[string]int32{"gone-1": 3, "gone-2": 4},
	}

	rc.touchEpisode(key, workloadObservation{
		counts:   map[string]int32{"api-live": 9},
		restarts: 9,
	}, time.Now())

	assert.Equal(t, map[string]int32{"api-live": 9}, rc.crashLoopEpisode[key].restartsByPod)
}

// A container can crash and be back up between two one-minute observations, so
// the controller sees a higher restart count without ever seeing the crash
// loop. That has to interrupt the recovery run once and then become the new
// baseline. Left stale, every later healthy observation compares against a
// number it can never reach again, the run restarts forever, the all-clear never
// goes out, and the episode cannot even age out because it is still being
// observed.
func TestAnUnobservedCrashInterruptsRecoveryOnceThenRebaselines(t *testing.T) {
	rc := NewResourceController(fake.NewClientset(
		podWith("shop-prod", "api", "api-1", readyStatus(6)),
	), nil)

	key := episodeKey("shop-prod", "api-1", "api", "app", mainContainer)
	rc.crashLoopEpisode[key] = episode{
		firstSeen:     time.Now().Add(-7 * time.Hour),
		escalatedAt:   time.Now().Add(-time.Hour),
		readySince:    time.Now().Add(-9 * time.Minute),
		restartsByPod: map[string]int32{"api-1": 5},
	}

	// The tick that notices the count moved: the clean run is void.
	rc.commitBatches(rc.checkPodProblems(context.Background()))
	ep := rc.crashLoopEpisode[key]
	require.True(t, ep.readySince.IsZero(), "a crash we did not see is still a crash")
	assert.EqualValues(t, 6, ep.restartsByPod["api-1"],
		"and the count it crashed to is what the next ten minutes are measured from")

	// The next tick starts the run again, because nothing has crashed since.
	rc.commitBatches(rc.checkPodProblems(context.Background()))
	assert.False(t, rc.crashLoopEpisode[key].readySince.IsZero(),
		"the recovery run has to be able to start again, or the episode is stuck for good")
}

// An image-pull failure and a crash loop are independent problems with
// independent remedies, and a workload can have both at once: one replica
// OOM-looping while a replacement cannot pull a new image. Reducing to one
// waiting reason and then treating it as the whole story let the crash loop
// swallow the pull failure, and the OOM path swallowed both.
func TestAnImagePullFailureIsNotHiddenByAnotherReplicasCrashLoop(t *testing.T) {
	oom := podWith("shop-prod", "api", "api-1", corev1.ContainerStatus{
		Name: "app", RestartCount: 4, State: crashLooping(),
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137},
		},
	})
	pulling := podWith("shop-prod", "api", "api-2", corev1.ContainerStatus{
		Name: "app",
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"},
		},
	})

	for _, order := range []([]runtimeObj){{oom, pulling}, {pulling, oom}} {
		rc := NewResourceController(fake.NewClientset(order[0], order[1]), nil)
		rc.readPreviousLog = func(context.Context, string, string, string) string { return "" }

		actions := map[string]bool{}
		for _, b := range rc.checkPodProblems(context.Background()) {
			actions[b.entry.Action] = true
		}
		assert.True(t, actions["ImagePullBackOff"],
			"a replica that cannot pull its image needs saying whatever another replica is doing")
	}
}

// Both problems at once, neither of them OOM, is two alerts.
func TestACrashLoopAndAPullFailureAreBothReported(t *testing.T) {
	looping := podWith("shop-prod", "api", "api-1", corev1.ContainerStatus{
		Name: "app", RestartCount: 9, State: crashLooping(),
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
		},
	})
	pulling := podWith("shop-prod", "api", "api-2", corev1.ContainerStatus{
		Name: "app",
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"},
		},
	})
	rc := NewResourceController(fake.NewClientset(looping, pulling), nil)
	rc.readPreviousLog = func(context.Context, string, string, string) string { return "" }

	actions := map[string]bool{}
	for _, b := range rc.checkPodProblems(context.Background()) {
		actions[b.entry.Action] = true
	}
	assert.True(t, actions["CrashLoopBackOff"], "the crash loop")
	assert.True(t, actions["ImagePullBackOff"], "and the pull failure")
}

// A replacement pod that cannot be scheduled sits Pending with no container
// statuses at all. Reducing only what has a status made the workload look
// entirely clean, so a replica that had never once run declared the incident
// over. Absence of an observation is not a clean observation.
func TestAPodWithNoStatusYetHoldsRecoveryOpen(t *testing.T) {
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-2", Namespace: "shop-prod",
			Labels: map[string]string{"app": "api", "app.kubernetes.io/managed-by": "kipper"},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	healthy := podWith("shop-prod", "api", "api-1", readyStatus(3))
	healthy.Spec = corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}

	obs := observeWorkloads([]corev1.Pod{*healthy, *pending})
	require.Len(t, obs, 1)
	assert.False(t, obs[0].allClean,
		"one replica has not started, so the workload is not running clean")
}

// And end to end: no all-clear while a replica is still Pending.
func TestNoAllClearWhileAReplicaHasNotStarted(t *testing.T) {
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-2", Namespace: "shop-prod",
			Labels: map[string]string{"app": "api", "app.kubernetes.io/managed-by": "kipper"},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	healthy := podWith("shop-prod", "api", "api-1", readyStatus(3))
	healthy.Spec = corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}

	rc := NewResourceController(fake.NewClientset(healthy, pending), nil)
	key := episodeKey("shop-prod", "api-1", "api", "app", mainContainer)
	rc.crashLoopEpisode[key] = episode{
		firstSeen:   time.Now().Add(-7 * time.Hour),
		escalatedAt: time.Now().Add(-time.Hour),
	}

	rc.commitBatches(rc.checkPodProblems(context.Background()))

	ep, tracked := rc.crashLoopEpisode[key]
	require.True(t, tracked, "the episode closed while a replica had not started")
	assert.True(t, ep.readySince.IsZero())
}

// A workload where no replica has published a status yet is observed entirely
// from the pod spec, so its observation carries no container status at all.
// Nothing downstream may assume one is there.
func TestAWorkloadObservedOnlyFromItsSpec(t *testing.T) {
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-1", Namespace: "shop-prod",
			Labels: map[string]string{"app": "api", "app.kubernetes.io/managed-by": "kipper"},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}

	obs := observeWorkloads([]corev1.Pod{*pending})
	require.Len(t, obs, 1)
	assert.Nil(t, obs[0].status, "nothing has reported on this container yet")
	assert.False(t, obs[0].allClean)

	// The scan must survive it, with an episode open and without one.
	rc := NewResourceController(fake.NewClientset(pending), nil)
	assert.NotPanics(t, func() { rc.commitBatches(rc.checkPodProblems(context.Background())) })

	rc.crashLoopEpisode[obs[0].key] = episode{
		firstSeen:   time.Now().Add(-7 * time.Hour),
		escalatedAt: time.Now().Add(-time.Hour),
		readySince:  time.Now().Add(-time.Hour),
	}
	assert.NotPanics(t, func() { rc.commitBatches(rc.checkPodProblems(context.Background())) })
	assert.True(t, rc.crashLoopEpisode[obs[0].key].readySince.IsZero(),
		"a container nobody has reported on is not running clean")
}

// A status for a container the spec no longer declares, which is what a rollout
// mid-flight looks like, must still be observed rather than dropped.
func TestAStatusWithoutASpecEntryIsStillObserved(t *testing.T) {
	pod := podWith("shop-prod", "api", "api-1", readyStatus(3))
	pod.Spec = corev1.PodSpec{Containers: []corev1.Container{{Name: "renamed"}}}

	obs := observeWorkloads([]corev1.Pod{*pod})
	require.Len(t, obs, 2, "the container that is running and the one the spec now names")

	byName := map[string]workloadObservation{}
	for _, o := range obs {
		byName[o.key] = o
	}
	running := byName[episodeKey("shop-prod", "api-1", "api", "app", mainContainer)]
	require.NotNil(t, running.status)
	assert.True(t, running.allClean, "the container that is up is up")
}

// The recovery this alerts prescribe replaces the pod, and the pod being
// replaced can sit Terminating for a while behind a finalizer or a volume
// detach. Counting it against the workload means following the advice keeps the
// incident open, which is the recovery detector working against the recovery.
func TestATerminatingPodDoesNotHoldRecoveryOpen(t *testing.T) {
	deleted := metav1.NewTime(time.Now().Add(-time.Minute))
	terminating := podWith("shop-prod", "api", "api-old", corev1.ContainerStatus{
		Name: "app", RestartCount: 30, State: crashLooping(),
	})
	terminating.DeletionTimestamp = &deleted
	terminating.Finalizers = []string{"example.com/holding-on"}
	terminating.Spec = corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}

	healthy := podWith("shop-prod", "api", "api-new", readyStatus(0))
	healthy.Spec = corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}

	obs := observeWorkloads([]corev1.Pod{*terminating, *healthy})
	require.Len(t, obs, 1)
	assert.True(t, obs[0].allClean, "the pod on its way out is not a replica any more")
	assert.Empty(t, obs[0].failing, "and its crash loop is not this workload's problem now")
}

// A workload whose every pod is terminating has nothing to say either way, and
// must not be treated as recovered on the strength of no observations at all.
func TestAWorkloadWithOnlyTerminatingPods(t *testing.T) {
	deleted := metav1.NewTime(time.Now())
	pod := podWith("shop-prod", "api", "api-old", readyStatus(3))
	pod.DeletionTimestamp = &deleted
	pod.Finalizers = []string{"example.com/holding-on"}

	assert.Empty(t, observeWorkloads([]corev1.Pod{*pod}),
		"a workload with nothing left running is not an observation")
}
