package cmd

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

type copiesStub struct {
	tagged *bool
	saved  *config.NodeStorage
}

func copiesCheck(runs []installer.FioSyncRun, gateErr error, ignore bool) (*nodeCopiesCheck, *copiesStub, *bytes.Buffer) {
	var out bytes.Buffer
	stub := &copiesStub{}
	rtt := installer.RTTSample{P99Ms: 0.8, MaxMs: 2, Sent: 200}
	tunnel := installer.RTTSample{P99Ms: 1.2, MaxMs: 1.2, Sent: 20}
	return &nodeCopiesCheck{
		node:           "worker-1",
		out:            &out,
		runGate:        func() ([]installer.FioSyncRun, error) { return runs, gateErr },
		measureRTT:     func() *installer.RTTSample { return &rtt },
		measureTunnel:  func() *installer.RTTSample { return &tunnel },
		engineTimeout:  installer.DefaultEngineReplicaTimeout,
		ignoreSlowDisk: ignore,
		setTag: func(on bool) error {
			stub.tagged = &on
			return nil
		},
		isTagged: func() (bool, error) { return false, nil },
		save: func(s config.NodeStorage) error {
			stub.saved = &s
			return nil
		},
		now: func() time.Time { return time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC) },
	}, stub, &out
}

var fastRuns = []installer.FioSyncRun{{P99Ms: 3.8, MaxMs: 21, IOBytes: 23066700}, {P99Ms: 4.4, MaxMs: 9, IOBytes: 23066700}}

func TestAFastDiskEarnsTheReplicaTag(t *testing.T) {
	check, stub, out := copiesCheck(fastRuns, nil, false)
	require.NoError(t, check.enable())
	require.NotNil(t, stub.tagged)
	assert.True(t, *stub.tagged)
	require.NotNil(t, stub.saved)
	assert.True(t, stub.saved.Copies)
	assert.InDelta(t, 4.4, stub.saved.P99Ms, 0.001)
	assert.Equal(t, "2026-10-24T06:00:00Z", stub.saved.CheckedAt)
	assert.Contains(t, out.String(), "worker-1 can hold copies")
}

func TestTheTunnelRoundTripIsRecordedAndPrinted(t *testing.T) {
	check, stub, out := copiesCheck(fastRuns, nil, false)
	require.NoError(t, check.enable())
	require.NotNil(t, stub.saved.TunnelRTTP99Ms)
	assert.InDelta(t, 1.2, *stub.saved.TunnelRTTP99Ms, 0.001)
	assert.Contains(t, out.String(), "the slowest of 20 pings to the server over the pod network took 1.2 ms")
}

func TestATunnelThatCannotBeMeasuredOnlyWarns(t *testing.T) {
	check, stub, out := copiesCheck(fastRuns, nil, false)
	check.measureTunnel = func() *installer.RTTSample { return nil }
	require.NoError(t, check.enable())
	assert.True(t, *stub.tagged, "the tunnel round trip never refuses a node")
	assert.Nil(t, stub.saved.TunnelRTTP99Ms)
	assert.Contains(t, out.String(), "The round trip to the server over the pod network could not be measured.")

	check, stub, out = copiesCheck(fastRuns, nil, false)
	check.measureTunnel = func() *installer.RTTSample { return &installer.RTTSample{P99Ms: 0.9, MaxMs: 0.9, Sent: 20, Lost: 3} }
	require.NoError(t, check.enable())
	assert.True(t, *stub.tagged)
	assert.Contains(t, out.String(), "3 of 20 pings to the server over the pod network got no reply.")

	check, stub, out = copiesCheck(fastRuns, nil, false)
	check.measureTunnel = func() *installer.RTTSample { return &installer.RTTSample{Sent: 20, Lost: 20} }
	require.NoError(t, check.enable())
	assert.Nil(t, stub.saved.TunnelRTTP99Ms, "no reply at all is no round trip")
	assert.Contains(t, out.String(), "None of 20 pings to the server over the pod network got a reply.")
}

func TestASlowDiskIsRefusedAndRecorded(t *testing.T) {
	slow := []installer.FioSyncRun{{P99Ms: 20.3, MaxMs: 848, IOBytes: 1000, CapHit: true}, {P99Ms: 14, MaxMs: 830, IOBytes: 23066700}}
	check, stub, _ := copiesCheck(slow, nil, false)
	err := check.enable()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--ignore-slow-disk")
	assert.Nil(t, stub.tagged, "a refused node is not tagged")
	require.NotNil(t, stub.saved, "the figures are recorded either way")
	assert.False(t, stub.saved.Copies)
	assert.True(t, stub.saved.CapHit)
}

func TestARefusedRecheckOfATaggedWorkerSaysItStillTakesCopies(t *testing.T) {
	slow := []installer.FioSyncRun{{P99Ms: 25, MaxMs: 900, IOBytes: 23066700}, {P99Ms: 22, MaxMs: 700, IOBytes: 23066700}}
	check, stub, _ := copiesCheck(slow, nil, false)
	check.isTagged = func() (bool, error) { return true, nil }
	err := check.enable()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-1 still takes copies from its earlier check")
	assert.Contains(t, err.Error(), "kip node storage worker-1 --copies=false")
	assert.Nil(t, stub.tagged, "the refusal changes no tag")
	assert.True(t, stub.saved.Copies, "the record says what Longhorn does: the worker still takes copies")
	assert.InDelta(t, 25, stub.saved.P99Ms, 0.001, "with the new figures")

	check, stub, _ = copiesCheck(slow, nil, false)
	check.isTagged = func() (bool, error) { return false, errors.New("connection refused") }
	err = check.enable()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not read whether worker-1 still takes copies")
	assert.False(t, stub.saved.Copies)
}

func TestARefusedRecheckKeepsTheOverrideTheTagRestsOn(t *testing.T) {
	slow := []installer.FioSyncRun{{P99Ms: 25, MaxMs: 900, IOBytes: 23066700}, {P99Ms: 22, MaxMs: 700, IOBytes: 23066700}}
	check, stub, _ := copiesCheck(slow, nil, false)
	check.isTagged = func() (bool, error) { return true, nil }
	check.previous = &config.NodeStorage{Copies: true, P99Ms: 18, Overridden: true, CheckedAt: "2026-10-01T06:00:00Z"}
	require.Error(t, check.enable())
	require.NotNil(t, stub.saved)
	assert.True(t, stub.saved.Overridden, "the tag still rests on the earlier override")
	assert.InDelta(t, 25, stub.saved.P99Ms, 0.001)

	check, stub, _ = copiesCheck(slow, nil, false)
	check.previous = &config.NodeStorage{Copies: true, Overridden: true}
	require.Error(t, check.enable())
	assert.False(t, stub.saved.Overridden, "an untagged node takes no copies, so no override is in force")
}

func TestTheSlowDiskOverrideIsRecorded(t *testing.T) {
	slow := []installer.FioSyncRun{{P99Ms: 25, MaxMs: 900, IOBytes: 23066700}, {P99Ms: 22, MaxMs: 700, IOBytes: 23066700}}
	check, stub, _ := copiesCheck(slow, nil, true)
	require.NoError(t, check.enable())
	assert.True(t, *stub.tagged)
	assert.True(t, stub.saved.Overridden)
}

func TestAStallNearTheEngineTimeoutCannotBeOverridden(t *testing.T) {
	stall := []installer.FioSyncRun{{P99Ms: 5, MaxMs: 4500, IOBytes: 23066700}, {P99Ms: 5, MaxMs: 10, IOBytes: 23066700}}
	check, stub, _ := copiesCheck(stall, nil, true)
	err := check.enable()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "--ignore-slow-disk")
	assert.Nil(t, stub.tagged)
}

func TestADiskCheckThatCannotRunTagsNothing(t *testing.T) {
	check, stub, _ := copiesCheck(nil, errors.New("fio: command not found"), true)
	err := check.enable()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fio: command not found")
	assert.Nil(t, stub.tagged)
}

func TestRemovingTheTagNeedsANodeWithoutCopies(t *testing.T) {
	var untagged bool
	err := disableCopies(&bytes.Buffer{}, "worker-1", func() (int, error) { return 3, nil }, func(on bool) error {
		untagged = !on
		return nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "3")
	assert.False(t, untagged)

	require.NoError(t, disableCopies(&bytes.Buffer{}, "worker-1", func() (int, error) { return 0, nil }, func(on bool) error {
		untagged = !on
		return nil
	}))
	assert.True(t, untagged)

	err = disableCopies(&bytes.Buffer{}, "worker-1", func() (int, error) { return 0, errors.New("unreachable") }, func(bool) error { return nil })
	assert.Error(t, err, "an unknown replica count never removes the tag")
}

type inventoryRunner struct{ replicas, nodes, objects string }

func (r inventoryRunner) Run(cmd string) (string, error) {
	switch {
	case strings.Contains(cmd, "get replicas.longhorn.io"):
		return r.replicas, nil
	case strings.Contains(cmd, "get nodes.longhorn.io"):
		return r.nodes, nil
	case strings.Contains(cmd, "services.kipper.run"):
		return r.objects, nil
	}
	return "", errors.New("unexpected command " + cmd)
}

func (r inventoryRunner) RunStdin(cmd string, _ io.Reader) (string, error) { return r.Run(cmd) }

func TestUpgradeNamesTheDiskCheckForEachWorkerHoldingCopies(t *testing.T) {
	var out bytes.Buffer
	reportCopiesForUpgrade(&out, inventoryRunner{
		replicas: "worker-1\t2026-10-01T00:00:00Z\t\n",
		nodes:    "server-1\t[\"kipper-server\",\"kipper-replica\"]\nworker-1\t\n",
		objects:  "Service\tshop\tdb\t\n",
	})
	assert.Contains(t, out.String(), "kip node storage worker-1 --copies")
	assert.Contains(t, out.String(), "1 service or volume has copies unset")

	out.Reset()
	reportCopiesForUpgrade(&out, inventoryRunner{nodes: "server-1\t[\"kipper-replica\"]\n", objects: "Service\tshop\tdb\t1\n"})
	assert.Empty(t, out.String())
}
