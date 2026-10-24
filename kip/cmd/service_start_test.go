package cmd

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/controller/pkg/stranded"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

type fakeStarter struct {
	mark     *installer.FenceStopMark
	records  []installer.NodeRecord
	replicas []stranded.Replica
	started  []string
	saved    []installer.NodeRecord
	volumes  []string
	cleared  []string
}

func (f *fakeStarter) FenceStop(string, string) (*installer.FenceStopMark, error) { return f.mark, nil }
func (f *fakeStarter) Records() ([]installer.NodeRecord, error)                   { return f.records, nil }
func (f *fakeStarter) Replicas() ([]stranded.Replica, error)                      { return f.replicas, nil }
func (f *fakeStarter) LonghornVolumes() ([]string, error)                         { return f.volumes, nil }
func (f *fakeStarter) ClearStop(ns, name string) error {
	f.cleared = append(f.cleared, ns+"/"+name)
	return nil
}
func (f *fakeStarter) Start(ns, name string) error {
	f.started = append(f.started, ns+"/"+name)
	return nil
}
func (f *fakeStarter) SaveRecord(rec installer.NodeRecord) error {
	f.saved = append(f.saved, rec)
	return nil
}

func starterFor(state string, replicas ...stranded.Replica) *fakeStarter {
	rec := worker(state)
	if installer.Fenced(state) {
		rec.Fence = killallFence()
		rec.Fence.Stopped = []config.FenceStop{{Namespace: "shop", StatefulSet: "db", Reason: installer.FenceStopStale}}
	}
	return &fakeStarter{
		mark:     &installer.FenceStopMark{Node: "worker-1", Replicas: 1, Reason: installer.FenceStopStale, Volumes: []string{"pvc-1"}},
		records:  []installer.NodeRecord{{Name: "server-1", Role: installer.NodeRoleServer, State: installer.NodeStateReady}, rec},
		replicas: replicas,
		volumes:  []string{"pvc-1"},
	}
}

func TestServiceStartRefusesAServiceTheFenceDidNotStop(t *testing.T) {
	s := &fakeStarter{}
	err := fenceStoppedStart{Out: &bytes.Buffer{}, Server: s}.run("shop", "db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip service restart db")
	assert.Empty(t, s.started)
}

func TestServiceStartStartsOnACurrentCopyAndClearsTheRecord(t *testing.T) {
	s := starterFor(installer.NodeStateFenced, stranded.Replica{Name: "r1", Volume: "pvc-1", Node: "server-1", HealthyAt: healthy})
	require.NoError(t, fenceStoppedStart{Out: &bytes.Buffer{}, Server: s}.run("shop", "db"))
	assert.Equal(t, []string{"shop/db"}, s.started)
	require.Len(t, s.saved, 1)
	assert.Empty(t, s.saved[0].Fence.Stopped)
}

func TestServiceStartCountsTheFencedWorkersCopyOnlyAfterItRejoined(t *testing.T) {
	onWorker := stranded.Replica{Name: "r1", Volume: "pvc-1", Node: "worker-1", HealthyAt: healthy}
	s := starterFor(installer.NodeStateFenced, onWorker)
	err := fenceStoppedStart{Out: &bytes.Buffer{}, Server: s}.run("shop", "db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip node add")

	s = starterFor(installer.NodeStateReady, onWorker)
	require.NoError(t, fenceStoppedStart{Out: &bytes.Buffer{}, Server: s}.run("shop", "db"))
	assert.Equal(t, []string{"shop/db"}, s.started)
}

func TestServiceStartNeedsConsentForStaleCopies(t *testing.T) {
	stale := stranded.Replica{Name: "r1", Volume: "pvc-1", Node: "server-1", HealthyAt: healthy, LastFailedAt: "2026-10-24T08:30:00Z"}
	s := starterFor(installer.NodeStateFenced, stale)
	err := fenceStoppedStart{Out: &bytes.Buffer{}, Server: s}.run("shop", "db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--accept-stale-data")
	assert.Empty(t, s.started)

	var asked []string
	out := &bytes.Buffer{}
	s = starterFor(installer.NodeStateFenced, stale)
	start := fenceStoppedStart{Out: out, Server: s, Interactive: true, Confirm: func(p string) (bool, error) {
		asked = append(asked, p)
		return false, nil
	}}
	require.Error(t, start.run("shop", "db"))
	assert.Len(t, asked, 1)
	assert.Contains(t, out.String(), "last failed at 2026-10-24T08:30:00Z")
	assert.Empty(t, s.started)

	s = starterFor(installer.NodeStateFenced, stale)
	require.NoError(t, fenceStoppedStart{Out: &bytes.Buffer{}, Server: s, AcceptStale: true}.run("shop", "db"))
	assert.Equal(t, []string{"shop/db"}, s.started)
}

func TestServiceStartIgnoresCopiesOnAnotherFencedOrRemovedWorker(t *testing.T) {
	removing := otherFencedWorker()
	removing.State, removing.Fence = installer.NodeStateRemoving, nil
	wiping := removing
	wiping.State = installer.NodeStateWiping
	for _, accept := range []bool{false, true} {
		for name, other := range map[string][]installer.NodeRecord{
			"fenced":   {otherFencedWorker()},
			"removed":  nil,
			"removing": {removing},
			"wiping":   {wiping},
		} {
			t.Run(fmt.Sprintf("%s/accept=%v", name, accept), func(t *testing.T) {
				s := starterFor(installer.NodeStateFenced, stranded.Replica{Name: "r1", Volume: "pvc-1", Node: "worker-2", HealthyAt: healthy})
				s.records = append(s.records, other...)
				err := fenceStoppedStart{Out: &bytes.Buffer{}, Server: s, AcceptStale: accept}.run("shop", "db")
				require.Error(t, err)
				assert.Contains(t, err.Error(), "the only copy")
				assert.Empty(t, s.started)
				assert.Empty(t, s.saved)
			})
		}
	}
}

func TestServiceStartTellsHowToRestoreAServiceWhoseOnlyCopyIsGone(t *testing.T) {
	s := starterFor(installer.NodeStateFenced)
	s.volumes = nil
	err := fenceStoppedStart{Out: &bytes.Buffer{}, Server: s}.run("shop", "db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no longer exist")
	assert.Contains(t, err.Error(), "kip service delete db --delete-data")
	assert.Contains(t, err.Error(), "kip backup restore <backup> --namespace shop")
	assert.Contains(t, err.Error(), "from a backup taken before the fence")
	assert.NotContains(t, err.Error(), "kip node add")
	assert.Empty(t, s.cleared)

	s = starterFor(installer.NodeStateFenced)
	s.volumes = nil
	var asked []string
	start := fenceStoppedStart{Out: &bytes.Buffer{}, Server: s, Interactive: true, Confirm: func(p string) (bool, error) {
		asked = append(asked, p)
		return true, nil
	}}
	require.NoError(t, start.run("shop", "db"))
	assert.Len(t, asked, 1)
	assert.Equal(t, []string{"shop/db"}, s.cleared, "the mark goes, and the service stays stopped")
	assert.Empty(t, s.started)
	require.Len(t, s.saved, 1)
	assert.Empty(t, s.saved[0].Fence.Stopped)
}

func TestServiceStartNamesTheRejoinOnlyWhileTheWorkerIsRecorded(t *testing.T) {
	s := starterFor(installer.NodeStateFenced)
	err := fenceStoppedStart{Out: &bytes.Buffer{}, Server: s}.run("shop", "db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip node add")
	assert.Contains(t, err.Error(), "kip service delete db --delete-data")

	s = starterFor(installer.NodeStateFenced)
	s.records = s.records[:1]
	err = fenceStoppedStart{Out: &bytes.Buffer{}, Server: s}.run("shop", "db")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "kip node add", "the worker was removed")
	assert.Contains(t, err.Error(), "kip service delete db --delete-data")
}
