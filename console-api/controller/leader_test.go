package controller

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var testLease = leaseTiming{LeaseDuration: 2 * time.Second, RenewDeadline: time.Second, RetryPeriod: 200 * time.Millisecond}

// activity records how many workers are active at once, and how many terms
// each identity has started.
type activity struct {
	active, maxActive atomic.Int32
	mu                sync.Mutex
	terms             map[string]int
	// cleanup is how long a worker keeps running after its context ends.
	cleanup time.Duration
}

func newActivity(cleanup time.Duration) *activity {
	return &activity{terms: map[string]int{}, cleanup: cleanup}
}

func (a *activity) run(identity string) func(context.Context) {
	return func(ctx context.Context) {
		n := a.active.Add(1)
		for {
			old := a.maxActive.Load()
			if n <= old || a.maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		a.mu.Lock()
		a.terms[identity]++
		a.mu.Unlock()
		<-ctx.Done()
		time.Sleep(a.cleanup)
		a.active.Add(-1)
	}
}

func (a *activity) termsOf(identity string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.terms[identity]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func startElector(t *testing.T, client kubernetes.Interface, identity string, worker *leaderWorker, a *activity) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runAsLeader(ctx, client, identity, testLease, worker, a.run(identity))
		close(done)
	}()
	return func() { cancel(); <-done }
}

func TestAutoSizerRunsInOnePodAndHandsOver(t *testing.T) {
	client := fake.NewClientset()
	a := newActivity(0)

	stopA := startElector(t, client, "pod-a", &leaderWorker{}, a)
	waitFor(t, "pod-a to lead", func() bool { return a.termsOf("pod-a") == 1 })
	stopB := startElector(t, client, "pod-b", &leaderWorker{}, a)
	defer stopB()

	// pod-a stops; pod-b takes over once the Lease expires.
	stopA()
	waitFor(t, "pod-b to take over", func() bool { return a.termsOf("pod-b") == 1 })

	if got := a.maxActive.Load(); got != 1 {
		t.Fatalf("%d auto-sizers ran at once, want 1", got)
	}
}

// A pod that loses the Lease and wins it back must not start a second worker
// while the first is still finishing its tick.
func TestAutoSizerWaitsForTheLastTermBeforeStartingAgain(t *testing.T) {
	client := fake.NewClientset()
	// Failing every Lease update makes renewal fail, which ends the term. The
	// fake does not enforce resourceVersion conflicts, so taking the Lease
	// over through the API would not end it.
	var renewalsFail atomic.Bool
	client.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if renewalsFail.Load() {
			return true, nil, errors.New("renewal refused by the test")
		}
		return false, nil, nil
	})

	var arrived, terms, active, maxActive atomic.Int32
	firstCancelled := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer release()

	worker := &leaderWorker{beforeLock: func() { arrived.Add(1) }}
	run := func(ctx context.Context) {
		term := terms.Add(1)
		if n := active.Add(1); n > maxActive.Load() {
			maxActive.Store(n)
		}
		defer active.Add(-1)
		<-ctx.Done()
		if term == 1 {
			close(firstCancelled)
			<-releaseFirst
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runAsLeader(ctx, client, "pod-a", testLease, worker, run); close(done) }()
	defer func() { release(); cancel(); <-done }()

	waitFor(t, "the first term", func() bool { return terms.Load() == 1 })

	renewalsFail.Store(true)
	select {
	case <-firstCancelled:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the first term to end")
	}
	renewalsFail.Store(false)

	// The second term has won the Lease back and is waiting for the first
	// worker, which is still held in its cleanup.
	waitFor(t, "the second term to arrive", func() bool { return arrived.Load() == 2 })
	time.Sleep(200 * time.Millisecond)
	if got := terms.Load(); got != 1 {
		t.Fatalf("second worker started while the first was still running (terms = %d)", got)
	}

	release()
	waitFor(t, "the second term to start", func() bool { return terms.Load() == 2 })
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("%d workers ran at once in one pod, want 1", got)
	}
}
