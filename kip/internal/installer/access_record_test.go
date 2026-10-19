package installer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func accessRecordJSON(t *testing.T, rec AccessRecord) string {
	t.Helper()
	b, err := json.Marshal(rec)
	require.NoError(t, err)
	data, err := json.Marshal(map[string]string{accessRecordKey: string(b)})
	require.NoError(t, err)
	return string(data)
}

func sampleAccessRecord() AccessRecord {
	return AccessRecord{Version: 1, Generation: 7, Sources: []AccessSource{
		{Prefix: "203.0.113.0/24", Note: "office", AddedBy: "operator@198.51.100.7", AddedAt: "2026-10-19T08:00:00Z"},
		{Prefix: "2001:db8::/48"},
	}}
}

func TestReadAccessRecordWithoutAConfigMapIsAbsent(t *testing.T) {
	rec, present, err := ReadAccessRecord(&routedRunner{})
	require.NoError(t, err)
	assert.False(t, present)
	assert.Equal(t, AccessRecord{}, rec)
}

func TestReadAccessRecordReadsTheRecord(t *testing.T) {
	r := &routedRunner{routes: []route{{contains: "get configmap kipper-access", out: accessRecordJSON(t, sampleAccessRecord())}}}
	rec, present, err := ReadAccessRecord(r)
	require.NoError(t, err)
	assert.True(t, present)
	assert.Equal(t, sampleAccessRecord(), rec)
	ps, err := rec.Prefixes()
	require.NoError(t, err)
	assert.Equal(t, prefixes(t, "203.0.113.0/24", "2001:db8::/48"), ps)
}

func TestReadAccessRecordThatCannotBeReadIsAnError(t *testing.T) {
	r := &routedRunner{routes: []route{{contains: "get configmap kipper-access", err: errors.New("connection refused")}}}
	_, _, err := ReadAccessRecord(r)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrInvalidAccessRecord), "unreadable is not invalid")
}

func TestReadAccessRecordRefusesAnInvalidRecord(t *testing.T) {
	empty := sampleAccessRecord()
	empty.Sources = nil
	version := sampleAccessRecord()
	version.Version = 2
	bad := sampleAccessRecord()
	bad.Sources[0].Prefix = "203.0.113.7/24"
	dup := sampleAccessRecord()
	dup.Sources = append(dup.Sources, AccessSource{Prefix: "203.0.113.0/24"})
	for name, out := range map[string]string{
		"bad JSON":          `{"record":"{not json"}`,
		"no record key":     `{"other":"x"}`,
		"not a map":         `[1,2]`,
		"empty sources":     accessRecordJSON(t, empty),
		"unknown version":   accessRecordJSON(t, version),
		"non-canonical":     accessRecordJSON(t, bad),
		"duplicate sources": accessRecordJSON(t, dup),
	} {
		r := &routedRunner{routes: []route{{contains: "get configmap kipper-access", out: out}}}
		_, present, err := ReadAccessRecord(r)
		require.Error(t, err, name)
		assert.True(t, errors.Is(err, ErrInvalidAccessRecord), name)
		assert.True(t, present, "%s: an invalid record is still a present one", name)
	}
}

func TestWriteAccessRecordPatchesTheConfigMap(t *testing.T) {
	r := &routedRunner{}
	require.NoError(t, WriteAccessRecord(r, sampleAccessRecord()))
	require.Len(t, r.stdins, 1)
	assert.True(t, r.ran("create configmap kipper-access"))
	assert.True(t, r.ran("patch configmap kipper-access"))
	var patch struct {
		Data map[string]string `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(r.stdins[0]), &patch))
	var rec AccessRecord
	require.NoError(t, json.Unmarshal([]byte(patch.Data[accessRecordKey]), &rec))
	assert.Equal(t, sampleAccessRecord(), rec)
}

func TestWriteAccessRecordKeepsTheLastHundredChanges(t *testing.T) {
	rec := sampleAccessRecord()
	for i := 0; i < 130; i++ {
		rec.History = append(rec.History, AccessChange{At: fmt.Sprintf("t%d", i), Action: "allow", Generation: i})
	}
	r := &routedRunner{}
	require.NoError(t, WriteAccessRecord(r, rec))
	var patch struct {
		Data map[string]string `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(r.stdins[0]), &patch))
	var saved AccessRecord
	require.NoError(t, json.Unmarshal([]byte(patch.Data[accessRecordKey]), &saved))
	require.Len(t, saved.History, accessHistoryLimit)
	assert.Equal(t, "t30", saved.History[0].At)
	assert.Equal(t, "t129", saved.History[accessHistoryLimit-1].At)
}

func TestWriteAccessRecordRefusesAnInvalidRecord(t *testing.T) {
	rec := sampleAccessRecord()
	rec.Sources = nil
	assert.Error(t, WriteAccessRecord(&routedRunner{}, rec))
}

func TestDeleteAccessRecord(t *testing.T) {
	r := &routedRunner{}
	require.NoError(t, DeleteAccessRecord(r))
	assert.True(t, r.ran("delete configmap kipper-access --ignore-not-found"))
}

func TestNextAccessGenerationIsAboveTheRecordAndEveryHeader(t *testing.T) {
	assert.Equal(t, 8, NextAccessGeneration(7, nil))
	assert.Equal(t, 10, NextAccessGeneration(7, []AccessNodeHeader{{Name: "server", Generation: 9, Present: true}, {Name: "worker-1"}}))
}

func TestStaleAccessRecordRefusesWhenANodeIsAhead(t *testing.T) {
	err := StaleAccessRecord(7, []AccessNodeHeader{{Name: "server", Generation: 7, Present: true}, {Name: "worker-1", Generation: 8, Present: true}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the record (generation 7) is older than worker-1 (generation 8)")
	assert.Contains(t, err.Error(), "kip cluster access apply --accept-older-record")

	assert.NoError(t, StaleAccessRecord(7, []AccessNodeHeader{{Name: "server", Generation: 7, Present: true}, {Name: "worker-1"}}), "equal or missing is not ahead")
}

func TestAbsentRecordWithALiveTableIsRefused(t *testing.T) {
	err := OrphanedAccessTable([]AccessNodeHeader{{Name: "server"}, {Name: "worker-1", Generation: 3, Present: true, Sources: 2}})
	require.Error(t, err, "a table on a worker alone counts")
	assert.Contains(t, err.Error(), "the operator access record is gone but worker-1 still restricts SSH and 6443 to 2 sources")
	assert.Contains(t, err.Error(), "kip cluster access reset")
	assert.NoError(t, OrphanedAccessTable([]AccessNodeHeader{{Name: "server"}, {Name: "worker-1"}}))
}

func TestAccessWideningCountsAClosedOrMissingTableAsAdmittingNothing(t *testing.T) {
	record := prefixes(t, "203.0.113.0/24", "198.51.100.7/32")
	got := AccessWidening(record, map[string][]netip.Prefix{
		"server":   prefixes(t, "203.0.113.0/24"),
		"worker-1": nil,
		"worker-2": prefixes(t, "203.0.113.0/25", "203.0.113.128/25", "198.51.100.7/32"),
	})
	assert.Equal(t, map[string][]string{
		"server":   {"198.51.100.7/32"},
		"worker-1": {"198.51.100.7/32", "203.0.113.0/24"},
	}, got)
}
