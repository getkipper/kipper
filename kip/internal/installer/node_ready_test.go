package installer

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The regression this exists for: `kubectl wait --all` does not wait when no
// node object exists yet. It returns "no matching resources found" at once, so
// a fresh k3s that has not registered its node failed the install in seconds
// despite a 120s timeout.
func TestWaitForNodeReadyRetriesWhileNoNodeExistsYet(t *testing.T) {
	calls := 0
	run := func(string) (string, error) {
		calls++
		if calls < 3 {
			return "", errors.New(`exit status 1: error: no matching resources found`)
		}
		return "", nil
	}

	if err := waitForNodeReady(run, 2*time.Second, time.Millisecond); err != nil {
		t.Fatalf("expected the wait to survive a node that has not registered yet, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected it to keep asking until a node appeared, got %d calls", calls)
	}
}

func TestWaitForNodeReadySucceedsImmediatelyWhenNodeIsReady(t *testing.T) {
	calls := 0
	run := func(string) (string, error) { calls++; return "", nil }

	if err := waitForNodeReady(run, time.Second, time.Millisecond); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("a ready node should be one call, got %d", calls)
	}
}

func TestWaitForNodeReadyGivesUpAtTheDeadline(t *testing.T) {
	run := func(string) (string, error) {
		return "", errors.New("exit status 1: error: no matching resources found")
	}

	err := waitForNodeReady(run, 20*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("expected an error once the deadline passed")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("error should say what failed, got %v", err)
	}
}

// A node that exists but stays NotReady is a different failure from one that
// never registers, and the deadline must still bound it.
func TestWaitForNodeReadyGivesUpOnANodeThatNeverBecomesReady(t *testing.T) {
	run := func(string) (string, error) {
		return "", errors.New(`timed out waiting for the condition on nodes/alex-test`)
	}

	if err := waitForNodeReady(run, 20*time.Millisecond, time.Millisecond); err == nil {
		t.Fatal("expected an error for a node that never becomes ready")
	}
}
