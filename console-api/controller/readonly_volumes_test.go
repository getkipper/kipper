package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// A log line says a filesystem stopped accepting writes. It does not say which,
// and no parser can settle it: a pathname cannot establish its backing
// filesystem without resolving the container's mount namespace and its symlinks,
// and Postgres puts /var/lib/postgresql/data/pg_wal on a different volume from
// the path it is written as often enough for that to matter.
//
// Four review rounds went into attributing the failure from the line, and the
// attempt was wrong in both directions: naming a healthy volume with confidence,
// and suppressing a real incident because a path looked like it pointed
// somewhere else. So the alert reports what it knows.

func containerWith(volumes []corev1.Volume, mounts []corev1.VolumeMount) *corev1.Pod {
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			Volumes:    volumes,
			Containers: []corev1.Container{{Name: "postgres", VolumeMounts: mounts}},
		},
	}
}

func claimVolume(name, claim string, readOnly bool) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: claim, ReadOnly: readOnly,
		},
	}}
}

func TestWritableClaims(t *testing.T) {
	tests := []struct {
		name    string
		volumes []corev1.Volume
		mounts  []corev1.VolumeMount
		want    []string
	}{
		{
			name:    "one volume",
			volumes: []corev1.Volume{claimVolume("data", "data-db-0", false)},
			mounts:  []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/postgresql"}},
			want:    []string{"data-db-0"},
		},
		{
			name: "two volumes, named in a stable order",
			volumes: []corev1.Volume{
				claimVolume("wal", "wal-db-0", false),
				claimVolume("data", "data-db-0", false),
			},
			mounts: []corev1.VolumeMount{
				{Name: "wal", MountPath: "/var/lib/wal"},
				{Name: "data", MountPath: "/var/lib/postgresql"},
			},
			want: []string{"data-db-0", "wal-db-0"},
		},
		{
			name:    "a mount asked for read-only did not remount; it was mounted that way",
			volumes: []corev1.Volume{claimVolume("config", "config-db-0", false)},
			mounts:  []corev1.VolumeMount{{Name: "config", MountPath: "/etc/postgresql", ReadOnly: true}},
			want:    nil,
		},
		{
			name:    "and neither did a claim declared read-only",
			volumes: []corev1.Volume{claimVolume("config", "config-db-0", true)},
			mounts:  []corev1.VolumeMount{{Name: "config", MountPath: "/etc/postgresql"}},
			want:    nil,
		},
		{
			name: "a ConfigMap is not a volume this can help with",
			volumes: []corev1.Volume{{
				Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}},
			}},
			mounts: []corev1.VolumeMount{{Name: "config", MountPath: "/etc/postgresql"}},
			want:   nil,
		},
		{
			name:    "nor is an emptyDir",
			volumes: []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			mounts:  []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}},
			want:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, writableClaims(containerWith(tc.volumes, tc.mounts), "postgres", mainContainer))
		})
	}
}

// An init container and a main container can share a name, and their mounts must
// not run together.
func TestWritableClaimsAreScopedToTheContainerKind(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Volumes: []corev1.Volume{
			claimVolume("init-data", "scratch", false),
			claimVolume("app-data", "data", false),
		},
		InitContainers: []corev1.Container{{
			Name: "db", VolumeMounts: []corev1.VolumeMount{{Name: "init-data", MountPath: "/init"}},
		}},
		Containers: []corev1.Container{{
			Name: "db", VolumeMounts: []corev1.VolumeMount{{Name: "app-data", MountPath: "/data"}},
		}},
	}}

	assert.Equal(t, []string{"data"}, writableClaims(pod, "db", mainContainer))
	assert.Equal(t, []string{"scratch"}, writableClaims(pod, "db", initContainer))
}

// The message says what the container mounts, leaves the attribution alone, and
// keeps the command inside the condition rather than after it.
func TestDescribeClaims(t *testing.T) {
	one := describeClaims([]string{"data-db-0"}, "kip service restart db")
	assert.Contains(t, one, "volume data-db-0")
	assert.Contains(t, one, "If that is the one")
	assert.Contains(t, one, "kip service restart db is how")
	assert.NotContains(t, one, "points at",
		"nothing here establishes which filesystem failed")

	both := describeClaims([]string{"data-db-0", "wal-db-0"}, "kip service restart db")
	assert.Contains(t, both, "data-db-0 and wal-db-0")

	// A workload this cannot name a command for still gets the diagnosis.
	assert.Contains(t, describeClaims([]string{"data-db-0"}, ""), "read-only filesystem")
}

// A container mounting a writable volume and a read-only ConfigMap logs the same
// message either way. That the two coexist does not connect the error to the
// volume, so the alert states the fact it has and offers the recovery as a
// conditional rather than asserting a remount.
func TestDescribeClaimsDoesNotAssertARemount(t *testing.T) {
	for _, claims := range [][]string{{"data-db-0"}, {"data-db-0", "wal-db-0"}} {
		msg := describeClaims(claims, "kip service restart db")
		assert.Contains(t, msg, "read-only filesystem",
			"the fact is that it hit one; which one is not established")
		assert.NotContains(t, msg, "stopped accepting writes",
			"that phrasing asserts the volume or the node broke")
		assert.Contains(t, msg, "If", "the recovery is offered on a condition, not promised")
	}
}

// The evidence establishes that the container hit a read-only filesystem. It
// does not establish that a persistent volume remounted, so the alert must not
// be titled as though it had, and must not end with an imperative that
// contradicts the conditional sentence before it.
//
// A container with a writable volume and a read-only ConfigMap that fails on
// the ConfigMap gets this alert. Recreating its pod interrupts the service and
// brings back the same configuration.
func TestTheAlertNamesWhatTheEvidenceEstablishes(t *testing.T) {
	rc := NewResourceController(nil, nil)
	rc.readPreviousLog = func(context.Context, string, string, string) string {
		return `FATAL:  could not remove old lock file "postmaster.pid": Read-only file system`
	}

	pod := crashLoopingPodFor("shop-test", "db", "postgres")
	obs := soleObservation(pod)
	batch, ok := rc.crashLoopAlert(context.Background(), rc.evidenceBudget(), obs.key, obs, at(0), "2026-09-06T00:00:00Z")
	require.True(t, ok)

	assert.Equal(t, "ReadOnlyFilesystem", batch.entry.Action,
		"a read-only filesystem is what the log says; which filesystem is not established")

	assert.NotContains(t, batch.entry.Reason, "Recover with",
		"an imperative contradicts the conditional sentence it follows")
	assert.Contains(t, batch.entry.Reason, "kip service restart db",
		"the command is still named, as the thing to run if the volume is the one")
	assert.Contains(t, batch.entry.Reason, "If that is the one",
		"and it stays a condition all the way to the end")
}

// The evidence reads are API calls made while the controller holds its state
// lock. One is bounded at five seconds; a broad outage means one per failing
// workload, and the first tick after a console-api restart could hold the lock
// for as long as that takes. Nothing contends for it today — it is only ever
// taken from the tick — but a locked reader added off the tick path later would
// find a lock held for minutes.
func TestEvidenceReadingIsBoundedForTheWholeTick(t *testing.T) {
	restore := evidenceBudgetPerTick
	evidenceBudgetPerTick = 200 * time.Millisecond
	defer func() { evidenceBudgetPerTick = restore }()

	rc := NewResourceController(nil, nil)

	var reads int
	rc.readPreviousLog = func(context.Context, string, string, string) string {
		reads++
		time.Sleep(50 * time.Millisecond) // a slow API server
		return ""
	}

	pod := crashLoopingPodFor("shop-test", "db", "postgres")
	obs := soleObservation(pod)

	start := time.Now()
	budget := rc.evidenceBudget()
	for i := 0; i < 50; i++ {
		rc.readOnlyVolumeEvidence(context.Background(), budget, obs)
	}

	assert.Less(t, time.Since(start), evidenceBudgetPerTick*3,
		"fifty failing workloads spent %v inside the lock", time.Since(start))
	assert.Less(t, reads, 50, "the budget should have stopped it reading every one")
	assert.Greater(t, reads, 0, "and it should have read some")
}

// A tick with a few failing workloads reads all of them, because the budget is
// a ceiling rather than a quota.
func TestEvidenceReadingIsNotRationedOnAQuietCluster(t *testing.T) {
	rc := NewResourceController(nil, nil)

	var reads int
	rc.readPreviousLog = func(context.Context, string, string, string) string {
		reads++
		return "nothing interesting"
	}

	pod := crashLoopingPodFor("shop-test", "db", "postgres")
	obs := soleObservation(pod)

	budget := rc.evidenceBudget()
	for i := 0; i < 3; i++ {
		rc.readOnlyVolumeEvidence(context.Background(), budget, obs)
	}

	assert.Equal(t, 3, reads, "a handful of failing workloads all get read")
}

// A crash loop that has already escalated repeats daily. If the volume goes
// read-only underneath it after that — the disk fails while the database was
// already failing for another reason — the read-only diagnosis would wait for
// the next daily slot, up to 24 hours.
//
// That is the incident this feature exists for, arriving a day late. The
// read-only alert has its own cadence, because it is the recoverable one.
func TestReadOnlyEvidenceIsNotHeldBackByTheDailyCadence(t *testing.T) {
	rc := NewResourceController(nil, nil)
	rc.readPreviousLog = func(context.Context, string, string, string) string {
		return `FATAL:  could not remove old lock file "postmaster.pid": Read-only file system`
	}

	pod := crashLoopingPodFor("shop-test", "db", "postgres")
	obs := soleObservation(pod)

	// Escalated an hour ago and not due to say anything again until tomorrow.
	rc.crashLoopEpisode[obs.key] = episode{
		firstSeen:   at(0),
		lastAlerted: at(7),
		escalatedAt: at(6),
	}

	batch, ok := rc.crashLoopAlert(context.Background(), rc.evidenceBudget(), obs.key, obs, at(8), "2026-09-06T00:00:00Z")

	require.True(t, ok, "the disk failing is news whatever the crash loop was already saying")
	assert.Equal(t, "ReadOnlyFilesystem", batch.entry.Action)
}

// And it says it once, not every tick for the rest of the incident.
func TestTheReadOnlyDiagnosisIsNotRepeatedEveryTick(t *testing.T) {
	rc := NewResourceController(nil, nil)
	rc.readPreviousLog = func(context.Context, string, string, string) string {
		return "cannot write /var/lib/data/x: Read-only file system"
	}

	pod := crashLoopingPodFor("shop-test", "db", "postgres")
	obs := soleObservation(pod)
	rc.crashLoopEpisode[obs.key] = episode{firstSeen: at(0), lastAlerted: at(7), escalatedAt: at(6)}

	first, ok := rc.crashLoopAlert(context.Background(), rc.evidenceBudget(), obs.key, obs, at(8), "t1")
	require.True(t, ok)
	rc.commitBatches([]alertBatch{first})

	_, again := rc.crashLoopAlert(context.Background(), rc.evidenceBudget(), obs.key, obs, at(8).Add(time.Minute), "t2")
	assert.False(t, again, "the same disk failure a minute later is not new")
}
