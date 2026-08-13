package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func claimFor(t *testing.T, r *FunctionReconciler, name string) *kipperv1.WorkloadName {
	t.Helper()
	var claim kipperv1.WorkloadName
	err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &claim)
	require.NoError(t, err, "no name reservation for default/%s", name)
	return &claim
}

// Every workload on a cluster holds no reservation the moment it upgrades, and
// a migration restore writes CRs directly. The reconcile is what converts them,
// so the invariant reaches workloads nobody touches again.
func TestReconcileNameClaim_BackfillsAWorkloadThatHasNone(t *testing.T) {
	scheme := testScheme()
	fn := collidingFunction()
	fn.Name = "reports"
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn).WithStatusSubresource(fn).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "reports", Namespace: "default"}})

	claim := claimFor(t, r, "reports")
	assert.Equal(t, "function", claim.Spec.Kind)

	// Owned by the workload, so deleting the workload frees the name rather
	// than parking it for ever.
	require.Len(t, claim.OwnerReferences, 1)
	assert.Equal(t, "Function", claim.OwnerReferences[0].Kind)
	assert.Equal(t, "reports", claim.OwnerReferences[0].Name)
	require.NotNil(t, claim.OwnerReferences[0].Controller)
	assert.True(t, *claim.OwnerReferences[0].Controller)
}

// A reservation whose owner reference never landed, because the client that
// made it died between reserving the name and writing the workload, must be
// adopted. Left ownerless it outlives its workload and parks the name.
func TestReconcileNameClaim_AdoptsAnOwnerlessReservation(t *testing.T) {
	scheme := testScheme()
	fn := collidingFunction()
	fn.Name = "reports"
	ownerless := &kipperv1.WorkloadName{
		ObjectMeta: metav1.ObjectMeta{Name: "reports", Namespace: "default"},
		Spec:       kipperv1.WorkloadNameSpec{Kind: "function"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(fn, ownerless).WithStatusSubresource(fn).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "reports", Namespace: "default"}})

	claim := claimFor(t, r, "reports")
	require.Len(t, claim.OwnerReferences, 1, "the ownerless reservation was not adopted")
	assert.Equal(t, "reports", claim.OwnerReferences[0].Name)
}

// A restore carries a reservation whose owner reference names a UID from the
// cluster it came from, so it points at nothing here and garbage collection
// would never fire.
func TestReconcileNameClaim_RepairsAReservationOwnedByAStaleUID(t *testing.T) {
	scheme := testScheme()
	fn := collidingFunction()
	fn.Name = "reports"
	fn.UID = "99999999-8888-7777-6666-555555555555"
	controller := true
	stale := &kipperv1.WorkloadName{
		ObjectMeta: metav1.ObjectMeta{
			Name: "reports", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(),
				Kind:       "Function",
				Name:       "reports",
				UID:        "11111111-1111-1111-1111-111111111111",
				Controller: &controller,
			}},
		},
		Spec: kipperv1.WorkloadNameSpec{Kind: "function"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(fn, stale).WithStatusSubresource(fn).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "reports", Namespace: "default"}})

	claim := claimFor(t, r, "reports")
	require.Len(t, claim.OwnerReferences, 1)
	assert.Equal(t, fn.UID, claim.OwnerReferences[0].UID,
		"the reservation still points at a UID from another cluster, so nothing will ever collect it")
}

// A reservation another kind holds is the collision itself. Taking it would
// hide which workload got there first, and the pass is about to refuse the
// child objects over it anyway.
func TestReconcileNameClaim_LeavesAnotherKindsReservationAlone(t *testing.T) {
	scheme := testScheme()
	fn := collidingFunction()
	held := &kipperv1.WorkloadName{
		ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "default"},
		Spec:       kipperv1.WorkloadNameSpec{Kind: "app"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(fn, held, deploymentOwnedByApp()).WithStatusSubresource(fn).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	_, _ = r.Reconcile(context.Background(), functionRequest())

	claim := claimFor(t, r, "hello")
	assert.Equal(t, "app", claim.Spec.Kind, "the function took a reservation the app holds")
	assert.Empty(t, claim.OwnerReferences, "the function adopted a reservation that is not its own")
}

// GitOps, kubectl and a restore all write CRs straight to the API server, where
// no reservation was ever taken. The losing controller stopping is the only
// thing between that and two live workloads: an App and a Job share no child
// object, so nothing further down would refuse them, and the App's Service
// selects the label the Job's pods carry.
func TestReconcileNameClaim_TheLoserOfADirectWriteStopsReconciling(t *testing.T) {
	scheme := testScheme()

	// Both written directly, as a GitOps controller would.
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "default", UID: "app-uid"},
		Spec:       kipperv1.AppSpec{Image: "checkout:v1", Port: 8080},
	}
	job := &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "default", UID: "job-uid"},
		Spec:       kipperv1.JobSpec{Image: "checkout:v1", Schedule: "0 2 * * *"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, job).
		WithStatusSubresource(app, job).Build()

	// The App reconciles first and takes the reservation.
	appR := &AppReconciler{Client: c, Scheme: scheme}
	_, _ = appR.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "checkout", Namespace: "default"}})

	var claim kipperv1.WorkloadName
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "checkout", Namespace: "default"}, &claim))
	require.Equal(t, "app", claim.Spec.Kind)

	// The Job must now refuse to build anything.
	jobR := &JobReconciler{Client: c, Scheme: scheme}
	_, err := jobR.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "checkout", Namespace: "default"}})
	require.Error(t, err, "the job that lost the name carried on reconciling")
	assert.Contains(t, err.Error(), "already used by an app")

	var got kipperv1.Job
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "checkout", Namespace: "default"}, &got))
	assert.Equal(t, "Failed", got.Status.Phase, "the blocked job does not say so")

	// Nothing may carry the label the app's Service selects.
	var cronJobs batchv1.CronJobList
	require.NoError(t, c.List(context.Background(), &cronJobs, crclient.InNamespace("default")))
	assert.Empty(t, cronJobs.Items, "the blocked job built a CronJob anyway")
}

// Deleting one of the two colliding workloads is how an operator fixes this, so
// a blocked workload has to stay deletable. Refusing before the deletion path
// would leave its finalizer in place and the object stuck in Terminating for
// ever, taking the remedy away at the moment it is needed.
func TestReconcileNameClaim_ABlockedWorkloadCanStillBeDeleted(t *testing.T) {
	scheme := testScheme()

	held := &kipperv1.WorkloadName{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "default"},
		Spec:       kipperv1.WorkloadNameSpec{Kind: "app"},
	}
	deleting := &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout", Namespace: "default",
			Finalizers:        []string{jobFinalizer},
			DeletionTimestamp: &metav1.Time{Time: metav1.Now().Time},
		},
		Spec: kipperv1.JobSpec{Image: "checkout:v1", Schedule: "0 2 * * *"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(held, deleting).WithStatusSubresource(deleting).Build()
	r := &JobReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "checkout", Namespace: "default"}})
	require.NoError(t, err, "the blocked job refused its own deletion")

	var got kipperv1.Job
	getErr := c.Get(context.Background(), types.NamespacedName{Name: "checkout", Namespace: "default"}, &got)
	if getErr == nil {
		assert.Empty(t, got.Finalizers, "the finalizer was never removed, so the job is stuck in Terminating")
	}
}

// Both reconcilers read the claim as absent — a true race, or the ordinary lag
// of the cache the Get reads — and only the create settles it. AlreadyExists
// says somebody else won, so treating it as success is what would let both
// workloads go live.
func TestReconcileNameClaim_LosingTheCreateRaceStopsTheWorkload(t *testing.T) {
	scheme := testScheme()
	job := &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "default", UID: "job-uid"},
		Spec:       kipperv1.JobSpec{Image: "checkout:v1", Schedule: "0 2 * * *"},
	}

	// The App's claim lands between this reconciler's read and its create, so
	// the Get sees nothing and the Create loses.
	var appClaimed bool
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(job).
		WithStatusSubresource(job).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				if _, ok := obj.(*kipperv1.WorkloadName); ok && !appClaimed {
					return apierrors.NewNotFound(
						kipperv1.GroupVersion.WithResource("workloadnames").GroupResource(), key.Name)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
			Create: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				if claim, ok := obj.(*kipperv1.WorkloadName); ok && !appClaimed {
					appClaimed = true
					// The App got there first.
					if err := cl.Create(ctx, &kipperv1.WorkloadName{
						ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace},
						Spec:       kipperv1.WorkloadNameSpec{Kind: "app"},
					}); err != nil {
						return err
					}
					return apierrors.NewAlreadyExists(
						kipperv1.GroupVersion.WithResource("workloadnames").GroupResource(), claim.Name)
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()

	r := &JobReconciler{Client: c, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "checkout", Namespace: "default"}})
	require.Error(t, err, "the job that lost the create race carried on reconciling")

	var cronJobs batchv1.CronJobList
	require.NoError(t, c.List(context.Background(), &cronJobs, crclient.InNamespace("default")))
	assert.Empty(t, cronJobs.Items, "the job that lost the race built a CronJob anyway")
}

// The collision persists until somebody renames one of the two, so this path
// runs on every pass. The controller watches Jobs with no status-change
// predicate, which makes each status write its own next trigger.
func TestReconcileNameClaim_ABlockedJobStopsRewritingItsStatus(t *testing.T) {
	scheme := testScheme()
	held := &kipperv1.WorkloadName{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "default"},
		Spec:       kipperv1.WorkloadNameSpec{Kind: "app"},
	}
	job := &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "default"},
		Spec:       kipperv1.JobSpec{Image: "checkout:v1", Schedule: "0 2 * * *"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(held, job).WithStatusSubresource(job).Build()
	r := &JobReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "checkout", Namespace: "default"}}

	_, err := r.Reconcile(context.Background(), req)
	require.Error(t, err)

	var afterFirst kipperv1.Job
	require.NoError(t, c.Get(context.Background(), req.NamespacedName, &afterFirst))

	_, err = r.Reconcile(context.Background(), req)
	require.Error(t, err, "the collision is unchanged, so the pass still fails")

	var afterSecond kipperv1.Job
	require.NoError(t, c.Get(context.Background(), req.NamespacedName, &afterSecond))

	assert.Equal(t, afterFirst.ResourceVersion, afterSecond.ResourceVersion,
		"a second pass that learned nothing new wrote status again, which re-enqueues itself")
}

// Reclamation can delete an ownerless claim between this pass reading it and
// adopting it. NotFound on the adoption therefore means the reservation is
// gone, not merely un-collected, so the workload holds nothing and must not go
// on to build children — another kind is free to take the name.
func TestReconcileNameClaim_AFailedAdoptionStopsTheWorkload(t *testing.T) {
	scheme := testScheme()
	job := &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "default", UID: "job-uid"},
		Spec:       kipperv1.JobSpec{Image: "checkout:v1", Schedule: "0 2 * * *"},
	}
	ownerless := &kipperv1.WorkloadName{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "default"},
		Spec:       kipperv1.WorkloadNameSpec{Kind: "job"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(job, ownerless).WithStatusSubresource(job).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if _, ok := obj.(*kipperv1.WorkloadName); ok {
					// Reclamation got there first.
					return apierrors.NewNotFound(
						kipperv1.GroupVersion.WithResource("workloadnames").GroupResource(), obj.GetName())
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()

	r := &JobReconciler{Client: c, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "checkout", Namespace: "default"}})
	require.Error(t, err, "the job carried on without holding its reservation")

	var cronJobs batchv1.CronJobList
	require.NoError(t, c.List(context.Background(), &cronJobs, crclient.InNamespace("default")))
	assert.Empty(t, cronJobs.Items, "the job built a CronJob while holding no reservation")
}

// A handler reserves the name with the direct client and then writes the
// workload, so on the workload's first pass the claim it loses the create to is
// very often its own. Reading the winner through the cache, which has not
// caught up, reported no holder at all and marked a healthy workload failed for
// holding its own name.
func TestReconcileNameClaim_DoesNotBlockAWorkloadOverItsOwnClaim(t *testing.T) {
	scheme := testScheme()
	job := &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "default", UID: "job-uid"},
		Spec:       kipperv1.JobSpec{Image: "checkout:v1", Schedule: "0 2 * * *"},
	}
	// The handler's reservation, already in the cluster and invisible to the
	// reconciler's cache.
	own := &kipperv1.WorkloadName{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "default"},
		Spec:       kipperv1.WorkloadNameSpec{Kind: "job"},
	}
	live := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(own).Build()

	cached := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(job).WithStatusSubresource(job).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				if _, ok := obj.(*kipperv1.WorkloadName); ok {
					// The lagging cache: it cannot see the handler's claim.
					return apierrors.NewNotFound(
						kipperv1.GroupVersion.WithResource("workloadnames").GroupResource(), key.Name)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
			Create: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				if _, ok := obj.(*kipperv1.WorkloadName); ok {
					return apierrors.NewAlreadyExists(
						kipperv1.GroupVersion.WithResource("workloadnames").GroupResource(), obj.GetName())
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()

	// live is the uncached reader, which can see the claim the cache cannot.
	r := &JobReconciler{Client: cached, APIReader: live, Scheme: scheme}
	heldBy, err := reconcileNameClaim(context.Background(), r.Client, r.hostReader(), r.Scheme, job, "job")

	require.NoError(t, err)
	assert.Empty(t, heldBy, "a job was blocked over the reservation it holds itself")
}

// Not being able to establish that the name is this workload's is not
// permission to use it. A controller enforcing an invariant has to fail closed,
// or a transient API error becomes a licence to build children under a name
// somebody else may hold.
func TestReconcileNameClaim_FailsClosedWhenTheClaimCannotBeEstablished(t *testing.T) {
	scheme := testScheme()
	job := &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "default", UID: "job-uid"},
		Spec:       kipperv1.JobSpec{Image: "checkout:v1", Schedule: "0 2 * * *"},
	}

	for name, funcs := range map[string]interceptor.Funcs{
		"the claim cannot be read": {
			Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				if _, ok := obj.(*kipperv1.WorkloadName); ok {
					return apierrors.NewServiceUnavailable("etcd leader changed")
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		},
		"the claim cannot be created": {
			Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				if _, ok := obj.(*kipperv1.WorkloadName); ok {
					return apierrors.NewNotFound(
						kipperv1.GroupVersion.WithResource("workloadnames").GroupResource(), key.Name)
				}
				return cl.Get(ctx, key, obj, opts...)
			},
			Create: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				if _, ok := obj.(*kipperv1.WorkloadName); ok {
					return apierrors.NewServiceUnavailable("etcd leader changed")
				}
				return cl.Create(ctx, obj, opts...)
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := crfake.NewClientBuilder().WithScheme(scheme).
				WithObjects(job.DeepCopy()).WithStatusSubresource(job).
				WithInterceptorFuncs(funcs).Build()
			r := &JobReconciler{Client: c, Scheme: scheme}

			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "checkout", Namespace: "default"}})
			require.Error(t, err, "the workload carried on without establishing that the name is its own")

			var cronJobs batchv1.CronJobList
			require.NoError(t, c.List(context.Background(), &cronJobs, crclient.InNamespace("default")))
			assert.Empty(t, cronJobs.Items, "children were built without a reservation")
		})
	}
}

// On an upgraded cluster no workload holds a reservation yet, so whoever
// reconciles first would take the name. Controller scheduling order is not
// evidence of who had it: a Job written straight to the API server must not
// take a live App's name by reconciling before it, because the App's refusal
// afterwards does not remove the children it already has, and its Service goes
// on selecting the Job's pods.
func TestReconcileNameClaim_TheOlderWorkloadKeepsTheNameOnBackfill(t *testing.T) {
	scheme := testScheme()
	long := metav1.NewTime(time.Now().Add(-24 * time.Hour))
	recent := metav1.NewTime(time.Now().Add(-time.Minute))

	// The App has served this name since yesterday and holds no reservation.
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout", Namespace: "default", UID: "app-uid", CreationTimestamp: long,
		},
		Spec: kipperv1.AppSpec{Image: "checkout:v1", Port: 8080},
	}
	// The Job turned up a minute ago, written straight to the API server.
	job := &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout", Namespace: "default", UID: "job-uid", CreationTimestamp: recent,
		},
		Spec: kipperv1.JobSpec{Image: "checkout:v1", Schedule: "0 2 * * *"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, job).
		WithStatusSubresource(app, job).Build()

	// The Job reconciles first, which before this rule was enough to win.
	jobR := &JobReconciler{Client: c, APIReader: c, Scheme: scheme}
	_, err := jobR.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "checkout", Namespace: "default"}})
	require.Error(t, err, "the newer job took the name from a workload that has served it for a day")

	var claim kipperv1.WorkloadName
	err = c.Get(context.Background(), types.NamespacedName{Name: "checkout", Namespace: "default"}, &claim)
	if err == nil {
		assert.Equal(t, "app", claim.Spec.Kind, "the reservation went to the newer workload")
	}

	var cronJobs batchv1.CronJobList
	require.NoError(t, c.List(context.Background(), &cronJobs, crclient.InNamespace("default")))
	assert.Empty(t, cronJobs.Items, "the newer job built children over the older workload's name")
}

// creationTimestamp has one-second granularity, and a GitOps apply or a restore
// lands a collision well inside one second, so the tie is the ordinary case
// rather than a corner. Both sides must reach opposite answers: if both refuse,
// neither workload ever runs again without a human.
func TestReconcileNameClaim_ATieResolvesInExactlyOneDirection(t *testing.T) {
	scheme := testScheme()
	same := metav1.NewTime(time.Now().Truncate(time.Second))
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "default", CreationTimestamp: same},
		Spec:       kipperv1.AppSpec{Image: "checkout:v1", Port: 8080},
	}
	job := &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "default", CreationTimestamp: same},
		Spec:       kipperv1.JobSpec{Image: "checkout:v1", Schedule: "0 2 * * *"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, job).Build()
	key := types.NamespacedName{Name: "checkout", Namespace: "default"}

	fromJob, err := olderHolder(context.Background(), c, key, "job", job)
	require.NoError(t, err)
	fromApp, err := olderHolder(context.Background(), c, key, "app", app)
	require.NoError(t, err)

	require.False(t, fromJob != "" && fromApp != "",
		"both workloads refused the name, so neither runs again without a human")
	require.False(t, fromJob == "" && fromApp == "",
		"both workloads took the name, which is the collision this prevents")
	assert.Equal(t, "app", fromJob, "the tiebreak has to be stable, not merely decisive")
}

// A cluster can upgrade into a collision that is already running: both
// workloads built their children before the rule existed. Kipper does not tear
// the loser's children down, so the message has to say they are still there, or
// an operator reads "blocked" as "contained" while traffic is still being
// routed to the wrong pods.
func TestBlockedMessage_SaysWhatKeepsRunning(t *testing.T) {
	msg := blockedMessage("checkout", "app")

	assert.Contains(t, msg, "already used by an app",
		"the message has to name the kind holding it, because renaming is the remedy")
	assert.Contains(t, msg, "keeps running",
		"an operator upgrading into a live collision is not told their workload is still serving")
}

// A claim of the right kind can already be controlled by something else: a
// direct apply or a restore leaves one behind, and controller-runtime refuses
// to move a controller reference that names a different object. Treating that
// refusal as a successful adoption let the workload build children while its
// name reservation belonged to another owner. Deleting that owner would then
// garbage-collect the claim and free the name under a workload still running
// on it.
func TestReconcileNameClaim_FailsClosedWhenAnotherOwnerHoldsTheClaim(t *testing.T) {
	scheme := testScheme()
	job := &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "default", UID: "job-uid"},
		Spec:       kipperv1.JobSpec{Image: "checkout:v1", Schedule: "0 2 * * *"},
	}
	controller := true
	claim := &kipperv1.WorkloadName{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(),
				Kind:       "App",
				Name:       "something-else",
				UID:        "other-uid",
				Controller: &controller,
			}},
		},
		Spec: kipperv1.WorkloadNameSpec{Kind: "job"},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(job, claim).WithStatusSubresource(job).Build()
	r := &JobReconciler{Client: c, Scheme: scheme}

	_, err := reconcileNameClaim(context.Background(), r.Client, r.hostReader(), r.Scheme, job, "job")

	require.Error(t, err,
		"a workload that could not take ownership of its reservation went on to build children anyway")
}
