package installer

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

var gateNow = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

// gatePod is db's pod mounting claim, Ready since readySince. A zero
// readySince leaves the pod Running but not Ready.
func gatePod(claim string, readySince time.Time) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db-0"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{
			{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}},
		}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	ready := corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse}
	if !readySince.IsZero() {
		ready = corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(readySince)}
	}
	pod.Status.Conditions = []corev1.PodCondition{ready}
	return pod
}

func gateBackup(name, phase string, started, expires time.Time) VeleroBackup {
	b := VeleroBackup{Metadata: metav1.ObjectMeta{Name: name, Namespace: "velero", UID: types.UID("uid-" + name)}}
	b.Status.Phase = phase
	if !started.IsZero() {
		b.Status.StartTimestamp = &metav1.Time{Time: started}
	}
	if !expires.IsZero() {
		b.Status.Expiration = &metav1.Time{Time: expires}
	}
	return b
}

func gatePVB(backup, pod, volume, pvcUID, phase string) VeleroPodVolumeBackup {
	p := VeleroPodVolumeBackup{Metadata: metav1.ObjectMeta{Name: backup + "-" + volume, Namespace: "velero", Labels: map[string]string{
		"velero.io/backup-name": backup,
		"velero.io/backup-uid":  "uid-" + backup,
	}}}
	if pvcUID != "" {
		p.Metadata.Labels["velero.io/pvc-uid"] = pvcUID
	}
	p.Spec.Pod = corev1.ObjectReference{Kind: "Pod", Namespace: "shop", Name: pod}
	p.Spec.Volume = volume
	p.Status.Phase = phase
	return p
}

// gateFacts is db in shop, moved movedAgo before gateNow onto claim c1, with
// its pod Ready for readyFor. A zero readyFor means the pod is not Ready.
func gateFacts(movedAgo, readyFor time.Duration) RetainedCopyFacts {
	class := "longhorn-single"
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "data-db-0", UID: types.UID("c1")},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &class},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	var since time.Time
	if readyFor > 0 {
		since = gateNow.Add(-readyFor)
	}
	return RetainedCopyFacts{
		Namespace:    "shop",
		Service:      "db",
		Destination:  "c1",
		MovedAt:      gateNow.Add(-movedAgo),
		Claim:        claim,
		ServiceState: &RetainedService{},
		Pod:          gatePod("data-db-0", since),
	}
}

// withBackup adds backup b, started startedAgo before gateNow and expiring in
// a week, with a completed data PodVolumeBackup of db-0 bound to c1.
func withBackup(f RetainedCopyFacts, startedAgo time.Duration) RetainedCopyFacts {
	f.Backups = append(f.Backups, gateBackup("b", "Completed", gateNow.Add(-startedAgo), gateNow.Add(7*24*time.Hour)))
	f.PodVolumeBackups = append(f.PodVolumeBackups, gatePVB("b", "db-0", "data", "c1", "Completed"))
	return f
}

func TestRetainedCopyHealthGate(t *testing.T) {
	noPod := gateFacts(25*time.Hour, 0)
	noPod.Pod = nil
	otherClaim := gateFacts(25*time.Hour, 2*time.Hour)
	otherClaim.Pod = gatePod("data-other-0", gateNow.Add(-2*time.Hour))
	pending := gateFacts(25*time.Hour, 2*time.Hour)
	pending.Pod.Status.Phase = corev1.PodPending

	for name, tc := range map[string]struct {
		facts RetainedCopyFacts
		note  string
	}{
		"never ran":           {noPod, "kept: db has not run on Longhorn yet"},
		"never Ready":         {gateFacts(25*time.Hour, 0), "kept: db is not Ready on Longhorn"},
		"not running":         {pending, "kept: db is not Ready on Longhorn"},
		"Ready 30 min":        {gateFacts(25*time.Hour, 30*time.Minute), "kept: db has not been Ready on Longhorn for an hour yet"},
		"another claim":       {otherClaim, "kept: db's pod does not use the Longhorn volume the move verified"},
		"no backup needed":    {withBackup(gateFacts(25*time.Hour, 30*time.Minute), time.Hour), "kept: db has not been Ready on Longhorn for an hour yet"},
		"--now is not enough": {gateFacts(25*time.Hour, 0), "kept: db is not Ready on Longhorn"},
	} {
		t.Run(name, func(t *testing.T) {
			d := DecideRetainedCopy(tc.facts, gateNow, PruneWaivers{SkipSoak: name == "--now is not enough"})
			assert.False(t, d.Prunable, "the health gate is never skipped")
			assert.True(t, d.Unproven)
			assert.Equal(t, tc.note, d.Note)
		})
	}
}

func TestRetainedCopyNeedsABackupOrADay(t *testing.T) {
	d := DecideRetainedCopy(gateFacts(3*time.Hour, 2*time.Hour), gateNow, PruneWaivers{})
	assert.False(t, d.Prunable, "Ready for 2h, moved 3h ago and no backup keeps the copy")
	assert.False(t, d.Unproven)
	assert.Equal(t, "kept until a backup of db completes or 21h from now", d.Note)

	d = DecideRetainedCopy(gateFacts(23*time.Hour+50*time.Minute, 2*time.Hour), gateNow, PruneWaivers{})
	assert.Equal(t, "kept until a backup of db completes or 10m from now", d.Note)

	d = DecideRetainedCopy(gateFacts(25*time.Hour, 2*time.Hour), gateNow, PruneWaivers{})
	assert.True(t, d.Prunable, "a day after the move a healthy service's copy can go")
	assert.False(t, d.FallbackWaived)

	d = DecideRetainedCopy(withBackup(gateFacts(3*time.Hour, 2*time.Hour), 30*time.Minute), gateNow, PruneWaivers{})
	assert.True(t, d.Prunable, "a backup after the move releases the copy before the day is over")
	assert.Contains(t, d.Note, "backup b")
}

func TestRetainedCopyWithoutAMoveTimeIsKept(t *testing.T) {
	f := gateFacts(25*time.Hour, 2*time.Hour)
	f.MovedAt = time.Time{}
	f = withBackup(f, 30*time.Minute)
	d := DecideRetainedCopy(f, gateNow, PruneWaivers{})
	assert.False(t, d.Prunable, "neither a backup nor the day can be measured without the move's time")
	assert.Contains(t, d.Note, "recorded no time")

	d = DecideRetainedCopy(f, gateNow, PruneWaivers{SkipSoak: true})
	assert.True(t, d.Prunable, "--now skips the part that needs the move's time")
}

func TestRetainedCopyBackupEvidence(t *testing.T) {
	moved := 3 * time.Hour
	base := func() RetainedCopyFacts { return gateFacts(moved, 2*time.Hour) }
	before := withBackup(base(), 4*time.Hour)
	unbound := base()
	unbound.Backups = []VeleroBackup{gateBackup("b", "Completed", gateNow.Add(-time.Hour), gateNow.Add(time.Hour))}
	unbound.PodVolumeBackups = []VeleroPodVolumeBackup{gatePVB("b", "db-0", "data", "c0", "Completed")}
	unlabeled := base()
	unlabeled.Backups = unbound.Backups
	unlabeled.PodVolumeBackups = []VeleroPodVolumeBackup{gatePVB("b", "db-0", "data", "", "Completed")}
	otherPod := base()
	otherPod.Backups = unbound.Backups
	otherPod.PodVolumeBackups = []VeleroPodVolumeBackup{gatePVB("b", "web-0", "data", "c1", "Completed")}
	otherVolume := base()
	otherVolume.Backups = unbound.Backups
	otherVolume.PodVolumeBackups = []VeleroPodVolumeBackup{gatePVB("b", "db-0", "cache", "c1", "Completed")}
	failedPVB := base()
	failedPVB.Backups = unbound.Backups
	failedPVB.PodVolumeBackups = []VeleroPodVolumeBackup{gatePVB("b", "db-0", "data", "c1", "Failed")}
	otherBackup := base()
	otherBackup.Backups = unbound.Backups
	otherBackup.PodVolumeBackups = []VeleroPodVolumeBackup{gatePVB("a", "db-0", "data", "c1", "Completed")}
	earlierBackup := withBackup(base(), time.Hour)
	earlierBackup.PodVolumeBackups[0].Metadata.Labels["velero.io/backup-uid"] = "uid-b-deleted"
	noBackupUID := withBackup(base(), time.Hour)
	delete(noBackupUID.PodVolumeBackups[0].Metadata.Labels, "velero.io/backup-uid")
	noUIDs := withBackup(base(), time.Hour)
	noUIDs.Backups[0].Metadata.UID = ""
	delete(noUIDs.PodVolumeBackups[0].Metadata.Labels, "velero.io/backup-uid")
	partial := withBackup(base(), time.Hour)
	partial.Backups[0].Status.Phase = "PartiallyFailed"
	expired := withBackup(base(), time.Hour)
	expired.Backups[0].Status.Expiration = &metav1.Time{Time: gateNow.Add(-time.Minute)}
	noExpiry := withBackup(base(), time.Hour)
	noExpiry.Backups[0].Status.Expiration = nil
	noStart := withBackup(base(), time.Hour)
	noStart.Backups[0].Status.StartTimestamp = nil
	deleting := withBackup(base(), time.Hour)
	deleting.Backups[0].Metadata.DeletionTimestamp = &metav1.Time{Time: gateNow}
	phaseDeleting := withBackup(base(), time.Hour)
	phaseDeleting.Backups[0].Status.Phase = "Deleting"

	for name, f := range map[string]RetainedCopyFacts{
		"started before the move":           before,
		"bound to another claim":            unbound,
		"without a claim label":             unlabeled,
		"for another pod":                   otherPod,
		"for another volume":                otherVolume,
		"pod volume backup failed":          failedPVB,
		"pod volume backup of other backup": otherBackup,
		"pod volume backup of an earlier b": earlierBackup,
		"pod volume backup without its UID": noBackupUID,
		"backup without a UID":              noUIDs,
		"partially failed":                  partial,
		"expiring":                          expired,
		"no expiration":                     noExpiry,
		"no start time":                     noStart,
		"being deleted":                     deleting,
		"phase Deleting":                    phaseDeleting,
	} {
		t.Run(name, func(t *testing.T) {
			d := DecideRetainedCopy(f, gateNow, PruneWaivers{})
			assert.False(t, d.Prunable, "a backup that does not prove it holds the data from Longhorn keeps the copy")
			assert.Equal(t, "kept until a backup of db completes or 21h from now", d.Note)
		})
	}
}

// A backup's hook counts cover every pod in it, and the dump's own manifest is
// only inside the backup, so no backup proves that this service's dump ran.
func TestRetainedCopyOfAConsistentPostgresWaitsForTheDay(t *testing.T) {
	consistent := func(movedAgo time.Duration) RetainedCopyFacts {
		f := withBackup(gateFacts(movedAgo, 2*time.Hour), time.Hour)
		f.ServiceState.ConsistentBackups = true
		f.PodVolumeBackups = append(f.PodVolumeBackups, gatePVB("b", "db-0", "kipper-dump", "d1", "Completed"))
		return f
	}

	d := DecideRetainedCopy(consistent(3*time.Hour), gateNow, PruneWaivers{})
	assert.False(t, d.Prunable, "another pod's succeeded hook and a backed-up dump volume do not prove this service's dump ran")
	assert.Equal(t, "kept for 21h more: db takes consistent backups, and Velero records hook results per backup, not per pod", d.Note)

	d = DecideRetainedCopy(consistent(25*time.Hour), gateNow, PruneWaivers{})
	assert.True(t, d.Prunable, "a day after the move the copy goes")

	d = DecideRetainedCopy(consistent(3*time.Hour), gateNow, PruneWaivers{SkipSoak: true})
	assert.True(t, d.Prunable, "--now gives up the fallback")
	assert.True(t, d.FallbackWaived)
	assert.True(t, d.ConsistentBackups)
	assert.Equal(t, "db takes consistent backups, which the prune check cannot use as proof that its data was backed up. Using --now skips the day's wait", d.Note,
		"a backup that holds the data is no reason the copy was kept")
}

func TestRetainedCopyNowSkipsOnlyTheWait(t *testing.T) {
	d := DecideRetainedCopy(gateFacts(3*time.Hour, 2*time.Hour), gateNow, PruneWaivers{SkipSoak: true})
	assert.True(t, d.Prunable, "--now gives up the fallback for a healthy service")
	assert.True(t, d.FallbackWaived)
	assert.False(t, d.Unproven)

	d = DecideRetainedCopy(gateFacts(3*time.Hour, 30*time.Minute), gateNow, PruneWaivers{SkipSoak: true})
	assert.False(t, d.Prunable, "--now never skips the health gate")

	d = DecideRetainedCopy(withBackup(gateFacts(3*time.Hour, 2*time.Hour), time.Hour), gateNow, PruneWaivers{SkipSoak: true})
	assert.True(t, d.Prunable)
	assert.False(t, d.FallbackWaived, "a copy a backup already releases gives up nothing")
}

func TestRetainedCopyDeleteUnprovenWaivesOnlyTheHealthGate(t *testing.T) {
	d := DecideRetainedCopy(gateFacts(25*time.Hour, 0), gateNow, PruneWaivers{DeleteUnproven: true})
	assert.True(t, d.Prunable)
	assert.True(t, d.Unproven)

	d = DecideRetainedCopy(gateFacts(3*time.Hour, 0), gateNow, PruneWaivers{DeleteUnproven: true})
	assert.False(t, d.Prunable, "the backup-or-a-day part still applies")
	assert.True(t, d.Unproven)
	assert.Contains(t, d.Note, "kept until a backup of db completes")

	d = DecideRetainedCopy(gateFacts(3*time.Hour, 0), gateNow, PruneWaivers{DeleteUnproven: true, SkipSoak: true})
	assert.True(t, d.Prunable)
	assert.True(t, d.Unproven)
	assert.True(t, d.FallbackWaived)

	replaced := gateFacts(25*time.Hour, 0)
	replaced.Destination = "c0"
	d = DecideRetainedCopy(replaced, gateNow, PruneWaivers{DeleteUnproven: true, SkipSoak: true})
	assert.False(t, d.Prunable, "the waivers never override the checks on the data volume")
	assert.Contains(t, d.Note, "replaced")
}

func TestRetainedCopyOfADeletedServiceKeepsTheOldRule(t *testing.T) {
	f := gateFacts(time.Minute, 0)
	f.Claim, f.ServiceState, f.Pod = nil, nil, nil
	d := DecideRetainedCopy(f, gateNow, PruneWaivers{})
	assert.True(t, d.Prunable, "a copy whose service and claim are both gone needs no health or wait")
	assert.True(t, d.Gone)
	assert.False(t, d.Unproven)
	assert.Equal(t, "its service was deleted", d.Note)

	f.ServiceState = &RetainedService{}
	d = DecideRetainedCopy(f, gateNow, PruneWaivers{})
	assert.False(t, d.Prunable, "a service without its data volume keeps the copy")
	assert.Contains(t, d.Note, "only one left")
}

// gateRoutes serves db in shop over kubectl: moved 3h before retainedNow onto
// claim c1, with the pod's Ready condition given and the Velero lists given.
func gateRoutes(ready, backups, pvbs string) []route {
	moved := strings.Replace(retainedCopyJSON("pvc-a", "shop/db", platform.RetainedLocalCopyMoved, 1, "c1"),
		retainedNow.Add(-25*time.Hour).Format(time.RFC3339), retainedNow.Add(-3*time.Hour).Format(time.RFC3339), 1)
	return []route{
		{contains: "get pv -l", out: `{"items":[` + moved + `]}`},
		{contains: "get pv 'pvc-a'", out: moved},
		{contains: "get pvc 'data-db-0'", out: `{"metadata":{"name":"data-db-0","uid":"c1"},"spec":{"storageClassName":"longhorn-single"},"status":{"phase":"Bound"}}`},
		{contains: "get services.kipper.run 'db'", out: `{"metadata":{"uid":"s2"},"spec":{}}`},
		{contains: "get pod 'db-0'", out: `{"metadata":{"name":"db-0"},"spec":{"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"data-db-0"}}]},"status":{"phase":"Running","conditions":[` + ready + `]}}`},
		{contains: "get backups.velero.io", out: backups},
		{contains: "get podvolumebackups.velero.io", out: pvbs},
	}
}

const gateReady2h = `{"type":"Ready","status":"True","lastTransitionTime":"2026-10-25T10:00:00Z"}`

func TestRetainedCopiesReadTheGatesOverKubectl(t *testing.T) {
	backups := `{"items":[{"metadata":{"name":"daily-1","namespace":"velero","uid":"u1"},"status":{"phase":"Completed","startTimestamp":"2026-10-25T11:00:00Z","expiration":"2026-11-24T11:00:00Z"}}]}`
	pvbs := `{"items":[{"metadata":{"name":"daily-1-x","namespace":"velero","labels":{"velero.io/backup-name":"daily-1","velero.io/backup-uid":"u1","velero.io/pvc-uid":"c1"}},"spec":{"pod":{"kind":"Pod","namespace":"shop","name":"db-0"},"volume":"data"},"status":{"phase":"Completed"}}]}`
	host := &routedRunner{routes: gateRoutes(gateReady2h, backups, pvbs)}
	got, err := RetainedCopies(host, retainedNow, PruneWaivers{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, got[0].Prunable, "a backup read from Velero releases the copy")
	assert.Contains(t, got[0].Note, "backup daily-1")

	noVelero := `error: the server doesn't have a resource type "backups"`
	host = &routedRunner{routes: gateRoutes(gateReady2h, noVelero, noVelero)}
	host.routes[5].err, host.routes[6].err = errors.New("exit 1"), errors.New("exit 1")
	got, err = RetainedCopies(host, retainedNow, PruneWaivers{})
	require.NoError(t, err, "a cluster without Velero has no backup evidence, which is no error")
	assert.False(t, got[0].Prunable)
	assert.Equal(t, "kept until a backup of db completes or 21h from now", got[0].Note)

	hooked := `{"items":[{"metadata":{"name":"daily-1","namespace":"velero","uid":"u1"},"status":{"phase":"Completed","startTimestamp":"2026-10-25T11:00:00Z","expiration":"2026-11-24T11:00:00Z","hookStatus":{"hooksAttempted":1}}}]}`
	withDump := strings.Replace(pvbs, `]}`, `,{"metadata":{"name":"daily-1-y","namespace":"velero","labels":{"velero.io/backup-name":"daily-1","velero.io/backup-uid":"u1","velero.io/pvc-uid":"d1"}},"spec":{"pod":{"kind":"Pod","namespace":"shop","name":"db-0"},"volume":"kipper-dump"},"status":{"phase":"Completed"}}]}`, 1)
	host = &routedRunner{routes: gateRoutes(gateReady2h, hooked, withDump)}
	host.routes[3].out = `{"metadata":{"uid":"s2"},"spec":{"consistentBackups":true}}`
	got, err = RetainedCopies(host, retainedNow, PruneWaivers{})
	require.NoError(t, err)
	assert.False(t, got[0].Prunable, "a backup's hook count may come from another pod, so it proves nothing about this dump")
	assert.Contains(t, got[0].Note, "Velero records hook results per backup, not per pod")

	host = &routedRunner{routes: gateRoutes(gateReady2h, `{"items":[]}`, `{"items":[]}`)}
	got, err = RetainedCopies(host, retainedNow, PruneWaivers{SkipSoak: true})
	require.NoError(t, err)
	assert.True(t, got[0].Prunable)
	assert.True(t, got[0].FallbackWaived)
}

func TestPruneRetainedCopyRechecksTheGatesRightBeforeDeleting(t *testing.T) {
	notReady := `{"type":"Ready","status":"False","lastTransitionTime":"2026-10-25T11:50:00Z"}`
	host := &routedRunner{routes: gateRoutes(notReady, `{"items":[]}`, `{"items":[]}`)}
	_, err := PruneRetainedCopy(host, "pvc-a", retainedNow, PruneWaivers{SkipSoak: true}, 2, time.Millisecond, func(time.Duration) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db is not Ready on Longhorn")
	assert.Empty(t, host.stdins, "a copy whose service is not healthy is kept")

	host = &routedRunner{routes: gateRoutes(notReady, `{"items":[]}`, `{"items":[]}`)}
	_, err = PruneRetainedCopy(host, "pvc-a", retainedNow, PruneWaivers{DeleteUnproven: true}, 2, time.Millisecond, func(time.Duration) {})
	require.Error(t, err, "--delete-unproven alone does not skip the wait for a backup or a day")
	assert.Empty(t, host.stdins)

	host = &routedRunner{routes: gateRoutes(notReady, `{"items":[]}`, `{"items":[]}`)}
	_, err = PruneRetainedCopy(host, "pvc-a", retainedNow, PruneWaivers{DeleteUnproven: true, SkipSoak: true}, 2, time.Millisecond, func(time.Duration) {})
	require.NoError(t, err)
	require.Len(t, host.stdins, 1)
	assert.Contains(t, host.stdins[0], `"persistentVolumeReclaimPolicy":"Delete"`)
}

// The local copy remains a fallback during a destination copy change, even
// with both waivers. The claim's stop record persists across retries, and the
// service's StorageCopies reason also covers changes without a recorded stop.
func TestRetainedCopyWaitsForTheCopiesOfItsDestinationToSettle(t *testing.T) {
	all := PruneWaivers{SkipSoak: true, DeleteUnproven: true}
	f := withBackup(gateFacts(25*time.Hour, 2*time.Hour), time.Hour)
	f.Claim.Annotations = map[string]string{platform.CopyChangeStopAnnotation: `{"at":"2026-10-24T11:59:00Z"}`}
	d := DecideRetainedCopy(f, gateNow, all)
	assert.False(t, d.Prunable)
	assert.Equal(t, "kept: the copies of db's data volume are changing", d.Note)

	f = withBackup(gateFacts(25*time.Hour, 2*time.Hour), time.Hour)
	f.ServiceState.CopiesChange = "Converting"
	d = DecideRetainedCopy(f, gateNow, all)
	assert.False(t, d.Prunable)
	assert.Equal(t, "kept: the copies of db's data volume are changing (Converting)", d.Note)
}

func TestRetainedCopiesReadTheCopiesChangeOverKubectl(t *testing.T) {
	host := &routedRunner{routes: gateRoutes(gateReady2h, `{"items":[]}`, `{"items":[]}`)}
	host.routes[3].out = `{"metadata":{"uid":"s2"},"spec":{},"status":{"conditions":[{"type":"StorageCopies","status":"False","reason":"Blocked"}]}}`
	got, err := RetainedCopies(host, retainedNow, PruneWaivers{SkipSoak: true})
	require.NoError(t, err)
	assert.False(t, got[0].Prunable)
	assert.Contains(t, got[0].Note, "(Blocked)")
}
