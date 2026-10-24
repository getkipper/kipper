package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/getkipper/kipper/console-api/internal/leader"
)

// heldLease tracks this process's controller Lease. It timestamps successful
// acquisitions and renewals when their requests are sent, before other pods
// can observe them, to give the shutdown check a conservative deadline.
type heldLease struct {
	resourcelock.Interface
	timing leader.Timing

	mu      sync.Mutex
	renewed time.Time
}

// controllerLeaseIdentity combines the pod name and UID into a Lease holder
// identity. Container restarts retain it, allowing acquisition of the pod's
// unexpired Lease. A recreated pod has a new UID and must wait for release or
// expiry. An empty podUID generates a fresh UUID on each call, so a restarted
// process must also wait for release or expiry.
func controllerLeaseIdentity(podName, podUID string) string {
	if podUID == "" {
		podUID = string(uuid.NewUUID())
	}
	return podName + "_" + podUID
}

func controllerLease(cfg *rest.Config, identity string, timing leader.Timing) (*heldLease, error) {
	lock, err := resourcelock.NewFromKubeconfig(resourcelock.LeasesResourceLock, controllerLeaseNamespace, controllerLeaseName,
		resourcelock.ResourceLockConfig{Identity: identity}, cfg, timing.RenewDeadline)
	if err != nil {
		return nil, err
	}
	return &heldLease{Interface: lock, timing: timing}, nil
}

// Create acquires the Lease and records when the successful request was sent
// if this process is the holder. Lock errors are returned unchanged.
func (l *heldLease) Create(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	sent := time.Now()
	err := l.Interface.Create(ctx, record)
	l.note(record, sent, err)
	return err
}

// Update writes the Lease and records when a successful renewal was sent
// if this process remains the holder. Lock errors are returned unchanged.
func (l *heldLease) Update(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	sent := time.Now()
	err := l.Interface.Update(ctx, record)
	l.note(record, sent, err)
	return err
}

func (l *heldLease) note(record resourcelock.LeaderElectionRecord, sent time.Time, err error) {
	if err != nil || record.HolderIdentity != l.Identity() {
		return
	}
	l.mu.Lock()
	l.renewed = sent
	l.mu.Unlock()
}

// current reports whether the last successful acquisition or renewal was sent
// less than the renew deadline ago. This conservative check leaves time to
// exit before another pod can acquire the expired Lease.
func (l *heldLease) current() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return time.Since(l.renewed) < l.timing.RenewDeadline
}

// errLeaseNotRenewed reports a leader that went a whole renew deadline
// without renewing the controller Lease.
var errLeaseNotRenewed = errors.New("the controller Lease was not renewed in time")

// runControllerManager starts mgr and monitors its Lease, including during
// shutdown. It returns lost when an elected manager stops before ctx ends or
// its last successful renewal reaches the renew deadline. The caller must
// then exit immediately because leader-only work may still be running.
// After a shutdown drains the work while the Lease is current, it attempts
// release with a five-second timeout so the next pod can take over promptly.
// Release errors are logged rather than returned.
func runControllerManager(ctx context.Context, mgr ctrl.Manager, lock *heldLease) (lost bool, err error) {
	stopped := make(chan error, 1)
	go func() { stopped <- mgr.Start(ctx) }()
	check := time.NewTicker(time.Second)
	defer check.Stop()
	for {
		select {
		case err := <-stopped:
			if !elected(mgr) {
				return false, err
			}
			// Graceful shutdown drains leader-only work before stopping renewals.
			// A canceled ctx and a current Lease make early release safe.
			if ctx.Err() == nil || !lock.current() {
				return true, err
			}
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if releaseErr := releaseControllerLease(releaseCtx, lock); releaseErr != nil {
				log.Printf("the controller Lease was not released and expires within %s: %v", lock.timing.LeaseDuration, releaseErr)
			}
			cancel()
			return false, err
		case <-check.C:
			if elected(mgr) && !lock.current() {
				return true, fmt.Errorf("%w within %s", errLeaseNotRenewed, lock.timing.RenewDeadline)
			}
		}
	}
}

// releaseControllerLease clears lock's holder while this process holds it,
// the way client-go releases a Lease. The update is conditional on the Lease
// as read, so a pod that took it over in between keeps it.
func releaseControllerLease(ctx context.Context, lock resourcelock.Interface) error {
	record, _, err := lock.Get(ctx)
	if err != nil {
		return err
	}
	if record.HolderIdentity != lock.Identity() {
		return nil
	}
	now := metav1.NewTime(time.Now())
	return lock.Update(ctx, resourcelock.LeaderElectionRecord{
		LeaderTransitions: record.LeaderTransitions, LeaseDurationSeconds: 1, RenewTime: now, AcquireTime: now,
	})
}
