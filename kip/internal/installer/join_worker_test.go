package installer

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWaitForWorkerJoinedSucceedsWhenNodeIsPresent(t *testing.T) {
	calls := 0
	run := func(cmd string) (string, error) {
		calls++
		if !strings.Contains(cmd, "kubectl get node") {
			t.Fatalf("expected kubectl get node, got %q", cmd)
		}
		if !strings.Contains(cmd, "'worker-1'") {
			t.Fatalf("expected shell-quoted node name, got %q", cmd)
		}
		return "worker-1   Ready   <none>   10s   v1.36.2+k3s1\n", nil
	}

	if err := waitForWorkerJoined(run, "worker-1", time.Second, time.Millisecond); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("a present node should be one call, got %d", calls)
	}
}

func TestWaitForWorkerJoinedFailsWhenKubectlFails(t *testing.T) {
	run := func(string) (string, error) {
		return "", errors.New("exit status 1: The connection to the server was refused")
	}

	err := waitForWorkerJoined(run, "worker-1", 20*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("expected an error when kubectl fails")
	}
	if !strings.Contains(err.Error(), "verifying worker node joined") {
		t.Fatalf("error should identify the check, got %v", err)
	}
	if !strings.Contains(err.Error(), "connection to the server was refused") {
		t.Fatalf("error should wrap the kubectl failure, got %v", err)
	}
}

func TestWaitForWorkerJoinedFailsWhenWorkerIsAbsent(t *testing.T) {
	run := func(string) (string, error) {
		return "", errors.New(`exit status 1: Error from server (NotFound): nodes "worker-1" not found`)
	}

	err := waitForWorkerJoined(run, "worker-1", 20*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("expected an error when the worker is absent from the node list")
	}
	if !strings.Contains(err.Error(), "verifying worker node joined") {
		t.Fatalf("error should identify the check, got %v", err)
	}
}

func TestWaitForWorkerJoinedRetriesUntilNodeAppears(t *testing.T) {
	calls := 0
	run := func(string) (string, error) {
		calls++
		if calls < 3 {
			return "", errors.New(`exit status 1: Error from server (NotFound): nodes "worker-1" not found`)
		}
		return "worker-1   Ready   <none>   1s   v1.36.2+k3s1\n", nil
	}

	if err := waitForWorkerJoined(run, "worker-1", 2*time.Second, time.Millisecond); err != nil {
		t.Fatalf("expected the wait to survive delayed registration, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected it to keep asking until the node appeared, got %d calls", calls)
	}
}

func TestWaitForWorkerJoinedRejectsEmptyNodeName(t *testing.T) {
	run := func(string) (string, error) {
		t.Fatal("should not call kubectl with an empty node name")
		return "", nil
	}
	err := waitForWorkerJoined(run, "", time.Second, time.Millisecond)
	if err == nil {
		t.Fatal("expected an error for an empty node name")
	}
	if !strings.Contains(err.Error(), "empty node name") {
		t.Fatalf("error should mention empty node name, got %v", err)
	}
}

func TestWaitForWorkerJoinedQuotesNodeNameForShell(t *testing.T) {
	run := func(cmd string) (string, error) {
		// A hostname with a quote must still be one shell word.
		if cmd != "kubectl get node 'weird'\\''name' --no-headers" {
			t.Fatalf("unexpected command: %q", cmd)
		}
		return "weird'name   Ready\n", nil
	}
	if err := waitForWorkerJoined(run, "weird'name", time.Second, time.Millisecond); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
