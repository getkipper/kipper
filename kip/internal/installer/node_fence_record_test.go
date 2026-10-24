package installer

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
)

func fencedWorker(state string, fence *config.NodeFence) NodeRecord {
	return NodeRecord{Name: "worker-1", Host: "203.0.113.11", MachineID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Role: NodeRoleWorker, State: state, Fence: fence}
}

func validFence() *config.NodeFence {
	return &config.NodeFence{Method: FenceMethodKillall, Operator: "ops", StartedAt: "2026-10-24T08:00:00Z",
		BootID: "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"}
}

func TestFenceStatesNeedAFenceRecord(t *testing.T) {
	require.NoError(t, validateNodeRecord(fencedWorker(NodeStateFencing, validFence())))
	require.NoError(t, validateNodeRecord(fencedWorker(NodeStateFenced, validFence())))
	assert.Error(t, validateNodeRecord(fencedWorker(NodeStateFencing, nil)))
	assert.Error(t, validateNodeRecord(fencedWorker(NodeStateFenced, nil)))
	// A forced removal that started from a fence keeps it until the record goes.
	require.NoError(t, validateNodeRecord(fencedWorker(NodeStateRemoving, validFence())))
	require.NoError(t, validateNodeRecord(fencedWorker(NodeStateWiping, validFence())))
	assert.Error(t, validateNodeRecord(fencedWorker(NodeStateReady, validFence())))
	assert.Error(t, validateNodeRecord(fencedWorker(NodeStateJoining, validFence())))
}

func TestFenceRecordAcceptsAnUnknownBootIDAndLaterMethods(t *testing.T) {
	f := validFence()
	f.BootID = BootIDUnknown
	f.Method = "provider-poweroff"
	require.NoError(t, validateNodeRecord(fencedWorker(NodeStateFenced, f)))
}

func TestFenceRecordRefusesUnsafeValues(t *testing.T) {
	cases := map[string]func(*config.NodeFence){
		"method":       func(f *config.NodeFence) { f.Method = "kill; rm -rf /" },
		"empty method": func(f *config.NodeFence) { f.Method = "" },
		"boot id":      func(f *config.NodeFence) { f.BootID = "not-a-boot-id" },
		"checkpoint":   func(f *config.NodeFence) { f.RebootCheckpoint = "unknown" },
		"started":      func(f *config.NodeFence) { f.StartedAt = "today" },
		"fenced":       func(f *config.NodeFence) { f.FencedAt = "today" },
		"namespace": func(f *config.NodeFence) {
			f.Stopped = []config.FenceStop{{Namespace: "Shop$", StatefulSet: "db", Reason: FenceStopStale}}
		},
		"statefulset": func(f *config.NodeFence) {
			f.Stopped = []config.FenceStop{{Namespace: "shop", StatefulSet: "db'x", Reason: FenceStopStale}}
		},
		"reason": func(f *config.NodeFence) {
			f.Stopped = []config.FenceStop{{Namespace: "shop", StatefulSet: "db", Reason: "maybe"}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := validFence()
			mutate(f)
			assert.Error(t, validateNodeRecord(fencedWorker(NodeStateFenced, f)))
		})
	}
}

func TestFenceRecordRoundTripsThroughTheConfigMap(t *testing.T) {
	f := validFence()
	f.Stopped = []config.FenceStop{{Namespace: "shop", StatefulSet: "db", Reason: FenceStopStale,
		Consent: []config.ReplicaStamp{{Volume: "pvc-1", Name: "pvc-1-r-1", Node: "server-1", HealthyAt: "2026-10-24T08:00:00Z", LastFailedAt: "2026-10-24T09:00:00Z"}}}}
	rec := fencedWorker(NodeStateFenced, f)
	runner := &routedRunner{}
	require.NoError(t, SaveNodeRecord(runner, rec))
	require.Len(t, runner.stdins, 1)
	data := configMapDataFromPatch(t, runner.stdins[0])
	records, err := ParseNodeRecordData(data)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, rec, records[0])
}

func configMapDataFromPatch(t *testing.T, patch string) map[string]string {
	t.Helper()
	var p struct {
		Data map[string]string `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(patch), &p))
	return p.Data
}
