package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
	"github.com/getkipper/kipper/console-api/internal/apiservertest"
	"github.com/getkipper/kipper/console-api/internal/leader"
)

// electionLog records when each manager's leader-only work started and when
// its in-flight work finished.
type electionLog struct {
	mu       sync.Mutex
	started  map[string]time.Time
	finished map[string]time.Time
}

func (l *electionLog) startedAt(name string) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.started[name]
	return t, ok
}

// leaderOnlyWork is a runnable that, like a reconciler, needs leadership. It
// keeps working for three seconds after its context ends, as a reconcile in flight
// would.
func (l *electionLog) leaderOnlyWork(name string) manager.RunnableFunc {
	return func(ctx context.Context) error {
		l.mu.Lock()
		l.started[name] = time.Now()
		l.mu.Unlock()
		<-ctx.Done()
		time.Sleep(3 * time.Second)
		l.mu.Lock()
		l.finished[name] = time.Now()
		l.mu.Unlock()
		return nil
	}
}

// Two console-api pods overlap during a rolling update. Only the elected one
// runs reconcilers, and a pod shutting down hands over only once its in-flight
// work has finished, and without waiting for the lease to expire.
func TestOnlyTheElectedConsoleAPIRunsReconcilers(t *testing.T) {
	cfg := apiservertest.Start(t)
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	c, err := crclient.New(cfg, crclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: controllerLeaseNamespace}}); err != nil {
		t.Fatal(err)
	}

	events := &electionLog{started: map[string]time.Time{}, finished: map[string]time.Time{}}
	start := func(name string) (stop func()) {
		m := newConsoleAPIManager(t, cfg, scheme, name, controllerLeaseTiming, nil)
		if err := m.mgr.Add(events.leaderOnlyWork(name)); err != nil {
			t.Fatal(err)
		}
		m.start(t)
		return m.stop
	}
	waitStarted := func(name string, within time.Duration) time.Time {
		deadline := time.Now().Add(within)
		for time.Now().Before(deadline) {
			if at, ok := events.startedAt(name); ok {
				return at
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("%s never started its leader-only work", name)
		return time.Time{}
	}

	stopOld := start("old")
	waitStarted("old", 30*time.Second)
	var lease coordinationv1.Lease
	if err := c.Get(context.Background(), crclient.ObjectKey{Namespace: controllerLeaseNamespace, Name: controllerLeaseName}, &lease); err != nil {
		t.Fatal(err)
	}
	if d := lease.Spec.LeaseDurationSeconds; d == nil || time.Duration(*d)*time.Second != controllerLeaseTiming.LeaseDuration {
		t.Fatalf("the Lease is not held for %v: %v", controllerLeaseTiming.LeaseDuration, lease.Spec)
	}
	stopNew := start("new")
	defer stopNew()
	time.Sleep(4 * time.Second)
	if _, ok := events.startedAt("new"); ok {
		t.Fatal("the second pod ran leader-only work while the first one leads")
	}

	stoppedAt := time.Now()
	stopOld()
	newStarted := waitStarted("new", 30*time.Second)
	events.mu.Lock()
	oldFinished := events.finished["old"]
	events.mu.Unlock()
	if oldFinished.IsZero() || newStarted.Before(oldFinished) {
		t.Fatalf("the new leader started at %v before the old one's in-flight work finished at %v", newStarted, oldFinished)
	}
	if handover := newStarted.Sub(stoppedAt); handover > 10*time.Second {
		t.Fatalf("the handover took %v, so the lease was left to expire rather than released", handover)
	}
}

// consoleAPIManager is one console-api process's controller manager, run the
// way startControllerManager runs it.
type consoleAPIManager struct {
	name   string
	mgr    ctrl.Manager
	lock   *heldLease
	cancel context.CancelFunc
	done   chan struct{}
	stop   func()
	// Read lost and returnedAt only after done is closed.
	lost       bool
	returnedAt time.Time
}

// newConsoleAPIManager builds the manager with the production options and
// the given Lease timing. wrap, when set, wraps the Lease lock's requests.
func newConsoleAPIManager(t *testing.T, cfg *rest.Config, scheme *runtime.Scheme, name string, timing leader.Timing, wrap func(resourcelock.Interface) resourcelock.Interface) *consoleAPIManager {
	t.Helper()
	lock, err := controllerLease(cfg, name, timing)
	if err != nil {
		t.Fatal(err)
	}
	if wrap != nil {
		lock.Interface = wrap(lock.Interface)
	}
	opts := controllerManagerOptions(scheme, lock)
	opts.Metrics.BindAddress = "0"
	mgr, err := ctrl.NewManager(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	return &consoleAPIManager{name: name, mgr: mgr, lock: lock, done: make(chan struct{})}
}

func (m *consoleAPIManager) start(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go func() {
		defer close(m.done)
		lost, err := runControllerManager(ctx, m.mgr, m.lock)
		m.lost, m.returnedAt = lost, time.Now()
		if err != nil && !lost {
			t.Errorf("manager %s: %v", m.name, err)
		}
	}()
	var once sync.Once
	m.stop = func() { once.Do(func() { cancel(); <-m.done }) }
	t.Cleanup(m.stop)
}

// consoleAPIPod creates a running console-api pod in kipper-system with the
// labels and image status the route-name sweeper reads.
func consoleAPIPod(t *testing.T, c crclient.Client, name string) *corev1.Pod {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: controllerLeaseNamespace, Labels: map[string]string{"app": "console-api"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "console-api", Image: "console-api:test"}}},
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "console-api", Image: "console-api:test", ImageID: "console-api@sha256:test", Ready: true,
	}}}
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	return pod
}

// routeNamePod is one console-api process: a manager wired the way
// startControllerManager wires it, with its own route-name gate.
type routeNamePod struct {
	mgr  ctrl.Manager
	gate *controllers.RouteNameGate
	stop func()
}

func (p *routeNamePod) admits() bool {
	release, ok := p.gate.Admit()
	if ok {
		release()
	}
	return ok
}

func startRouteNamePod(t *testing.T, cfg *rest.Config, scheme *runtime.Scheme, name string) *routeNamePod {
	t.Helper()
	m := newConsoleAPIManager(t, cfg, scheme, name, controllerLeaseTiming, nil)
	p := &routeNamePod{mgr: m.mgr, gate: controllers.NewRouteNameGate()}
	if err := addRouteNameSweeper(m.mgr, name, p.gate); err != nil {
		t.Fatal(err)
	}
	m.start(t)
	p.stop = m.stop
	return p
}

// eventually waits up to 30 seconds for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	const within = 30 * time.Second
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within %v", what, within)
}

// TestTheReconcilingConsoleAPIAdmitsRoutes checks that the controller Lease
// controls route admission even when another pod holds the former sweeper
// Lease. Only the leader opens its gate. A new leader waits for pods running
// another binary, but can admit routes alongside an older pod of the same
// binary. This covers a former leader whose container restarts as a follower.
func TestTheReconcilingConsoleAPIAdmitsRoutes(t *testing.T) {
	cfg := apiservertest.Start(t)
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kipperv1.AddToScheme(scheme))
	c, err := crclient.New(cfg, crclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: controllerLeaseNamespace}}); err != nil {
		t.Fatal(err)
	}
	older := consoleAPIPod(t, c, "console-api-older")
	time.Sleep(1100 * time.Millisecond)
	consoleAPIPod(t, c, "console-api-newer")
	// A leftover sweeper Lease must not block the controller leader's gate.
	holder, duration, now := "console-api-newer", int32(3600), metav1.NewMicroTime(time.Now())
	if err := c.Create(ctx, &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-route-names", Namespace: controllerLeaseNamespace},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &duration, AcquireTime: &now, RenewTime: &now},
	}); err != nil {
		t.Fatal(err)
	}

	leader := startRouteNamePod(t, cfg, scheme, "console-api-older")
	eventually(t, "the older pod winning the controller Lease", func() bool { return elected(leader.mgr) })
	eventually(t, "the reconciling pod admitting routes", leader.admits)

	follower := startRouteNamePod(t, cfg, scheme, "console-api-newer")
	time.Sleep(4 * time.Second)
	if elected(follower.mgr) || follower.admits() {
		t.Fatal("a pod that does not reconcile opened its route-name gate")
	}

	other := consoleAPIPod(t, c, "console-api-other")
	other.Status.ContainerStatuses[0].ImageID = "console-api@sha256:other"
	if err := c.Status().Update(ctx, other); err != nil {
		t.Fatal(err)
	}
	leader.stop()
	eventually(t, "the newer pod taking the controller Lease over", func() bool { return elected(follower.mgr) })
	time.Sleep(4 * time.Second)
	if follower.admits() {
		t.Fatal("the new leader admitted routes while a pod of another binary was live")
	}
	if err := c.Delete(ctx, other, crclient.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the new leader admitting routes beside the earlier pod", follower.admits)
	if err := c.Get(ctx, crclient.ObjectKeyFromObject(older), &corev1.Pod{}); err != nil {
		t.Fatalf("the earlier pod must still exist: %v", err)
	}
}

// cutOffLock is a Lease lock whose requests stall, as they do for a pod cut
// off from the API server, once cut is set.
type cutOffLock struct {
	resourcelock.Interface
	cut atomic.Bool
}

func (l *cutOffLock) stall(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (l *cutOffLock) Get(ctx context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	if l.cut.Load() {
		return nil, nil, l.stall(ctx)
	}
	return l.Interface.Get(ctx)
}

func (l *cutOffLock) Create(ctx context.Context, ler resourcelock.LeaderElectionRecord) error {
	if l.cut.Load() {
		return l.stall(ctx)
	}
	return l.Interface.Create(ctx, ler)
}

func (l *cutOffLock) Update(ctx context.Context, ler resourcelock.LeaderElectionRecord) error {
	if l.cut.Load() {
		return l.stall(ctx)
	}
	return l.Interface.Update(ctx, ler)
}

// shortLeaseTiming keeps the order of the production timings: the renew
// deadline plus a retry period ends before the Lease expires.
var shortLeaseTiming = leader.Timing{LeaseDuration: 7 * time.Second, RenewDeadline: 4 * time.Second, RetryPeriod: time.Second}

// TestACutOffLeaderStopsBeforeTheLeaseExpires checks that a cut-off leader
// returns lost before its successor starts, requiring the caller to exit to
// prevent concurrent reconciliation. It covers normal operation and shutdown
// with stuck leader-only work, which must finish before the Lease is released.
func TestACutOffLeaderStopsBeforeTheLeaseExpires(t *testing.T) {
	for _, shuttingDown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutting down %t", shuttingDown), func(t *testing.T) {
			cfg := apiservertest.Start(t)
			scheme := runtime.NewScheme()
			utilruntime.Must(clientgoscheme.AddToScheme(scheme))
			c, err := crclient.New(cfg, crclient.Options{Scheme: scheme})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: controllerLeaseNamespace}}); err != nil {
				t.Fatal(err)
			}
			lock := &cutOffLock{}
			old := newConsoleAPIManager(t, cfg, scheme, "console-api-old", shortLeaseTiming, func(inner resourcelock.Interface) resourcelock.Interface {
				lock.Interface = inner
				return lock
			})
			stuck := make(chan struct{})
			if err := old.mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
				<-ctx.Done()
				<-stuck
				return nil
			})); err != nil {
				t.Fatal(err)
			}
			old.start(t)
			t.Cleanup(func() { close(stuck) })
			eventually(t, "the first pod winning the controller Lease", func() bool { return elected(old.mgr) })
			events := &electionLog{started: map[string]time.Time{}, finished: map[string]time.Time{}}
			successor := newConsoleAPIManager(t, cfg, scheme, "console-api-new", shortLeaseTiming, nil)
			if err := successor.mgr.Add(events.leaderOnlyWork("new")); err != nil {
				t.Fatal(err)
			}
			successor.start(t)
			time.Sleep(2 * time.Second)

			lock.cut.Store(true)
			if shuttingDown {
				old.cancel()
			}
			eventually(t, "the successor taking the controller Lease over", func() bool {
				_, ok := events.startedAt("new")
				return ok
			})
			newStarted, _ := events.startedAt("new")
			select {
			case <-old.done:
			default:
				t.Fatal("the cut-off leader was still running when the successor took the Lease over")
			}
			if !old.returnedAt.Before(newStarted) {
				t.Fatalf("the cut-off leader stopped at %v, after the successor started at %v", old.returnedAt, newStarted)
			}
			if !old.lost {
				t.Fatal("the cut-off leader did not report the lost Lease, so the process would not exit")
			}
		})
	}
}

// A container restarts only after its process has exited and keeps its pod's
// UID, so the restarted process can acquire its own pod's unexpired Lease.
// A recreated pod with the same name has a new UID and waits for release or expiry.
func TestARestartedContainerTakesItsPodsLeaseAtOnce(t *testing.T) {
	cfg := apiservertest.Start(t)
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	c, err := crclient.New(cfg, crclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: controllerLeaseNamespace}}); err != nil {
		t.Fatal(err)
	}
	holder := controllerLeaseIdentity("console-api-a", "uid-1")
	now := metav1.NewMicroTime(time.Now())
	if err := c.Create(ctx, &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: controllerLeaseName, Namespace: controllerLeaseNamespace},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: &holder, LeaseDurationSeconds: ptr.To[int32](60), AcquireTime: &now, RenewTime: &now,
		},
	}); err != nil {
		t.Fatal(err)
	}

	recreated := newConsoleAPIManager(t, cfg, scheme, controllerLeaseIdentity("console-api-a", "uid-2"), controllerLeaseTiming, nil)
	recreated.start(t)
	select {
	case <-recreated.mgr.Elected():
		t.Fatal("a recreated pod with the same name took a Lease another process may still hold")
	case <-time.After(5 * time.Second):
	}
	recreated.stop()

	restarted := newConsoleAPIManager(t, cfg, scheme, holder, controllerLeaseTiming, nil)
	restarted.start(t)
	select {
	case <-restarted.mgr.Elected():
	case <-time.After(10 * time.Second):
		t.Fatal("the restarted container waited for its own pod's Lease to expire")
	}
}
