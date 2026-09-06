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

// A Postgres service crash-looped for three and a half days behind a read-only
// volume and produced the same hourly warning eighty-four times. Nobody reads
// the eighty-fourth copy of a message they ignored at the sixth, and the bell
// caps at fifty, so the flood evicted the first alert — the one whose timestamp
// coincided with the volume going read-only.
//
// The ladder: hourly warnings for six hours, then one critical, then daily.
// Eleven alerts over that incident rather than eighty-four.

const hour = time.Hour

func at(h float64) time.Time {
	return time.Unix(1_780_000_000, 0).UTC().Add(time.Duration(h * float64(hour)))
}

func TestEpisodeStage(t *testing.T) {
	tests := []struct {
		name      string
		ep        episode
		now       time.Time
		wantAlert bool
		wantStage string
	}{
		{
			name:      "first sight of a crash loop warns",
			ep:        episode{},
			now:       at(0),
			wantAlert: true,
			wantStage: stageWarning,
		},
		{
			name:      "inside the hour it says nothing",
			ep:        episode{firstSeen: at(0), lastAlerted: at(0)},
			now:       at(0.5),
			wantAlert: false,
		},
		{
			name:      "an hour on it warns again",
			ep:        episode{firstSeen: at(0), lastAlerted: at(0)},
			now:       at(1.1),
			wantAlert: true,
			wantStage: stageWarning,
		},
		{
			name:      "at five hours it is still only warning",
			ep:        episode{firstSeen: at(0), lastAlerted: at(4)},
			now:       at(5),
			wantAlert: true,
			wantStage: stageWarning,
		},
		{
			name:      "at six hours it escalates",
			ep:        episode{firstSeen: at(0), lastAlerted: at(5)},
			now:       at(6),
			wantAlert: true,
			wantStage: stageCritical,
		},
		{
			name:      "having escalated it goes quiet for a day, not an hour",
			ep:        episode{firstSeen: at(0), lastAlerted: at(6), escalatedAt: at(6)},
			now:       at(8),
			wantAlert: false,
		},
		{
			name:      "a day after escalating it says so again",
			ep:        episode{firstSeen: at(0), lastAlerted: at(6), escalatedAt: at(6)},
			now:       at(30.1),
			wantAlert: true,
			wantStage: stageCritical,
		},
		{
			name:      "and keeps saying so, because silence is the failure being fixed",
			ep:        episode{firstSeen: at(0), lastAlerted: at(78), escalatedAt: at(6)},
			now:       at(102.1),
			wantAlert: true,
			wantStage: stageCritical,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stage, alert := tc.ep.stageAt(tc.now)
			assert.Equal(t, tc.wantAlert, alert)
			if tc.wantAlert {
				assert.Equal(t, tc.wantStage, stage)
			}
		})
	}
}

// Escalation is measured from when the episode began, not from a restart count.
// RestartCount is cumulative for the pod's lifetime, so a StatefulSet pod
// carrying months of occasional restarts would escalate on the first tick of a
// fresh loop, announcing that it will not recover while somebody is mid-fix.
func TestEpisodeStage_MeasuresTheEpisodeNotThePodLifetime(t *testing.T) {
	ep := episode{firstSeen: at(0), lastAlerted: at(0), restarts: 800}
	_, alert := ep.stageAt(at(0.5))
	assert.False(t, alert, "a long restart history must not shortcut the clock")

	stage, alert := ep.stageAt(at(6))
	require.True(t, alert)
	assert.Equal(t, stageCritical, stage, "six hours of this episode is what escalates it")
}

// Ten one-minute observations span nine minutes, not ten. Recovery is measured
// from a timestamp, so a delayed tick cannot shorten it either.
func TestEpisodeRecovery_MeasuredInTimeNotTicks(t *testing.T) {
	ep := episode{firstSeen: at(0), escalatedAt: at(6), readySince: at(10)}

	assert.False(t, ep.recoveredAt(at(10).Add(9*time.Minute)),
		"nine minutes of calm is not ten")
	assert.True(t, ep.recoveredAt(at(10).Add(10*time.Minute)),
		"ten minutes clean clears it")
}

func TestEpisodeRecovery_NeedsAnUninterruptedRun(t *testing.T) {
	ep := episode{firstSeen: at(0), readySince: time.Time{}}
	assert.False(t, ep.recoveredAt(at(20)), "a container that was never ready has not recovered")
}

// An episode survives observations that are neither crash-looping nor healthy:
// Pending under another reason, or Running but not yet Ready. Resetting on those
// would let the brief Running phase between crash attempts stop escalation ever
// firing; treating them as crash-loop time would escalate a container that is
// merely slow. They pause the recovery clock and leave the episode standing.
func TestEpisodeUnsettled_PausesRecoveryAndKeepsTheEpisode(t *testing.T) {
	ep := episode{firstSeen: at(0), readySince: at(1)}

	ep.observeUnsettled(at(2))

	assert.True(t, ep.readySince.IsZero(), "an unsettled observation restarts the recovery run")
	assert.Equal(t, at(0), ep.firstSeen, "but the episode itself stands, so the six-hour clock keeps running")
	assert.False(t, ep.recoveredAt(at(2).Add(11*time.Minute)), "and recovery cannot complete from an unsettled state")
}

// The all-clear goes only to people who were told there was a problem.
func TestEpisodeAllClear_OnlyAfterEscalation(t *testing.T) {
	escalated := episode{firstSeen: at(0), escalatedAt: at(6)}
	assert.True(t, escalated.deservesAllClear(), "somebody was woken; they are owed the news it is over")

	brief := episode{firstSeen: at(0)}
	assert.False(t, brief.deservesAllClear(), "a four-minute blip that only warned closes quietly")
}

// The state sweep ages an episode on its last observation, not its first. A
// three-day crash loop has a firstSeen well past the 24h TTL, and sweeping on
// that would delete the episode at the moment it is about to escalate.
func TestPruneAlertState_KeepsALiveEpisodeOlderThanTheTTL(t *testing.T) {
	rc := NewResourceController(nil, nil)
	now := time.Now()

	rc.crashLoopEpisode["ns/db-0/postgres"] = episode{
		firstSeen: now.Add(-72 * hour), // three days of crash looping
		lastSeen:  now.Add(-time.Minute),
	}
	rc.crashLoopEpisode["ns/gone-0/old"] = episode{
		firstSeen: now.Add(-72 * hour),
		lastSeen:  now.Add(-72 * hour), // pod deleted, never seen again
	}

	rc.pruneAlertState()

	_, live := rc.crashLoopEpisode["ns/db-0/postgres"]
	assert.True(t, live, "a live three-day episode must survive the sweep")
	_, stale := rc.crashLoopEpisode["ns/gone-0/old"]
	assert.False(t, stale, "an episode nothing has observed since the TTL ages out")
}

// The recovery this alert prescribes is `kip service restart`, which recreates
// the pod. A recreated pod counts its restarts from zero, and the episode is
// keyed by pod name so it survives the recreation. Requiring the count to match
// what the episode last saw therefore made the one recovery the alert asks for
// unrecognisable: every healthy observation read as unsettled, the all-clear
// never went out, and the episode never closed.
func TestEpisodeCrashCounting(t *testing.T) {
	tests := []struct {
		name     string
		baseline map[string]int32
		observed map[string]int32
		want     bool
	}{
		{
			name:     "same pod, no new crash",
			baseline: map[string]int32{"db-0": 72},
			observed: map[string]int32{"db-0": 72},
			want:     true,
		},
		{
			name:     "crashed again",
			baseline: map[string]int32{"db-0": 72},
			observed: map[string]int32{"db-0": 73},
			want:     false,
		},
		{
			name:     "pod recreated under the same name, so its count restarts from zero",
			baseline: map[string]int32{"db-0": 72},
			observed: map[string]int32{"db-0": 0},
			want:     true,
		},
		{
			name:     "a fresh episode with no crashes yet",
			baseline: nil,
			observed: map[string]int32{"db-0": 0},
			want:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := episode{restartsByPod: tc.baseline}
			assert.Equal(t, tc.want, ep.stillClean(tc.observed))
		})
	}
}

// After a recreation the episode has to take the new count as its baseline, or
// a container that crashes once post-recovery still reads as clean.
func TestEpisodeRebaselinesAfterARecreation(t *testing.T) {
	ep := episode{firstSeen: at(0), escalatedAt: at(6), restartsByPod: map[string]int32{"db-0": 72}}

	ep.observeClean(at(10), map[string]int32{"db-0": 0}, 0)
	assert.EqualValues(t, 0, ep.restarts, "the replacement pod's count is the new baseline")

	assert.False(t, ep.stillClean(map[string]int32{"db-0": 1}), "a crash after the recovery is a crash again")
}

// Every Waiting state means the container is not running clean, so every one of
// them has to restart the recovery run. Only CrashLoopBackOff was doing that,
// which let a container go Ready for nine minutes, spend a tick in
// ContainerCreating or ImagePullBackOff, come back, and be declared recovered
// on the strength of one clean observation.
func TestWaitingStatesHoldTheRecoveryClock(t *testing.T) {
	for _, reason := range []string{"ImagePullBackOff", "ErrImagePull", "ContainerCreating", "PodInitializing", "CreateContainerConfigError"} {
		t.Run(reason, func(t *testing.T) {
			rc := NewResourceController(nil, nil)
			key := "ns/db-0/postgres"
			rc.crashLoopEpisode[key] = episode{firstSeen: at(0), escalatedAt: at(6), readySince: at(10)}

			rc.holdEpisode(key, at(10).Add(9*time.Minute))

			ep := rc.crashLoopEpisode[key]
			assert.True(t, ep.readySince.IsZero(), "a waiting container has not been running clean")
			assert.Equal(t, at(0), ep.firstSeen, "the episode itself stands")
			assert.False(t, ep.recoveredAt(at(10).Add(11*time.Minute)),
				"recovery must be measured from the interruption, not from before it")
		})
	}
}

// An OOM crash loop is alerted through the memory path instead, but it is still
// a crash loop, so an episode that is already open must not quietly age out or
// come back reading as recovered.
func TestOOMCrashLoopStillHoldsAnOpenEpisode(t *testing.T) {
	rc := NewResourceController(nil, nil)
	key := "ns/db-0/postgres"
	rc.crashLoopEpisode[key] = episode{firstSeen: at(0), escalatedAt: at(6), readySince: at(10)}

	rc.holdEpisode(key, at(11))

	ep := rc.crashLoopEpisode[key]
	assert.True(t, ep.readySince.IsZero())
	assert.Equal(t, at(11), ep.lastSeen, "an episode being observed must not age out of the sweep")
}

// Holding an episode must not conjure one. A container that has never crash
// looped and is merely pulling an image is not an incident.
func TestHoldEpisodeDoesNotInventOne(t *testing.T) {
	rc := NewResourceController(nil, nil)

	rc.holdEpisode("ns/web-0/nginx", at(3))

	assert.Empty(t, rc.crashLoopEpisode, "a container with no episode has nothing to hold")
}

// The pod scan covers every Kipper-managed workload, so the escalation must not
// prescribe `kip service restart` for an application pod: there is no Service CR
// behind it, and a stateful service that happens to share the name would be the
// one restarted.
func TestRecoveryCommandMatchesTheWorkload(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name:   "a service pod gets the command that recreates its pod",
			labels: map[string]string{"app": "db", "kipper.run/service-type": "postgres"},
			want:   "kip service restart db",
		},
		{
			name:   "an application pod gets the app command",
			labels: map[string]string{"app": "api"},
			want:   "kip app restart api",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, recoveryCommand(tc.labels))
		})
	}
}

// A pod with no app label names no workload, so there is no command to give and
// inventing one would send the operator somewhere wrong.
func TestRecoveryCommandSaysNothingWhenItCannotName(t *testing.T) {
	assert.Empty(t, recoveryCommand(map[string]string{"kipper.run/service-type": "postgres"}))
}

// An episode has to survive the recovery it prescribes. A StatefulSet recreates
// its pod under the same name, but a Deployment does not: `kip app restart api`
// replaces api-6d8f with api-7b9c, and an episode keyed by pod name would never
// see the healthy replacement. The critical alert would be followed by silence
// rather than an all-clear, and the old episode would age out unnoticed.
//
// The key is the workload, which both kinds carry as their app label.
func TestEpisodeKeyIsStableAcrossAReplacement(t *testing.T) {
	before := episodeKey("shop-prod", "api-6d8f4c", "api", "app", mainContainer)
	after := episodeKey("shop-prod", "api-7b9c1a", "api", "app", mainContainer)

	assert.Equal(t, before, after,
		"a Deployment's replacement pod has a new name; the episode must still be the same episode")
}

// Two workloads must not share an episode, or one recovering would announce the
// other's all-clear.
func TestEpisodeKeySeparatesWorkloads(t *testing.T) {
	assert.NotEqual(t,
		episodeKey("shop-prod", "api-1", "api", "app", mainContainer),
		episodeKey("shop-prod", "web-1", "web", "app", mainContainer))
	assert.NotEqual(t,
		episodeKey("shop-prod", "api-1", "api", "app", mainContainer),
		episodeKey("shop-test", "api-1", "api", "app", mainContainer),
		"the same workload in two environments is two episodes")
	assert.NotEqual(t,
		episodeKey("shop-prod", "api-1", "api", "app", mainContainer),
		episodeKey("shop-prod", "api-1", "api", "sidecar", mainContainer),
		"two containers in one pod fail independently")
}

// A pod with no app label has no workload identity to key on, so it falls back
// to its own name. Two such pods must not collide.
func TestEpisodeKeyFallsBackToThePodName(t *testing.T) {
	assert.NotEqual(t,
		episodeKey("shop-prod", "loose-pod-1", "", "app", mainContainer),
		episodeKey("shop-prod", "loose-pod-2", "", "app", mainContainer))
}

// An episode belongs to a workload, and a Deployment has several pods. A
// replica running clean beside one that is still crash-looping must not start
// the ten-minute recovery run, or the workload announces an all-clear while it
// is still failing.
//
// This runs through the real scan rather than the pieces, because the bug it
// covers is in how the two passes see each other.
func TestOneHealthyReplicaDoesNotRecoverAWorkloadStillFailing(t *testing.T) {
	failing := crashLoopingReplica("shop-prod", "api", "api-failing")
	healthy := healthyReplica("shop-prod", "api", "api-healthy")
	rc := NewResourceController(fake.NewClientset(failing, healthy), nil)
	rc.readPreviousLog = func(context.Context, string, string, string) string { return "" }

	key := episodeKey("shop-prod", "api-failing", "api", "app", mainContainer)
	rc.crashLoopEpisode[key] = episode{
		firstSeen:   time.Now().Add(-7 * time.Hour),
		escalatedAt: time.Now().Add(-time.Hour),
	}
	// Inside the hourly floor, which is where the scan spends fifty-nine ticks
	// out of sixty. The failing replica raises no alert on this tick, so nothing
	// it does can overwrite what the healthy one wrote.
	rc.crashLoopAlerted[key] = time.Now()

	rc.commitBatches(rc.checkPodProblems(context.Background()))

	ep, tracked := rc.crashLoopEpisode[key]
	require.True(t, tracked, "the episode closed while a replica was still crash-looping")
	assert.True(t, ep.readySince.IsZero(),
		"a recovery run started while a replica of the same workload is failing")
}

// Once every replica is healthy the recovery run does start, or a workload that
// really has recovered would never close.
func TestEveryReplicaHealthyStartsTheRecoveryRun(t *testing.T) {
	rc := NewResourceController(fake.NewClientset(
		healthyReplica("shop-prod", "api", "api-1"),
		healthyReplica("shop-prod", "api", "api-2"),
	), nil)

	key := episodeKey("shop-prod", "api-1", "api", "app", mainContainer)
	rc.crashLoopEpisode[key] = episode{
		firstSeen:   time.Now().Add(-7 * time.Hour),
		escalatedAt: time.Now().Add(-time.Hour),
	}

	rc.commitBatches(rc.checkPodProblems(context.Background()))

	ep := rc.crashLoopEpisode[key]
	assert.False(t, ep.readySince.IsZero(), "every replica is clean, so the recovery run has begun")
}

func crashLoopingReplica(namespace, app, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": app, "app.kubernetes.io/managed-by": "kipper"},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:         "app",
			RestartCount: 20,
			State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}
}

func healthyReplica(namespace, app, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": app, "app.kubernetes.io/managed-by": "kipper"},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "app",
			Ready: true,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
}

// A crash loop is usually the image or the configuration, and recreating the
// pod brings back the same image and the same configuration. Presenting that as
// the recovery sends an operator to run a command that cannot work and then
// wonder what else is wrong.
//
// The read-only case is where a recreation genuinely is the remedy, and that one
// has its own alert saying so.
func TestAGenericCrashLoopDoesNotPromiseARecovery(t *testing.T) {
	rc := NewResourceController(nil, nil)
	rc.readPreviousLog = func(context.Context, string, string, string) string {
		return `FATAL: password authentication failed for user "app"`
	}

	pod := crashLoopingPodFor("shop-test", "db", "postgres")
	obs := soleObservation(pod)
	rc.crashLoopEpisode[obs.key] = episode{firstSeen: at(0), lastAlerted: at(5)}

	batch, ok := rc.crashLoopAlert(context.Background(), rc.evidenceBudget(), obs.key, obs, at(6), "2026-09-06T00:00:00Z")
	require.True(t, ok)
	require.Equal(t, stageCritical, batch.entry.Severity)

	assert.NotContains(t, batch.entry.Reason, "Recover with",
		"nothing here has been established that a pod recreation fixes")
	assert.Contains(t, batch.entry.Reason, "kip service restart db",
		"the command is still worth naming, as the thing to try if it looks like storage")
}
