package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tick pairs alert entries with the cooldown marks that must not be
// committed until the alert has actually been stored. That pairing used to be
// positional: two parallel slices, sliced by the same overflow count when a
// tick exceeded the per-write cap. It held only while every problem produced
// exactly one entry and exactly one mark.
//
// It stops holding the moment a problem needs two marks (escalating a crash
// loop stamps both the cooldown and the escalation) or none (an all-clear
// records nothing). Then the overflow slice either panics or commits marks
// belonging to entries that were dropped, suppressing an alert nobody saw.

func TestAlertBatch_KeepsMarksWithTheirEntryUnderTheCap(t *testing.T) {
	at := time.Unix(1_780_000_000, 0).UTC()
	dstA := map[string]time.Time{}
	dstB := map[string]time.Time{}

	// Three problems: one with no mark, one with one, one with two.
	batches := []alertBatch{
		{entry: ResourceLogEntry{Action: "oldest, dropped first"}},
		{entry: ResourceLogEntry{Action: "one mark"}, marks: []pendingMark{{dst: dstA, key: "a", at: at}}},
		{entry: ResourceLogEntry{Action: "two marks"}, marks: []pendingMark{
			{dst: dstA, key: "b", at: at},
			{dst: dstB, key: "b", at: at},
		}},
	}

	kept, dropped := capBatches(batches, 2)

	require.Len(t, kept, 2, "the cap keeps the newest")
	assert.Equal(t, 1, dropped)
	assert.Equal(t, "one mark", kept[0].entry.Action, "the oldest entry is the one dropped")
	assert.Equal(t, "two marks", kept[1].entry.Action)

	// The dropped entry took its marks with it, and no surviving entry lost any.
	total := 0
	for _, b := range kept {
		total += len(b.marks)
	}
	assert.Equal(t, 3, total, "marks travel with their entry rather than by position")
}

func TestAlertBatch_UnderTheCapIsUntouched(t *testing.T) {
	batches := []alertBatch{{entry: ResourceLogEntry{Action: "only one"}}}
	kept, dropped := capBatches(batches, 25)
	assert.Equal(t, 0, dropped)
	require.Len(t, kept, 1)
}

// An entry carrying no mark must not shift the pairing of any other entry.
// Under the old parallel-slice contract this is the case that misaligned.
func TestAlertBatch_AnEntryWithNoMarkDoesNotShiftTheRest(t *testing.T) {
	at := time.Unix(1_780_000_000, 0).UTC()
	dst := map[string]time.Time{}
	batches := []alertBatch{
		{entry: ResourceLogEntry{Action: "dropped"}, marks: []pendingMark{{dst: dst, key: "dropped", at: at}}},
		{entry: ResourceLogEntry{Action: "all clear, no mark"}},
		{entry: ResourceLogEntry{Action: "kept"}, marks: []pendingMark{{dst: dst, key: "kept", at: at}}},
	}

	kept, _ := capBatches(batches, 2)

	rc := &ResourceController{}
	rc.commitBatches(kept)

	_, droppedCommitted := dst["dropped"]
	assert.False(t, droppedCommitted, "a dropped entry's mark must never be committed: the alert was never stored")
	_, keptCommitted := dst["kept"]
	assert.True(t, keptCommitted, "a surviving entry's mark must be committed")
}

// A mark may also remove state rather than stamp it, which is how an episode
// is forgotten once its service recovers. Deletion has to be deferred for the
// same reason a stamp is: if the all-clear never reaches the store, the episode
// must survive so the next tick can try again.
func TestAlertBatch_CommitsDeletions(t *testing.T) {
	at := time.Unix(1_780_000_000, 0).UTC()
	dst := map[string]time.Time{"gone": at, "stays": at}

	rc := &ResourceController{}
	rc.commitBatches([]alertBatch{
		{entry: ResourceLogEntry{Action: "recovered"}, marks: []pendingMark{{dst: dst, key: "gone", del: true}}},
	})

	_, present := dst["gone"]
	assert.False(t, present, "a deletion mark must remove the key")
	_, stays := dst["stays"]
	assert.True(t, stays, "and must touch nothing else")
}
