package installer

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

const (
	moveNS      = "shop"
	moveSvc     = "db"
	moveUID     = "0123456789abcdef"
	moveOldPV   = "pvc-old"
	moveOldPath = "/var/lib/rancher/k3s/storage/pvc-old_shop_data-db-0"
)

type fakeStatefulSet struct {
	replicas, saved int32
	mark            string
	fence           bool
}

type fakePod struct {
	MovePod
	claims []string
}

// fakeMoveCluster is an in-memory cluster with just enough behavior for the
// move: claims bind, deleted claims release their volumes, StatefulSets start
// and stop pods, and the copy carries a volume's contents across.
type fakeMoveCluster struct {
	mode      string
	server    string
	paths     map[string]bool
	used      int64
	backups   MoveBackupState
	records   map[string]MoveRecord
	service   *MoveService
	sts       *fakeStatefulSet
	claims    map[string]*MoveClaim
	pvs       map[string]*MovePV
	contents  map[string]string
	nsClaims  []string
	nsVersion int
	// nsUnenforced is the namespace without its move label, so its claims
	// are listed but the policy does not apply.
	nsUnenforced bool
	nsLapsed     []string
	policy       bool
	pods         map[string]*fakePod
	copyOK       bool
	copies       int
	uid          int

	// loseCheckpoint drops the record write that would complete a step,
	// as if the connection died after the step's work but before its record.
	loseCheckpoint string
	// failCopyStart fails StartCopy, leaving everything before it in place.
	failCopyStart bool
	// strayAfterRelease makes the StatefulSet controller create a claim from
	// its template as soon as the old claim is gone.
	strayAfterRelease bool
	// stopAfter saves the record for this step, then fails, as if the
	// connection died right after the checkpoint.
	stopAfter string
	// ignoreRetain drops a patch to Retain, as if something set the policy
	// back right away.
	ignoreRetain bool
	// replaceOnRebind swaps the retained volume for another one under the
	// same name as soon as a claim is created on it.
	replaceOnRebind bool
	patches         []PVPatch
	// slowPVDelete keeps a deleted volume readable for a few more reads, as
	// the API server does while the volume's protection finalizer is removed.
	slowPVDelete bool
	goingPVs     map[string]int
	// unfenced lists the claim and copy operations made while admission did
	// not keep pods off the data claim.
	unfenced []string
	// copyResultFails fails that many copy result reads, as if the connection
	// dropped while the copy ran.
	copyResultFails int
	failedCopy      *CopyResult
	// usedAfterStop is what the volume holds once the service has stopped,
	// as if it wrote more between the question and the stop.
	usedAfterStop int64
}

func (f *fakeMoveCluster) noteFence(op string) {
	if !f.policy || f.nsUnenforced || !containsString(f.nsClaims, "data-db-0") {
		f.unfenced = append(f.unfenced, op)
	}
}

func newFakeMoveCluster() *fakeMoveCluster {
	f := &fakeMoveCluster{
		mode:     string(platform.StorageLonghorn),
		server:   "server-1",
		paths:    map[string]bool{moveOldPath: true},
		used:     3 << 30,
		backups:  MoveBackupState{Configured: true, LatestCompleted: time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)},
		records:  map[string]MoveRecord{},
		service:  &MoveService{UID: moveUID, Storage: "2Gi", ConsistentBackups: true},
		sts:      &fakeStatefulSet{replicas: 1},
		claims:   map[string]*MoveClaim{},
		pvs:      map[string]*MovePV{},
		contents: map[string]string{moveOldPV: "the data"},
		policy:   true,
		pods:     map[string]*fakePod{},
		copyOK:   true,
	}
	f.claims["data-db-0"] = &MoveClaim{
		Namespace: moveNS, Name: "data-db-0", UID: "claim-old", Class: platform.LocalPathClass, Phase: "Bound",
		VolumeName: moveOldPV, Size: "2Gi", Labels: map[string]string{"app": moveSvc},
	}
	f.pvs[moveOldPV] = &MovePV{
		Name: moveOldPV, UID: "pv-old", Class: platform.LocalPathClass, Phase: "Bound", Reclaim: "Delete",
		Capacity: "2Gi", Path: moveOldPath, Node: "server-1", ClaimNamespace: moveNS, ClaimName: "data-db-0", ClaimUID: "claim-old",
	}
	f.claims["db-dump"] = &MoveClaim{
		Namespace: moveNS, Name: "db-dump", UID: "dump-old", Class: platform.LocalPathClass, Phase: "Bound",
		VolumeName: "pvc-dump", Size: "2Gi", Labels: map[string]string{dumpForLabel: moveSvc},
	}
	f.pvs["pvc-dump"] = &MovePV{
		Name: "pvc-dump", UID: "pv-dump", Class: platform.LocalPathClass, Phase: "Bound", Reclaim: "Delete",
		Capacity: "2Gi", Path: "/var/lib/rancher/k3s/storage/pvc-dump", Node: "server-1", ClaimNamespace: moveNS, ClaimName: "db-dump", ClaimUID: "dump-old",
	}
	f.startPods()
	return f
}

func (f *fakeMoveCluster) nextUID(prefix string) string {
	f.uid++
	return fmt.Sprintf("%s-%d", prefix, f.uid)
}

func (f *fakeMoveCluster) startPods() {
	if f.sts.replicas > 0 {
		f.pods["db-0"] = &fakePod{MovePod: MovePod{Name: "db-0", StatefulSet: moveSvc}, claims: []string{"data-db-0", "db-dump"}}
	}
}

func (f *fakeMoveCluster) StorageMode() (string, error)         { return f.mode, nil }
func (f *fakeMoveCluster) ServerNode() (string, error)          { return f.server, nil }
func (f *fakeMoveCluster) PathExists(path string) (bool, error) { return f.paths[path], nil }
func (f *fakeMoveCluster) UsedBytes(string) (int64, error)      { return f.used, nil }
func (f *fakeMoveCluster) BackupState(string) (MoveBackupState, error) {
	return f.backups, nil
}

func (f *fakeMoveCluster) ReadRecord(name string) (*MoveRecord, error) {
	rec, ok := f.records[name]
	if !ok {
		return nil, nil
	}
	return &rec, nil
}

func (f *fakeMoveCluster) SaveRecord(name string, rec MoveRecord) error {
	if f.loseCheckpoint != "" && rec.Step == f.loseCheckpoint {
		f.loseCheckpoint = ""
		return errors.New("connection lost")
	}
	f.records[name] = rec
	if f.stopAfter != "" && rec.Step == f.stopAfter {
		f.stopAfter = ""
		return errors.New("connection lost")
	}
	return nil
}

func (f *fakeMoveCluster) DeleteRecord(name string) error {
	delete(f.records, name)
	return nil
}

func (f *fakeMoveCluster) ReadService(string, string) (*MoveService, error) {
	s := *f.service
	return &s, nil
}

func (f *fakeMoveCluster) MarkService(_, _, mark string) error {
	f.service.Mark = mark
	return nil
}

func (f *fakeMoveCluster) SetServiceStorage(_, _, size string) error {
	f.service.Storage = size
	return nil
}

func (f *fakeMoveCluster) ReadStatefulSet(string, string) (*MoveStatefulSet, error) {
	replicas := f.sts.replicas
	if f.sts.mark != "" {
		replicas = f.sts.saved
	}
	return &MoveStatefulSet{FenceStopped: f.sts.fence, Mark: f.sts.mark, Replicas: replicas}, nil
}

func (f *fakeMoveCluster) RetainedVolume(serviceUID string) (*MovePV, error) {
	for _, pv := range f.pvs {
		if pv.Labels[platform.RetainedLocalCopyLabel] == "true" && pv.Annotations[retainedServiceUIDAnnotation] == serviceUID &&
			pv.Annotations[platform.RetainedLocalCopyStateAnnotation] == platform.RetainedLocalCopyMoving {
			cp := *pv
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeMoveCluster) StopStatefulSet(_, _, mark string, save int32) (int32, error) {
	if f.usedAfterStop > 0 {
		f.used = f.usedAfterStop
	}
	if f.sts.mark == "" {
		f.sts.saved, f.sts.mark = f.sts.replicas, mark
		if save >= 0 {
			f.sts.saved = save
		}
	}
	f.sts.replicas = 0
	delete(f.pods, "db-0")
	if save >= 0 {
		return save, nil
	}
	return f.sts.saved, nil
}

func (f *fakeMoveCluster) StartStatefulSet(_, _ string, replicas int32) (int32, error) {
	if replicas < 0 && f.sts.mark == "" {
		return f.sts.replicas, nil
	}
	if replicas < 0 {
		replicas = f.sts.saved
	}
	f.sts.replicas, f.sts.mark = replicas, ""
	f.startPods()
	return f.sts.replicas, nil
}

func (f *fakeMoveCluster) ServiceReady(string, string) (bool, error) { return true, nil }

func (f *fakeMoveCluster) ReadClaim(_, name string) (*MoveClaim, error) {
	c, ok := f.claims[name]
	if !ok {
		return nil, nil
	}
	cp := *c
	return &cp, nil
}

func (f *fakeMoveCluster) CreateClaim(c MoveClaim) error {
	f.noteFence("create " + c.Name)
	if _, ok := f.claims[c.Name]; ok {
		return errors.New("AlreadyExists")
	}
	c.UID, c.Phase = f.nextUID("claim"), "Pending"
	switch {
	case c.VolumeName != "":
		if pv := f.pvs[c.VolumeName]; pv != nil && pv.ClaimName == c.Name && pv.ClaimUID == "" {
			pv.ClaimUID, pv.Phase, c.Phase = c.UID, "Bound", "Bound"
			if f.replaceOnRebind {
				other := *pv
				other.UID = "pv-other"
				f.pvs[pv.Name] = &other
			}
		}
	case c.Class == platform.LonghornSingleClass:
		name := "pvc-" + c.UID
		f.pvs[name] = &MovePV{Name: name, UID: "pv-" + c.UID, Class: c.Class, Phase: "Bound", Reclaim: "Delete", Capacity: c.Size,
			ClaimNamespace: c.Namespace, ClaimName: c.Name, ClaimUID: c.UID}
		c.VolumeName, c.Phase = name, "Bound"
	}
	f.claims[c.Name] = &c
	return nil
}

func (f *fakeMoveCluster) DeleteClaim(_, name, uid string) error {
	f.noteFence("delete " + name)
	c, ok := f.claims[name]
	if !ok {
		return nil
	}
	if c.UID != uid {
		return errors.New("Conflict: UID precondition failed")
	}
	delete(f.claims, name)
	if pv := f.pvs[c.VolumeName]; pv != nil && pv.ClaimUID == c.UID {
		if pv.Reclaim == "Delete" {
			delete(f.pvs, pv.Name)
			delete(f.contents, pv.Name)
		} else {
			pv.Phase = "Released"
		}
	}
	if name == "data-db-0" && f.strayAfterRelease {
		f.strayAfterRelease = false
		f.claims[name] = &MoveClaim{Namespace: moveNS, Name: name, UID: f.nextUID("stray"), Class: platform.LocalPathClass, Phase: "Pending", Size: "2Gi"}
	}
	return nil
}

func (f *fakeMoveCluster) ReadPV(name string) (*MovePV, error) {
	pv, ok := f.pvs[name]
	if !ok {
		return nil, nil
	}
	if n, going := f.goingPVs[name]; going {
		if n <= 1 {
			delete(f.goingPVs, name)
			delete(f.pvs, name)
		} else {
			f.goingPVs[name] = n - 1
		}
	}
	cp := *pv
	return &cp, nil
}

func (f *fakeMoveCluster) PatchPV(name string, p PVPatch) error {
	pv, ok := f.pvs[name]
	if !ok {
		return errors.New("NotFound")
	}
	if p.ResourceVersion != "" && p.ResourceVersion != pv.ResourceVersion {
		return errors.New("Conflict: the object has been modified")
	}
	f.patches = append(f.patches, p)
	if p.Reclaim != "" && (!f.ignoreRetain || p.Reclaim != "Retain") {
		pv.Reclaim = p.Reclaim
	}
	apply := func(into *map[string]string, changes map[string]*string) {
		if *into == nil {
			*into = map[string]string{}
		}
		for k, v := range changes {
			if v == nil {
				delete(*into, k)
			} else {
				(*into)[k] = *v
			}
		}
	}
	apply(&pv.Labels, p.Labels)
	apply(&pv.Annotations, p.Annotations)
	if p.RebindTo != nil {
		pv.ClaimNamespace, pv.ClaimName, pv.ClaimUID, pv.Phase = p.RebindTo.Namespace, p.RebindTo.Name, "", "Available"
	}
	return nil
}

func (f *fakeMoveCluster) ReadMoveFence(string) (MoveFence, error) {
	return MoveFence{
		Claims:   append([]string(nil), f.nsClaims...),
		Enforced: len(f.nsClaims) > 0 && !f.nsUnenforced,
		Lapsed:   append([]string(nil), f.nsLapsed...),
		Version:  fmt.Sprint(f.nsVersion),
	}, nil
}

func (f *fakeMoveCluster) WriteMoveFence(_ string, fence MoveFence) error {
	if fence.Version != fmt.Sprint(f.nsVersion) {
		return errors.New("Conflict")
	}
	f.nsClaims, f.nsLapsed, f.nsVersion = fence.Claims, fence.Lapsed, f.nsVersion+1
	if len(fence.Claims) == 0 {
		f.nsLapsed = nil
	}
	f.nsUnenforced = false
	return nil
}

func (f *fakeMoveCluster) AdmissionRefuses(_, claim string) (bool, error) {
	return f.policy && !f.nsUnenforced && containsString(f.nsClaims, claim), nil
}

func (f *fakeMoveCluster) PodsMounting(_ string, claims []string) ([]MovePod, error) {
	var out []MovePod
	for _, p := range f.pods {
		for _, c := range p.claims {
			if containsString(claims, c) {
				out = append(out, p.MovePod)
				break
			}
		}
	}
	return out, nil
}

func (f *fakeMoveCluster) CreateSourceVolume(src SourceVolume) error {
	if _, ok := f.pvs[src.PV]; !ok {
		f.pvs[src.PV] = &MovePV{Name: src.PV, UID: f.nextUID("pv"), Phase: "Bound", Reclaim: "Retain", Capacity: src.Capacity,
			Path: src.Path, Node: src.Node, ClaimNamespace: src.Namespace, ClaimName: src.Claim}
	}
	if _, ok := f.claims[src.Claim]; !ok {
		uid := f.nextUID("src")
		f.claims[src.Claim] = &MoveClaim{Namespace: src.Namespace, Name: src.Claim, UID: uid, Phase: "Bound", VolumeName: src.PV, Size: src.Capacity}
		f.pvs[src.PV].ClaimUID = uid
	}
	return nil
}

func (f *fakeMoveCluster) DeletePV(name, uid string) error {
	pv, ok := f.pvs[name]
	if !ok {
		return nil
	}
	if pv.UID != uid {
		return errors.New("Conflict: UID precondition failed")
	}
	if f.slowPVDelete {
		if f.goingPVs == nil {
			f.goingPVs = map[string]int{}
		}
		f.goingPVs[name] = 3
		return nil
	}
	delete(f.pvs, name)
	return nil
}

func (f *fakeMoveCluster) StartCopy(pod CopyPod) (string, error) {
	if f.failCopyStart {
		return "", errors.New("connection lost")
	}
	f.noteFence("copy")
	if _, running := f.pods["db-0"]; running {
		f.unfenced = append(f.unfenced, "copy while db-0 runs")
	}
	f.copies++
	uid := f.nextUID("pod")
	f.pods[pod.Name] = &fakePod{MovePod: MovePod{Name: pod.Name, UID: uid}, claims: []string{pod.Source, pod.Target}}
	if !f.copyOK {
		return uid, nil
	}
	src := f.pvs[f.claims[pod.Source].VolumeName]
	for name, pv := range f.pvs {
		if pv.Path == src.Path && name != src.Name {
			f.contents[f.claims[pod.Target].VolumeName] = f.contents[name]
		}
	}
	return uid, nil
}

func (f *fakeMoveCluster) CopyResult(_, name string) (CopyResult, error) {
	p, ok := f.pods[name]
	if !ok {
		return CopyResult{}, errors.New("the copy pod disappeared")
	}
	if f.copyResultFails > 0 {
		f.copyResultFails--
		return CopyResult{}, errors.New("connection lost")
	}
	if f.copyOK {
		return CopyResult{Finished: true, Succeeded: true, UID: p.UID, Log: "kipper-move-verified 3 entries"}, nil
	}
	if f.failedCopy != nil {
		r := *f.failedCopy
		r.Finished, r.UID = true, p.UID
		return r, nil
	}
	return CopyResult{Finished: true, UID: p.UID, Log: "the copy differs from the source at ./x"}, nil
}

func (f *fakeMoveCluster) PodExists(_, name string) (bool, error) {
	_, ok := f.pods[name]
	return ok, nil
}

func (f *fakeMoveCluster) DeletePod(_, name string) (bool, error) {
	delete(f.pods, name)
	return true, nil
}

func newMove(f *fakeMoveCluster) (*StorageMove, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return &StorageMove{
		Cluster: f, Namespace: moveNS, Service: moveSvc, Operator: "admin", Out: out,
		Now:   func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) },
		Sleep: func(time.Duration) {},
	}, out
}

func requireMoved(t *testing.T, f *fakeMoveCluster) {
	t.Helper()
	data := f.claims["data-db-0"]
	require.NotNil(t, data)
	assert.Equal(t, platform.LonghornSingleClass, data.Class)
	assert.Equal(t, "Bound", data.Phase)
	assert.Equal(t, "the data", f.contents[data.VolumeName], "the data arrived on Longhorn")
	old := f.pvs[moveOldPV]
	require.NotNil(t, old, "the local volume is kept")
	assert.Equal(t, "Retain", old.Reclaim)
	assert.Equal(t, "true", old.Labels[platform.RetainedLocalCopyLabel])
	assert.Equal(t, platform.RetainedLocalCopyMoved, old.Annotations[platform.RetainedLocalCopyStateAnnotation])
	assert.Equal(t, data.UID, old.Annotations[retainedDestinationAnnotation], "prune can tell the verified claim from a replacement")
	assert.Equal(t, "the data", f.contents[moveOldPV])
	assert.Empty(t, f.service.Mark)
	assert.Empty(t, f.sts.mark)
	assert.Equal(t, int32(1), f.sts.replicas)
	assert.Empty(t, f.nsClaims, "the admission fence is lifted")
	assert.Empty(t, f.records)
	assert.NotContains(t, f.claims, "db-dump", "the controller recreates the dump on Longhorn")
	assert.NotContains(t, f.claims, "kipper-move-src-db")
	assert.NotContains(t, f.pvs, "kipper-move-src-01234567")
	assert.NotContains(t, f.pods, "kipper-move-copy-db")
	assert.Contains(t, f.pods, "db-0")
}

func requireRolledBack(t *testing.T, f *fakeMoveCluster) {
	t.Helper()
	data := f.claims["data-db-0"]
	require.NotNil(t, data)
	assert.Equal(t, platform.LocalPathClass, data.Class)
	assert.Equal(t, moveOldPV, data.VolumeName)
	assert.Equal(t, "Bound", data.Phase)
	old := f.pvs[moveOldPV]
	require.NotNil(t, old)
	assert.Equal(t, data.UID, old.ClaimUID)
	assert.Equal(t, "Delete", old.Reclaim)
	assert.NotContains(t, old.Labels, platform.RetainedLocalCopyLabel)
	assert.Empty(t, old.Annotations[platform.RetainedLocalCopyStateAnnotation])
	assert.Equal(t, "the data", f.contents[moveOldPV])
	assert.Equal(t, "2Gi", f.service.Storage)
	assert.Empty(t, f.service.Mark)
	assert.Empty(t, f.sts.mark)
	assert.Equal(t, int32(1), f.sts.replicas)
	assert.Empty(t, f.nsClaims)
	assert.Empty(t, f.records)
	for name, pv := range f.pvs {
		assert.NotEqual(t, platform.LonghornSingleClass, pv.Class, "Longhorn volume %s is left over", name)
	}
	assert.NotContains(t, f.pods, "kipper-move-copy-db")
}

func TestStorageMoveMovesTheDataAndKeepsTheLocalCopy(t *testing.T) {
	f := newFakeMoveCluster()
	m, out := newMove(f)
	var asked MovePlan
	m.Consent = func(p MovePlan) (bool, error) { asked = p; return true, nil }
	var room [2]int64
	m.CheckRoom = func(data, dump int64) error { room = [2]int64{data, dump}; return nil }

	require.NoError(t, m.Run())
	requireMoved(t, f)
	assert.Equal(t, "4Gi", asked.TargetSize, "3 GiB used plus a fifth, rounded up")
	assert.Equal(t, "db-dump", asked.DumpClaim)
	assert.Equal(t, 4*time.Hour, asked.BackupAge)
	assert.Equal(t, [2]int64{4 << 30, 4 << 30}, room, "the replacement dump is counted at full size")
	assert.Equal(t, "4Gi", f.service.Storage, "the declared size follows the larger volume")
	assert.Contains(t, out.String(), "raised")
	assert.Equal(t, 1, f.copies)
}

func TestStorageMoveKeepsALargerDeclaredSize(t *testing.T) {
	f := newFakeMoveCluster()
	f.claims["data-db-0"].Size = "20Gi"
	f.service.Storage = "20Gi"
	m, out := newMove(f)
	require.NoError(t, m.Run())
	assert.Equal(t, "20Gi", f.claims["data-db-0"].Size)
	assert.Equal(t, "20Gi", f.service.Storage)
	assert.NotContains(t, out.String(), "raised")
}

func TestStorageMovePreflightRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(f *fakeMoveCluster, m *StorageMove)
		want  string
	}{
		"local storage":          {func(f *fakeMoveCluster, _ *StorageMove) { f.mode = string(platform.StorageLocal) }, "kip cluster storage longhorn"},
		"already on Longhorn":    {func(f *fakeMoveCluster, _ *StorageMove) { f.claims["data-db-0"].Class = platform.LonghornSingleClass }, "nothing to move"},
		"unbound claim":          {func(f *fakeMoveCluster, _ *StorageMove) { f.claims["data-db-0"].Phase = "Pending" }, "not bound"},
		"fence stopped":          {func(f *fakeMoveCluster, _ *StorageMove) { f.sts.fence = true }, "kip node fence"},
		"directory missing":      {func(f *fakeMoveCluster, _ *StorageMove) { f.paths = nil }, "missing on the server"},
		"volume on another node": {func(f *fakeMoveCluster, _ *StorageMove) { f.pvs[moveOldPV].Node = "worker-1" }, "worker-1"},
		"path outside storage":   {func(f *fakeMoveCluster, _ *StorageMove) { f.pvs[moveOldPV].Path = "/etc" }, "outside"},
		"no completed backup": {func(f *fakeMoveCluster, _ *StorageMove) { f.backups.LatestCompleted = time.Time{} },
			"kip backup create"},
		"backups off without --no-backup": {func(f *fakeMoveCluster, _ *StorageMove) { f.backups = MoveBackupState{} }, "--no-backup"},
		"backups off names how to add them": {func(f *fakeMoveCluster, _ *StorageMove) { f.backups = MoveBackupState{} },
			"kip install again with --backup-storage-bucket"},
		"backup running":     {func(f *fakeMoveCluster, _ *StorageMove) { f.backups.Running = []string{"backup/nightly"} }, "backup/nightly"},
		"migration transfer": {func(f *fakeMoveCluster, _ *StorageMove) { f.backups.Transfers = []string{"dt-1"} }, "dt-1"},
		"no room": {func(_ *fakeMoveCluster, m *StorageMove) {
			m.CheckRoom = func(int64, int64) error { return errors.New("not enough Longhorn room") }
		}, "not enough Longhorn room"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeMoveCluster()
			m, _ := newMove(f)
			tc.setup(f, m)
			err := m.Run()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Empty(t, f.records, "a refused move writes nothing")
			assert.Empty(t, f.service.Mark)
			assert.Equal(t, int32(1), f.sts.replicas)
		})
	}
}

func TestStorageMoveRunsWithoutBackupsOnlyWhenTold(t *testing.T) {
	f := newFakeMoveCluster()
	f.backups = MoveBackupState{}
	m, _ := newMove(f)
	m.NoBackup = true
	require.NoError(t, m.Run())
	requireMoved(t, f)
}

func TestStorageMoveCanceledChangesNothing(t *testing.T) {
	f := newFakeMoveCluster()
	m, _ := newMove(f)
	m.Consent = func(MovePlan) (bool, error) { return false, nil }
	require.ErrorIs(t, m.Run(), ErrMoveCanceled)
	assert.Empty(t, f.records)
	assert.Contains(t, f.pods, "db-0")
}

// Without the admission policy a pod could start on the claim mid-move, so
// the move stops before it marks or stops anything.
func TestStorageMoveStopsWhenAdmissionDoesNotRefuse(t *testing.T) {
	f := newFakeMoveCluster()
	f.policy = false
	m, _ := newMove(f)
	err := m.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), platform.StorageMovePodsPolicy)
	assert.Empty(t, f.service.Mark)
	assert.Equal(t, int32(1), f.sts.replicas)
}

func TestStorageMoveRefusesAForeignPodAndRollsBack(t *testing.T) {
	f := newFakeMoveCluster()
	f.pods["dt-1-import"] = &fakePod{MovePod: MovePod{Name: "dt-1-import"}, claims: []string{"data-db-0"}}
	m, _ := newMove(f)
	err := m.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dt-1-import")
	assert.Equal(t, MoveRecorded, f.records[StorageMoveRecordName(moveUID)].Step)
	assert.Equal(t, "claim-old", f.claims["data-db-0"].UID, "nothing is released while a foreign pod holds the claim")

	delete(f.pods, "dt-1-import")
	require.NoError(t, m.Rollback())
	requireRolledBack(t, f)
	assert.Equal(t, "claim-old", f.claims["data-db-0"].UID, "the original claim is kept")
}

func TestStorageMoveCopyMismatchLeavesTheServiceStoppedThenCopiesAgain(t *testing.T) {
	f := newFakeMoveCluster()
	f.copyOK = false
	m, _ := newMove(f)
	err := m.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the copy of db failed")
	assert.Contains(t, err.Error(), "./x")
	assert.Equal(t, MoveClaimed, f.records[StorageMoveRecordName(moveUID)].Step)
	assert.NotEmpty(t, f.service.Mark)
	assert.Equal(t, int32(0), f.sts.replicas)
	assert.Equal(t, "the data", f.contents[moveOldPV])
	assert.NotContains(t, f.pods, "kipper-move-copy-db", "the copy pod is cleaned up")
	assert.NotContains(t, f.claims, "kipper-move-src-db")

	f.copyOK = true
	require.NoError(t, m.Run())
	requireMoved(t, f)
	assert.Equal(t, 2, f.copies)
}

// Every step must be safe to run again when its work was done but its
// checkpoint never reached the record.
func TestStorageMoveResumesAfterALostCheckpoint(t *testing.T) {
	for _, step := range []string{MoveRecorded, MoveStopped, MoveRetained, MoveReleased, MoveClaimed, MoveCopied, MoveDumpReleased, MoveCommitted} {
		t.Run(step, func(t *testing.T) {
			f := newFakeMoveCluster()
			f.loseCheckpoint = step
			m, _ := newMove(f)
			require.Error(t, m.Run())
			require.NoError(t, m.Run())
			requireMoved(t, f)
		})
	}
}

// A rollback at any point before the commit returns the service to its
// local volume with its data, whether or not the last checkpoint landed.
func TestStorageMoveRollsBackFromEveryStepBeforeTheCommit(t *testing.T) {
	for _, step := range []string{MoveRecorded, MoveStopped, MoveRetained, MoveReleased, MoveClaimed, MoveCopied, MoveDumpReleased} {
		for _, lost := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s lost=%v", step, lost), func(t *testing.T) {
				f := newFakeMoveCluster()
				m, _ := newMove(f)
				stopAfter(t, f, m, step, lost)
				if step == MoveRecorded && lost {
					assert.Error(t, m.Rollback(), "a move whose record was never written has nothing to roll back")
					assert.Equal(t, "claim-old", f.claims["data-db-0"].UID)
					assert.Contains(t, f.pods, "db-0")
					return
				}
				require.NoError(t, m.Rollback())
				requireRolledBack(t, f)
			})
		}
	}
}

// stopAfter runs the move until step completed, then interrupts it. With
// lost set, the step's work is done and its checkpoint is missing.
func stopAfter(t *testing.T, f *fakeMoveCluster, m *StorageMove, step string, lost bool) {
	t.Helper()
	next := map[string]string{
		MoveRecorded: MoveStopped, MoveStopped: MoveRetained, MoveRetained: MoveReleased, MoveReleased: MoveClaimed,
		MoveClaimed: MoveCopied, MoveCopied: MoveDumpReleased, MoveDumpReleased: MoveCommitted,
	}
	if lost {
		f.loseCheckpoint = step
	} else {
		f.loseCheckpoint = next[step]
	}
	require.Error(t, m.Run())
	f.loseCheckpoint = ""
}

func TestStorageMoveRefusesARollbackOnceCommitted(t *testing.T) {
	f := newFakeMoveCluster()
	m, _ := newMove(f)
	f.loseCheckpoint = MoveCommitted
	require.Error(t, m.Run())
	rec := f.records[StorageMoveRecordName(moveUID)]
	rec.Step = MoveCommitted
	f.records[StorageMoveRecordName(moveUID)] = rec
	err := m.Rollback()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "committed")
	require.NoError(t, m.Run())
	requireMoved(t, f)
}

func TestStorageMoveContinuesAnInterruptedRollback(t *testing.T) {
	f := newFakeMoveCluster()
	m, _ := newMove(f)
	stopAfter(t, f, m, MoveCopied, false)
	f.loseCheckpoint = MoveRollbackBound
	require.Error(t, m.Rollback())
	assert.Equal(t, MoveRollback, f.records[StorageMoveRecordName(moveUID)].Direction)

	rebound := f.claims["data-db-0"].UID
	out := &bytes.Buffer{}
	m.Out = out
	require.NoError(t, m.Run(), "a plain rerun continues the rollback")
	assert.Contains(t, out.String(), "continues the rollback")
	requireRolledBack(t, f)
	assert.Equal(t, rebound, f.claims["data-db-0"].UID, "the claim bound to the local volume is never deleted")
}

func TestStorageMoveDeletesAStrayTemplateClaim(t *testing.T) {
	f := newFakeMoveCluster()
	f.strayAfterRelease = true
	m, out := newMove(f)
	require.NoError(t, m.Run())
	requireMoved(t, f)
	assert.Contains(t, out.String(), "empty claim")
}

func TestStorageMoveRefusesAClaimItDidNotCreate(t *testing.T) {
	f := newFakeMoveCluster()
	m, _ := newMove(f)
	stopAfter(t, f, m, MoveReleased, false)
	f.claims["data-db-0"] = &MoveClaim{Namespace: moveNS, Name: "data-db-0", UID: "foreign", Class: platform.LonghornSingleClass, Phase: "Bound", VolumeName: "pvc-x", Size: "4Gi"}
	err := m.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not create")
	assert.Equal(t, "foreign", f.claims["data-db-0"].UID)

	err = m.Rollback()
	require.Error(t, err, "a rollback never deletes a claim it cannot account for")
	assert.Equal(t, "foreign", f.claims["data-db-0"].UID)
}

func TestStorageMoveAdoptsItsClaimAfterALostCreate(t *testing.T) {
	f := newFakeMoveCluster()
	m, _ := newMove(f)
	f.failCopyStart = true
	require.Error(t, m.Run())
	claimUID := f.claims["data-db-0"].UID
	rec := f.records[StorageMoveRecordName(moveUID)]
	rec.Step = MoveReleased
	rec.NewClaimUID = ""
	f.records[StorageMoveRecordName(moveUID)] = rec

	f.failCopyStart = false
	require.NoError(t, m.Run())
	requireMoved(t, f)
	assert.Equal(t, claimUID, f.claims["data-db-0"].UID, "the claim from the lost run is adopted")
}

func TestStorageMoveKeepsAStoppedServiceStopped(t *testing.T) {
	f := newFakeMoveCluster()
	f.sts.replicas = 0
	delete(f.pods, "db-0")
	m, out := newMove(f)
	require.NoError(t, m.Run())
	assert.Equal(t, int32(0), f.sts.replicas)
	assert.Contains(t, out.String(), "stays stopped")
}

func TestMoveTargetSize(t *testing.T) {
	for _, tc := range []struct {
		declared string
		used     int64
		want     string
	}{
		{"2Gi", 3 << 30, "4Gi"},
		{"10Gi", 3 << 30, "10Gi"},
		{"", 0, "1Gi"},
		{"1Gi", 900 << 20, "2Gi"},
		{"5G", 1 << 30, "5G"},
	} {
		got, err := moveTargetSize(tc.declared, tc.used)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "declared %s used %d", tc.declared, tc.used)
	}
	_, err := moveTargetSize("lots", 0)
	assert.Error(t, err)
}

func TestStorageMoveNamesOnlyOwnRecord(t *testing.T) {
	assert.True(t, strings.HasPrefix(StorageMoveRecordName("abc"), "kipper-storage-move-"))
}

// replaceDestination swaps the data claim for another bound Longhorn claim
// holding other data, as a project owner could while kip is not running.
func (f *fakeMoveCluster) replaceDestination() {
	f.claims["data-db-0"] = &MoveClaim{Namespace: moveNS, Name: "data-db-0", UID: "claim-theirs", Class: platform.LonghornSingleClass,
		Phase: "Bound", VolumeName: "pvc-theirs", Size: "4Gi"}
	f.pvs["pvc-theirs"] = &MovePV{Name: "pvc-theirs", UID: "pv-theirs", Class: platform.LonghornSingleClass, Phase: "Bound", Reclaim: "Delete",
		Capacity: "4Gi", ClaimNamespace: moveNS, ClaimName: "data-db-0", ClaimUID: "claim-theirs"}
	f.contents["pvc-theirs"] = "their data"
}

func TestStorageMoveRefusesADestinationReplacedBetweenRuns(t *testing.T) {
	for _, step := range []string{MoveClaimed, MoveCopied, MoveDumpReleased, MoveCommitted} {
		t.Run(step, func(t *testing.T) {
			f := newFakeMoveCluster()
			f.stopAfter = step
			m, _ := newMove(f)
			require.Error(t, m.Run())
			f.replaceDestination()
			copies := f.copies

			err := m.Run()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not the Longhorn claim the move created")
			assert.Equal(t, "their data", f.contents["pvc-theirs"], "the replacement is not overwritten")
			assert.Equal(t, copies, f.copies, "nothing is copied onto the replacement")
			assert.Equal(t, int32(0), f.sts.replicas, "the service does not start on the replacement")
			assert.Equal(t, "the data", f.contents[moveOldPV])
		})
	}
}

func TestStorageMoveRefusesADestinationBeingDeleted(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveClaimed
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.claims["data-db-0"].Deleting = true

	err := m.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not the Longhorn claim the move created")
	assert.Zero(t, f.copies)
}

func TestStorageMoveFencesTheSourceView(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveStopped
	m, _ := newMove(f)
	require.Error(t, m.Run())
	assert.Contains(t, f.nsClaims, "kipper-move-src-db", "no pod but the copy pod can mount the source view")
}

func TestStorageMoveRefusesAPodWaitingForTheSourceView(t *testing.T) {
	f := newFakeMoveCluster()
	f.pods["reader"] = &fakePod{MovePod: MovePod{Name: "reader"}, claims: []string{"kipper-move-src-db"}}
	m, _ := newMove(f)
	err := m.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reader")
	assert.Zero(t, f.copies)
}

func TestStorageMoveRefusesAPodHoldingTheCopyPodName(t *testing.T) {
	f := newFakeMoveCluster()
	f.pods["kipper-move-copy-db"] = &fakePod{MovePod: MovePod{Name: "kipper-move-copy-db"}}
	m, _ := newMove(f)
	err := m.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kipper-move-copy-db")
	assert.Zero(t, f.copies)
	assert.Contains(t, f.pods, "kipper-move-copy-db", "a pod kip did not create is left alone")
}

func TestStorageMoveLeavesAForeignSourceClaimAlone(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveClaimed
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.claims["kipper-move-src-db"] = &MoveClaim{Namespace: moveNS, Name: "kipper-move-src-db", UID: "claim-foreign", Class: platform.LocalPathClass,
		Phase: "Bound", VolumeName: "pvc-foreign", Size: "1Gi"}
	f.pvs["pvc-foreign"] = &MovePV{Name: "pvc-foreign", UID: "pv-foreign", Class: platform.LocalPathClass, Phase: "Bound", Reclaim: "Delete",
		Capacity: "1Gi", Path: "/var/lib/rancher/k3s/storage/pvc-foreign", ClaimNamespace: moveNS, ClaimName: "kipper-move-src-db", ClaimUID: "claim-foreign"}
	f.contents["pvc-foreign"] = "their files"

	err := m.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kipper-move-src-db that the move did not create")
	assert.Zero(t, f.copies)

	require.NoError(t, m.Rollback())
	requireRolledBack(t, f)
	assert.Contains(t, f.claims, "kipper-move-src-db", "rollback leaves a foreign claim alone")
	assert.Equal(t, "their files", f.contents["pvc-foreign"])
}

func TestStorageMoveRefusesAnExistingSourceClaimBeforeStopping(t *testing.T) {
	f := newFakeMoveCluster()
	f.claims["kipper-move-src-db"] = &MoveClaim{Namespace: moveNS, Name: "kipper-move-src-db", UID: "claim-foreign", Phase: "Pending"}
	m, _ := newMove(f)
	err := m.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kipper-move-src-db")
	assert.Equal(t, int32(1), f.sts.replicas, "the service keeps running")
	assert.Empty(t, f.records)
}

func TestStorageMoveRollbackLeavesAReplacedDestinationAlone(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveClaimed
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.replaceDestination()
	theirs := f.claims["data-db-0"]
	theirs.Size = f.records[StorageMoveRecordName(moveUID)].TargetSize
	theirs.Annotations = map[string]string{platform.StorageMoveAnnotation: StorageMoveRecordName(moveUID)}

	err := m.Rollback()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not create")
	assert.Equal(t, "claim-theirs", f.claims["data-db-0"].UID, "a claim that only looks like the move's is kept")
	assert.Equal(t, "their data", f.contents["pvc-theirs"])
	assert.Equal(t, "the data", f.contents[moveOldPV])
}

func TestStorageMoveRollbackBindsAgainWhenTheClaimWentAwayAfterItsCheckpoint(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveCopied
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.stopAfter = MoveRollbackBound
	require.Error(t, m.Rollback())
	rebound := f.claims["data-db-0"]
	require.NoError(t, f.DeleteClaim(moveNS, "data-db-0", rebound.UID))
	require.Equal(t, "Retain", f.pvs[moveOldPV].Reclaim)

	require.NoError(t, m.Rollback())
	requireRolledBack(t, f)
}

func TestStorageMoveRollbackKeepsTheVolumeOnRetainWhenAForeignClaimTookItsName(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveCopied
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.stopAfter = MoveRollbackBound
	require.Error(t, m.Rollback())
	rebound := f.claims["data-db-0"]
	require.NoError(t, f.DeleteClaim(moveNS, "data-db-0", rebound.UID))
	f.claims["data-db-0"] = &MoveClaim{Namespace: moveNS, Name: "data-db-0", UID: "claim-foreign", Class: platform.LocalPathClass, Phase: "Pending", Size: "2Gi"}

	err := m.Rollback()
	require.Error(t, err)
	assert.Equal(t, "Retain", f.pvs[moveOldPV].Reclaim, "the only copy of the data is never handed to the provisioner")
	assert.Equal(t, "the data", f.contents[moveOldPV])
}

func TestStorageMoveRollbackKeepsTheVolumeOnRetainWhileItsClaimIsBeingDeleted(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveCopied
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.stopAfter = MoveRollbackBound
	require.Error(t, m.Rollback())
	f.claims["data-db-0"].Deleting = true

	err := m.Rollback()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keeps it on Retain")
	assert.Equal(t, "Retain", f.pvs[moveOldPV].Reclaim)
}

func TestStorageMoveLeavesADumpClaimReplacedDuringTheMoveAlone(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveCopied
	m, out := newMove(f)
	require.Error(t, m.Run())
	f.claims["db-dump"] = &MoveClaim{Namespace: moveNS, Name: "db-dump", UID: "dump-theirs", Class: platform.LocalPathClass, Phase: "Bound",
		VolumeName: "pvc-dump-theirs", Size: "2Gi", Labels: map[string]string{dumpForLabel: moveSvc}}
	f.pvs["pvc-dump-theirs"] = &MovePV{Name: "pvc-dump-theirs", UID: "pv-dump-theirs", Class: platform.LocalPathClass, Phase: "Bound", Reclaim: "Delete",
		Capacity: "2Gi", ClaimNamespace: moveNS, ClaimName: "db-dump", ClaimUID: "dump-theirs"}
	f.contents["pvc-dump-theirs"] = "their dump"

	require.NoError(t, m.Run())
	assert.Equal(t, "dump-theirs", f.claims["db-dump"].UID, "a dump claim the move did not record is not deleted")
	assert.Equal(t, "their dump", f.contents["pvc-dump-theirs"])
	assert.Contains(t, out.String(), "db-dump")
}

func TestStorageMoveRetainsTheOldVolumeAgainBeforeReleasingItsClaim(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveRetained
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.pvs[moveOldPV].Reclaim = "Delete"

	require.NoError(t, m.Run())
	requireMoved(t, f)
}

func TestStorageMoveCleanupLeavesAForeignClaimOnTheSourceVolumeName(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveClaimed
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.claims["kipper-move-src-db"] = &MoveClaim{Namespace: moveNS, Name: "kipper-move-src-db", UID: "claim-foreign", Class: platform.LocalPathClass,
		Phase: "Bound", VolumeName: "kipper-move-src-01234567", Size: "1Gi"}
	f.pvs["kipper-move-src-01234567"] = &MovePV{Name: "kipper-move-src-01234567", UID: "pv-foreign", Class: platform.LocalPathClass, Phase: "Bound",
		Reclaim: "Delete", Capacity: "1Gi", Path: "/var/lib/rancher/k3s/storage/pvc-foreign", ClaimNamespace: moveNS, ClaimName: "kipper-move-src-db", ClaimUID: "claim-foreign"}
	f.contents["kipper-move-src-01234567"] = "their files"

	require.NoError(t, m.Rollback())
	requireRolledBack(t, f)
	assert.Contains(t, f.claims, "kipper-move-src-db", "a claim on a volume that is not the move's view is left alone")
	assert.Equal(t, "their files", f.contents["kipper-move-src-01234567"])
}

func TestStorageMoveStopsWhenTheOldVolumeWillNotStayOnRetain(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveRetained
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.pvs[moveOldPV].Reclaim = "Delete"
	f.ignoreRetain = true

	err := m.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not kept on Retain")
	assert.Contains(t, f.claims, "data-db-0", "the claim holding the only copy is not released")
	assert.Equal(t, "the data", f.contents[moveOldPV])
}

func TestStorageMoveRollbackLeavesAForeignPodWithTheCopyPodNameAlone(t *testing.T) {
	f := newFakeMoveCluster()
	f.pods["kipper-move-copy-db"] = &fakePod{MovePod: MovePod{Name: "kipper-move-copy-db"}}
	m, _ := newMove(f)
	require.Error(t, m.Run())

	require.NoError(t, m.Rollback())
	assert.Contains(t, f.pods, "kipper-move-copy-db", "a pod the move refused as foreign is not deleted")
	assert.Equal(t, int32(1), f.sts.replicas)
	assert.Empty(t, f.nsClaims)
	assert.Empty(t, f.records)
}

func TestStorageMoveFinishesWhenTheRetainedCopyIsAlreadyGone(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveCommitted
	m, _ := newMove(f)
	require.Error(t, m.Run())
	delete(f.pvs, moveOldPV)
	delete(f.contents, moveOldPV)

	require.NoError(t, m.Run())
	assert.Equal(t, int32(1), f.sts.replicas, "the service starts on its verified Longhorn volume")
	assert.Empty(t, f.records)
	assert.Equal(t, "the data", f.contents[f.claims["data-db-0"].VolumeName])
}

func TestStorageMoveRollbackRebindsOnlyTheVolumeItRead(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveCopied
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.pvs[moveOldPV].ResourceVersion = "9"

	require.NoError(t, m.Rollback())
	var rebind *PVPatch
	for i := range f.patches {
		if f.patches[i].RebindTo != nil {
			rebind = &f.patches[i]
		}
	}
	require.NotNil(t, rebind)
	assert.Equal(t, "9", rebind.ResourceVersion, "a volume changed since the read is not rebound")
}

func TestStorageMoveRollbackStopsWhenTheVolumeIsReplacedWhileBinding(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveCopied
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.replaceOnRebind = true

	err := m.Rollback()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "replaced")
}

// prebound puts a local-path claim at data-db-0 that names a volume holding
// data, still Pending while binding completes.
func (f *fakeMoveCluster) prebound() {
	f.claims["data-db-0"] = &MoveClaim{Namespace: moveNS, Name: "data-db-0", UID: "claim-prebound", Class: platform.LocalPathClass,
		Phase: "Pending", VolumeName: "pvc-restored", Size: "2Gi"}
	f.pvs["pvc-restored"] = &MovePV{Name: "pvc-restored", UID: "pv-restored", Class: platform.LocalPathClass, Phase: "Available", Reclaim: "Delete",
		Capacity: "2Gi", Path: "/var/lib/rancher/k3s/storage/pvc-restored", ClaimNamespace: moveNS, ClaimName: "data-db-0"}
	f.contents["pvc-restored"] = "restored data"
}

func TestStorageMoveNeverTakesAPreboundClaimForAStray(t *testing.T) {
	t.Run("forward", func(t *testing.T) {
		f := newFakeMoveCluster()
		f.stopAfter = MoveReleased
		m, _ := newMove(f)
		require.Error(t, m.Run())
		f.prebound()

		require.Error(t, m.Run())
		assert.Equal(t, "claim-prebound", f.claims["data-db-0"].UID)
		assert.Equal(t, "restored data", f.contents["pvc-restored"])
	})
	t.Run("rollback", func(t *testing.T) {
		f := newFakeMoveCluster()
		f.stopAfter = MoveReleased
		m, _ := newMove(f)
		require.Error(t, m.Run())
		f.prebound()

		require.Error(t, m.Rollback())
		assert.Equal(t, "claim-prebound", f.claims["data-db-0"].UID)
		assert.Equal(t, "restored data", f.contents["pvc-restored"])
	})
}

// A volume is deleted only once its protection finalizer is gone. A rerun
// that reads the source view back too early must not take its own view for
// someone else's.
func TestStorageMoveWaitsForItsSourceViewToGoBeforeCopyingAgain(t *testing.T) {
	f := newFakeMoveCluster()
	f.failCopyStart = true
	m, _ := newMove(f)
	require.Error(t, m.Run())
	require.Contains(t, f.pvs, "kipper-move-src-01234567", "the interrupted run left its view behind")

	f.failCopyStart = false
	f.slowPVDelete = true
	require.NoError(t, m.Run())
	requireMoved(t, f)
}

// The fence may have been removed while kip was stopped. Resuming an
// uncommitted forward move restores it before continuing.
func TestStorageMoveRestoresTheFenceBeforeResuming(t *testing.T) {
	for _, step := range []string{MoveStopped, MoveRetained, MoveReleased, MoveClaimed} {
		t.Run(step, func(t *testing.T) {
			f := newFakeMoveCluster()
			f.stopAfter = step
			m, _ := newMove(f)
			require.Error(t, m.Run())
			f.nsClaims = nil

			require.NoError(t, m.Run())
			requireMoved(t, f)
			assert.Empty(t, f.unfenced)
		})
	}
}

func TestStorageMoveDoesNotResumeWithoutAnEnforcingPolicy(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveClaimed
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.policy = false

	err := m.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), platform.StorageMovePodsPolicy)
	assert.Zero(t, f.copies)
}

func TestStorageMoveStopsTheServiceAgainWhenItWasStartedDuringAPause(t *testing.T) {
	f := newFakeMoveCluster()
	f.sts.replicas = 2
	f.startPods()
	f.stopAfter = MoveClaimed
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.sts.replicas = 1
	f.startPods()

	require.NoError(t, m.Run())
	assert.Empty(t, f.unfenced, "nothing is copied while the service runs")
	assert.Equal(t, int32(2), f.sts.replicas, "the count saved before the move is restored")
}

// A project editor can change the StatefulSet's saved-count annotation while
// a move is paused. The move record lives where they cannot write, so its
// count is the one restored.
func TestStorageMoveRestoresTheCountItRecordedOverAnEditedAnnotation(t *testing.T) {
	f := newFakeMoveCluster()
	f.sts.replicas = 2
	f.startPods()
	f.stopAfter = MoveClaimed
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.sts.saved = 5

	require.NoError(t, m.Run())
	assert.Equal(t, int32(2), f.sts.replicas)
}

func TestStorageMoveKeepsTheSavedCountWhenTheStatefulSetLostItsMark(t *testing.T) {
	f := newFakeMoveCluster()
	f.sts.replicas = 2
	f.startPods()
	f.stopAfter = MoveClaimed
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.sts.mark = ""

	require.NoError(t, m.Run())
	assert.Equal(t, int32(2), f.sts.replicas)
}

func TestStorageMoveRollbackRestoresTheFenceFirst(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveCopied
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.nsClaims = nil
	f.unfenced = nil

	require.NoError(t, m.Rollback())
	requireRolledBack(t, f)
	assert.Empty(t, f.unfenced)
}

func TestStorageMoveRollbackRefusesAForeignPodThatArrivedWhileUnfenced(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveCopied
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.nsClaims = nil
	f.pods["reader"] = &fakePod{MovePod: MovePod{Name: "reader"}, claims: []string{"data-db-0"}}

	err := m.Rollback()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reader")
	assert.Equal(t, platform.LonghornSingleClass, f.claims["data-db-0"].Class, "nothing is deleted under a foreign pod")
}

// A verified copy survives a lost checkpoint or a connection failure during
// copying because the recorded pod UID identifies the move's own result.
func TestStorageMoveKeepsItsOwnVerifiedCopy(t *testing.T) {
	t.Run("checkpoint lost", func(t *testing.T) {
		f := newFakeMoveCluster()
		f.loseCheckpoint = MoveCopied
		m, _ := newMove(f)
		require.Error(t, m.Run())
		require.NoError(t, m.Run())
		requireMoved(t, f)
		assert.Equal(t, 1, f.copies)
	})
	t.Run("connection dropped during the copy", func(t *testing.T) {
		f := newFakeMoveCluster()
		f.copyResultFails = 1
		m, _ := newMove(f)
		require.Error(t, m.Run())
		require.NoError(t, m.Run())
		requireMoved(t, f)
		assert.Equal(t, 1, f.copies)
	})
}

func TestStorageMoveCopiesAgainWhenTheCopyPodIsNotItsOwn(t *testing.T) {
	f := newFakeMoveCluster()
	f.copyResultFails = 1
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.pods["kipper-move-copy-db"].UID = "pod-someone-else"

	require.NoError(t, m.Run())
	requireMoved(t, f)
	assert.Equal(t, 2, f.copies)
}

func TestStorageMoveSaysHowASilentCopyEnded(t *testing.T) {
	f := newFakeMoveCluster()
	f.copyOK = false
	f.failedCopy = &CopyResult{Reason: "OOMKilled (exit 137)"}
	m, _ := newMove(f)
	err := m.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OOMKilled (exit 137)")
	assert.Contains(t, err.Error(), "printed nothing")
}

// The size is measured before the question, while the service still writes.
// Once it has stopped the data no longer changes, so the move measures again
// and sizes the Longhorn volume from that.
func TestStorageMoveSizesTheVolumeFromTheStoppedData(t *testing.T) {
	f := newFakeMoveCluster()
	f.usedAfterStop = 4 << 30
	m, out := newMove(f)
	var asked MovePlan
	m.Consent = func(p MovePlan) (bool, error) { asked = p; return true, nil }
	var rooms [][2]int64
	m.CheckRoom = func(data, dump int64) error { rooms = append(rooms, [2]int64{data, dump}); return nil }

	require.NoError(t, m.Run())
	requireMoved(t, f)
	assert.Equal(t, "4Gi", asked.TargetSize)
	assert.Equal(t, "5Gi", f.claims["data-db-0"].Size, "4 GiB written by the time it stopped, plus a fifth")
	assert.Equal(t, "5Gi", f.service.Storage)
	assert.Equal(t, fmt.Sprint(int64(4<<30)), f.pvs[moveOldPV].Annotations[platform.RetainedLocalCopyUsedBytesAnnotation])
	assert.Equal(t, [2]int64{5 << 30, 5 << 30}, rooms[len(rooms)-1], "the larger volume is checked for room")
	assert.Contains(t, out.String(), "5Gi")
}

func TestStorageMoveStopsWhenTheStoppedDataNoLongerFits(t *testing.T) {
	f := newFakeMoveCluster()
	f.usedAfterStop = 40 << 30
	m, _ := newMove(f)
	m.CheckRoom = func(data, _ int64) error {
		if data > 10<<30 {
			return errors.New("not enough Longhorn room")
		}
		return nil
	}

	err := m.Run()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not enough Longhorn room")
	assert.Equal(t, "claim-old", f.claims["data-db-0"].UID, "nothing is released")
	require.NoError(t, m.Rollback())
	requireRolledBack(t, f)
}

// A lost record leaves the service marked as moving. Rollback reconstructs
// the record from the remaining volumes so it can undo the move.
func TestStorageMoveRollbackRecoversAMoveWhoseRecordIsGone(t *testing.T) {
	for _, step := range []string{MoveStopped, MoveRetained, MoveReleased, MoveClaimed, MoveCopied} {
		t.Run(step, func(t *testing.T) {
			f := newFakeMoveCluster()
			f.stopAfter = step
			m, out := newMove(f)
			require.Error(t, m.Run())
			f.records = map[string]MoveRecord{}

			err := m.Run()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--rollback")

			require.NoError(t, m.Rollback())
			assert.Contains(t, out.String(), "record")
			data := f.claims["data-db-0"]
			require.NotNil(t, data)
			assert.Equal(t, moveOldPV, data.VolumeName)
			assert.Equal(t, moveSvc, data.Labels["app"], "deleting the service with its data finds the claim by this label")
			assert.Equal(t, "the data", f.contents[moveOldPV])
			assert.Equal(t, "Delete", f.pvs[moveOldPV].Reclaim)
			assert.Empty(t, f.service.Mark)
			assert.Empty(t, f.sts.mark)
			assert.Equal(t, int32(1), f.sts.replicas)
			assert.Empty(t, f.nsClaims)
			assert.Empty(t, f.records)
			assert.Empty(t, f.unfenced)
			for name, pv := range f.pvs {
				assert.NotEqual(t, platform.LonghornSingleClass, pv.Class, "Longhorn volume %s is left over", name)
			}
			assert.NotContains(t, f.pods, "kipper-move-copy-db")
			assert.NotContains(t, f.claims, "kipper-move-src-db")
		})
	}
}

func TestStorageMoveRollbackClearsAMarkWithNothingLeftToRecover(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveCopied
	m, out := newMove(f)
	require.Error(t, m.Run())
	f.records = map[string]MoveRecord{}
	delete(f.pvs, moveOldPV)
	delete(f.contents, moveOldPV)

	require.NoError(t, m.Rollback())
	assert.Contains(t, out.String(), "No local copy")
	assert.Empty(t, f.service.Mark)
	assert.Empty(t, f.sts.mark)
	assert.Equal(t, int32(1), f.sts.replicas, "the service runs on the only data left")
	assert.Empty(t, f.nsClaims)
	assert.Equal(t, platform.LonghornSingleClass, f.claims["data-db-0"].Class, "the only data left is kept")
}

// A rollback of a move that never stopped the service has no saved count to
// keep. Resuming it must leave the running service as it was.
func TestStorageMoveRollbackOfAMoveThatNeverStoppedKeepsTheServiceRunning(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveRecorded
	m, _ := newMove(f)
	require.Error(t, m.Run())
	require.Equal(t, int32(1), f.sts.replicas)
	f.stopAfter = MoveRollbackCleaned
	require.Error(t, m.Rollback())

	require.NoError(t, m.Rollback())
	requireRolledBack(t, f)
}

// A pod by the copy pod's name that this move did not start may have written
// the volume while the fence was down. It is removed, and a copy it may have
// changed is made again before anything commits.
func TestStorageMoveCopiesAgainAfterAStrangeCopyPodHeldTheVolume(t *testing.T) {
	for _, step := range []string{MoveCopied, MoveDumpReleased} {
		t.Run(step, func(t *testing.T) {
			f := newFakeMoveCluster()
			f.stopAfter = step
			m, out := newMove(f)
			require.Error(t, m.Run())
			f.pods["kipper-move-copy-db"] = &fakePod{MovePod: MovePod{Name: "kipper-move-copy-db", UID: "pod-strange"}, claims: []string{"data-db-0"}}

			require.NoError(t, m.Run())
			requireMoved(t, f)
			assert.Equal(t, 2, f.copies)
			assert.Contains(t, out.String(), "copies the data again")
		})
	}
}

// The strange pod is the only sign that the verified copy may have changed,
// so the move records that it copies again before it deletes the pod.
func TestStorageMoveRecordsTheRecopyBeforeDeletingAStrangeCopyPod(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveCopied
	m, _ := newMove(f)
	require.Error(t, m.Run())
	f.pods["kipper-move-copy-db"] = &fakePod{MovePod: MovePod{Name: "kipper-move-copy-db", UID: "pod-strange"}, claims: []string{"data-db-0"}}
	f.loseCheckpoint = MoveClaimed

	require.Error(t, m.Run())
	require.NoError(t, m.Run())
	requireMoved(t, f)
	assert.Equal(t, 2, f.copies)
}

// Possible writes after verification require another copy. Recording this
// before refusing or stopping the writer ensures that following the refusal's
// advice still leads to a fresh copy before commit.
func TestStorageMoveCopiesAgainAfterSomethingElseHadTheVolume(t *testing.T) {
	writer := func(f *fakeMoveCluster) {
		f.pods["writer"] = &fakePod{MovePod: MovePod{Name: "writer", UID: "pod-w"}, claims: []string{"data-db-0"}}
	}
	for name, tc := range map[string]struct {
		step    string
		lose    bool
		disturb func(f *fakeMoveCluster)
		refused bool
	}{
		"a writer pod after a lost copied checkpoint":   {step: MoveCopied, lose: true, disturb: writer, refused: true},
		"the fence down after a lost copied checkpoint": {step: MoveCopied, lose: true, disturb: func(f *fakeMoveCluster) { f.nsClaims = nil }},
		"a writer pod at copied":                        {step: MoveCopied, disturb: writer, refused: true},
		"a writer pod at dump":                          {step: MoveDumpReleased, disturb: writer, refused: true},
		"the service started again": {step: MoveCopied, disturb: func(f *fakeMoveCluster) {
			f.sts.replicas = 1
			f.startPods()
		}},
		"the fence was down": {step: MoveCopied, disturb: func(f *fakeMoveCluster) { f.nsClaims = nil }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeMoveCluster()
			if tc.lose {
				f.loseCheckpoint = tc.step
			} else {
				f.stopAfter = tc.step
			}
			m, out := newMove(f)
			require.Error(t, m.Run())
			tc.disturb(f)

			if tc.refused {
				err := m.Run()
				require.Error(t, err)
				assert.Contains(t, err.Error(), "writer")
				rec := f.records[StorageMoveRecordName(moveUID)]
				assert.Equal(t, MoveClaimed, rec.Step)
				assert.Empty(t, rec.CopyPodUID, "the verified copy is no longer trusted")
				delete(f.pods, "writer")
			}
			require.NoError(t, m.Run())
			requireMoved(t, f)
			assert.Equal(t, 2, f.copies)
			assert.Contains(t, out.String(), "copies the data again")
		})
	}
}

// Two unfinished moves share a namespace's fence. Whichever move finds the
// label gone restores it without dropping the other's claims, and records
// the gap so the other move still copies again when it resumes.
func TestStorageMoveKeepsAnotherMovesFenceWhenItRestoresTheLabel(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveCopied
	m, out := newMove(f)
	require.Error(t, m.Run())
	f.nsUnenforced = true

	other, _ := newMove(f)
	require.NoError(t, other.addMoveClaims(moveNS, []string{"data-other-0"}))
	assert.False(t, f.nsUnenforced)
	assert.Subset(t, f.nsClaims, []string{"data-db-0", "kipper-move-src-db", "data-other-0"})
	assert.Contains(t, f.nsLapsed, "data-db-0")
	assert.NotContains(t, f.nsLapsed, "data-other-0", "the move restoring the label accounts for its own claims")

	require.NoError(t, m.Run())
	assert.Empty(t, f.records, "the move finished")
	assert.Equal(t, 2, f.copies)
	assert.Contains(t, out.String(), "copies the data again")
	assert.Equal(t, []string{"data-other-0"}, f.nsClaims, "the other move stays fenced")
	assert.Empty(t, f.nsLapsed)
	assert.False(t, f.nsUnenforced)
}

func TestStorageMoveTreatsOnlyItsOwnCopyPodAsItsOwn(t *testing.T) {
	f := newFakeMoveCluster()
	f.stopAfter = MoveClaimed
	m, _ := newMove(f)
	require.Error(t, m.Run())
	rec := f.records[StorageMoveRecordName(moveUID)]
	f.pods["kipper-move-copy-db"] = &fakePod{MovePod: MovePod{Name: "kipper-move-copy-db", UID: "pod-strange"}, claims: []string{"data-db-0"}}

	_, err := m.foreignPods(&rec, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kipper-move-copy-db")
}
