package controller

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

// autoSizerLease coordinates tuning across console-api pods during rollouts
// to avoid duplicate adjustments and alerts during normal operation.
const autoSizerLease = "kipper-resource-auto-sizer"

type leaseTiming struct {
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

var defaultLeaseTiming = leaseTiming{LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second}

// leaderWorker makes each election term wait until the previous term's
// worker has returned. client-go ends a term without waiting for its worker,
// so without this a pod that wins the Lease back could run two loops at once.
type leaderWorker struct {
	mu sync.Mutex
	// beforeLock, when set, is called as a term's worker starts waiting for
	// the previous one, so a test can tell that a term has arrived.
	beforeLock func()
}

func (w *leaderWorker) run(ctx context.Context, run func(context.Context)) {
	if w.beforeLock != nil {
		w.beforeLock()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	run(ctx)
}

// RunAsLeader runs the resource controller only while this pod holds the
// auto-sizer Lease. identity names this pod. It blocks until ctx is
// cancelled.
func (rc *ResourceController) RunAsLeader(ctx context.Context, identity string) {
	runAsLeader(ctx, rc.client, identity, defaultLeaseTiming, &rc.leader, rc.Run)
}

func runAsLeader(ctx context.Context, client kubernetes.Interface, identity string, timing leaseTiming, worker *leaderWorker, run func(context.Context)) {
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: autoSizerLease, Namespace: modeConfigMapNamespace},
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
			Name:          autoSizerLease,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(termCtx context.Context) { worker.run(termCtx, run) },
				OnStoppedLeading: func() { log.Printf("resource controller: %s no longer holds the auto-sizer lease", identity) },
			},
		})
	}
}
