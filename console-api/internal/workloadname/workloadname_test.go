package workloadname

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/workload"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, kipperv1.AddToScheme(s))
	return s
}

func claimClient(t *testing.T) crclient.Client {
	t.Helper()
	return crfake.NewClientBuilder().WithScheme(testScheme(t)).Build()
}

// The whole point of the claim: the reservation is a create, so of two callers
// racing for one name exactly one wins. The lookup guard this replaces let both
// pass, because both read before either wrote.
func TestClaim_OnlyOneKindCanWinAName(t *testing.T) {
	c := claimClient(t)
	ctx := context.Background()

	// Both read the world before either writes, which is the race the lookup
	// guard could not survive.
	require.NoError(t, EnsureFree(ctx, c, "shop-prod", "checkout", "app"))
	require.NoError(t, EnsureFree(ctx, c, "shop-prod", "checkout", "job"))

	_, _, appErr := Claim(ctx, c, "shop-prod", "checkout", "app")
	_, _, jobErr := Claim(ctx, c, "shop-prod", "checkout", "job")

	require.NoError(t, appErr, "the first claimant must win")
	require.Error(t, jobErr, "the second claimant must lose, whatever it read first")

	var taken workload.NameTakenError
	require.True(t, errors.As(jobErr, &taken))
	assert.Equal(t, "app", taken.Kind)
}

// A claim this kind already holds is its own, so an upsert or a re-apply keeps
// working rather than refusing the workload its own name.
func TestClaim_TheHolderKeepsItsOwnName(t *testing.T) {
	c := claimClient(t)
	ctx := context.Background()

	_, _, err := Claim(ctx, c, "shop-prod", "checkout", "app")
	require.NoError(t, err)

	held, _, err := Claim(ctx, c, "shop-prod", "checkout", "app")
	require.NoError(t, err)
	assert.True(t, held, "a second claim by the holder has to report that it already held it")
}

// A create that fails after the reservation must not park the name for ever.
func TestReserve_ReleasesTheNameWhenTheWorkloadIsNotWritten(t *testing.T) {
	c := claimClient(t)
	ctx := context.Background()

	release, err := Reserve(ctx, c, "shop-prod", "checkout", "app")
	require.NoError(t, err)
	release()

	var claim kipperv1.WorkloadName
	err = c.Get(ctx, types.NamespacedName{Name: "checkout", Namespace: "shop-prod"}, &claim)
	assert.True(t, apierrors.IsNotFound(err), "the abandoned reservation still holds the name")
}

// Releasing must not drop a reservation somebody else already held, or a failed
// create would free the winner's name.
func TestReserve_DoesNotReleaseANameItDidNotTake(t *testing.T) {
	c := claimClient(t)
	ctx := context.Background()

	_, _, err := Claim(ctx, c, "shop-prod", "checkout", "app")
	require.NoError(t, err)

	release, err := Reserve(ctx, c, "shop-prod", "checkout", "app")
	require.NoError(t, err)
	release()

	var claim kipperv1.WorkloadName
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "checkout", Namespace: "shop-prod"}, &claim),
		"the holder's reservation was released by somebody else's rollback")
}

// A cluster that predates the CRD keeps the behaviour it already had rather
// than failing every create.
func TestReserve_FallsBackToTheLookupWhereTheClusterHasNoClaims(t *testing.T) {
	// A scheme without WorkloadName is what a client talking to such a cluster
	// effectively has: the create comes back as a kind the cluster does not know.
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, kipperv1.AddToScheme(s))
	c := &noClaimsClient{Client: crfake.NewClientBuilder().WithScheme(s).WithObjects(
		&kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-prod"}},
	).Build()}

	_, err := Reserve(context.Background(), c, "shop-prod", "checkout", "function")
	require.Error(t, err)

	var taken workload.NameTakenError
	require.True(t, errors.As(err, &taken), "the fallback must still name the holder")
	assert.Equal(t, "app", taken.Kind)
}

// noClaimsClient is a cluster with no WorkloadName resource.
type noClaimsClient struct {
	crclient.Client
}

func (n *noClaimsClient) Create(ctx context.Context, obj crclient.Object, opts ...crclient.CreateOption) error {
	if _, ok := obj.(*kipperv1.WorkloadName); ok {
		return apierrors.NewNotFound(kipperv1.GroupVersion.WithResource("workloadnames").GroupResource(), "")
	}
	return n.Client.Create(ctx, obj, opts...)
}

// A stale rollback must not delete a reservation that replaced its own: a claim
// removed out of band and re-made by another kind in between would otherwise be
// handed away by a caller that no longer owns anything.
//
// The precondition is what prevents it, and only a real API server enforces one
// (the fake assigns no UID on create and ignores preconditions on delete). So
// the create is intercepted to assign a UID the way the server would, and the
// delete is intercepted to assert the rollback carried that UID rather than
// deleting by name.
func TestRelease_DeletesOnlyTheClaimThisCallerCreated(t *testing.T) {
	const assigned = types.UID("the-uid-this-caller-created")
	var sawPrecondition *types.UID

	c := crfake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				if claim, ok := obj.(*kipperv1.WorkloadName); ok {
					claim.UID = assigned
				}
				return cl.Create(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.DeleteOption) error {
				for _, o := range opts {
					if p, ok := o.(crclient.Preconditions); ok {
						sawPrecondition = p.UID
					}
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()

	release, err := Reserve(context.Background(), c, "shop-prod", "checkout", "app")
	require.NoError(t, err)
	release()

	require.NotNil(t, sawPrecondition, "the rollback deleted by name, so it can take somebody else's reservation")
	assert.Equal(t, assigned, *sawPrecondition,
		"the rollback did not carry the uid this caller created")
}

// The compatibility path itself, with nothing else to refuse the name: a
// cluster whose API has no WorkloadName resource must still be able to create
// workloads. Staging a competing workload would let the lookup answer first and
// leave this branch unexercised.
func TestReserve_ProceedsWhereTheClusterHasNoClaimsAndTheNameIsFree(t *testing.T) {
	s := testScheme(t)
	c := &noClaimsClient{Client: crfake.NewClientBuilder().WithScheme(s).Build()}

	release, err := Reserve(context.Background(), c, "shop-prod", "checkout", "function")
	require.NoError(t, err, "a cluster without the reservation CRD could not create a workload")
	require.NotNil(t, release)
}

// The console surface needs the same rule the CLI has, in both directions. An
// upgraded namespace holds no reservation, so whoever writes first would
// otherwise establish one: an inline save of the newer intruder would create a
// claim that then outranks age at the controllers, which is the headline bug
// this rule exists to stop.
func TestEnsureFree_TheOlderWorkloadKeepsAContestedName(t *testing.T) {
	ctx := context.Background()
	long := metav1.NewTime(time.Now().Add(-24 * time.Hour))
	recent := metav1.NewTime(time.Now().Add(-time.Minute))

	t.Run("the newer workload is refused its own re-apply", func(t *testing.T) {
		c := claimClientWith(t,
			&kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-prod", CreationTimestamp: long}},
			&kipperv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-prod", CreationTimestamp: recent}},
		)

		err := EnsureFree(ctx, c, "shop-prod", "checkout", "function")
		require.Error(t, err, "the newer function re-applied over a name the older app holds")

		var taken workload.NameTakenError
		require.True(t, errors.As(err, &taken))
		assert.Equal(t, "app", taken.Kind)
	})

	t.Run("the older workload keeps working", func(t *testing.T) {
		c := claimClientWith(t,
			&kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-prod", CreationTimestamp: recent}},
			&kipperv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop-prod", CreationTimestamp: long}},
		)

		require.NoError(t, EnsureFree(ctx, c, "shop-prod", "checkout", "function"),
			"the function that has held this name for a day was refused its own update")
	})
}

func claimClientWith(t *testing.T, objs ...crclient.Object) crclient.Client {
	t.Helper()
	return crfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
}
