// Package leader runs a loop in one console-api pod at a time, by holding a
// Lease.
package leader

import (
	"context"
	"log"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Timing is how a Lease is held and renewed.
type Timing struct {
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

// DefaultTiming suits a loop that must run in one console-api pod at a time.
var DefaultTiming = Timing{LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second}

// Worker makes each election term wait until the previous term's worker has
// returned. client-go ends a term without waiting for its worker, so without
// this a pod that wins the Lease back could run two loops at once.
type Worker struct {
	mu sync.Mutex
	// BeforeLock, when set, is called as a term's worker starts waiting for
	// the previous one, so a test can tell that a term has arrived.
	BeforeLock func()
}

func (w *Worker) run(ctx context.Context, run func(context.Context)) {
	if w.BeforeLock != nil {
		w.BeforeLock()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	run(ctx)
}

// Run starts a worker on acquiring the Lease namespace/name and cancels its
// context on leadership loss, then rejoins the election. The worker must honor
// cancellation; Worker serializes terms within this process. identity names
// this pod. Run blocks until ctx is cancelled.
func Run(ctx context.Context, client kubernetes.Interface, namespace, name, identity string, timing Timing, worker *Worker, run func(context.Context)) {
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: name, Namespace: namespace},
		Client:     client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
	}
	// Re-enter the election after losing leadership. Let the Lease expire:
	// client-go can release it before the previous worker has stopped.
	for ctx.Err() == nil {
		leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
			Lock:          lock,
			LeaseDuration: timing.LeaseDuration,
			RenewDeadline: timing.RenewDeadline,
			RetryPeriod:   timing.RetryPeriod,
			Name:          name,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(termCtx context.Context) { worker.run(termCtx, run) },
				OnStoppedLeading: func() { log.Printf("%s no longer holds the %s lease", identity, name) },
			},
		})
	}
}
