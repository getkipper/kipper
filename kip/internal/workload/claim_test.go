package workload

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/workload"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

func claimScheme() *runtime.Scheme {
	s := workloadScheme()
	s.AddKnownTypeWithName(ClaimGVR.GroupVersion().WithKind("WorkloadName"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(ClaimGVR.GroupVersion().WithKind("WorkloadNameList"), &unstructured.UnstructuredList{})
	return s
}

// The reservation is a create, so of two callers racing for one name exactly
// one wins. The lookup this replaces let both through, because both read before
// either wrote.
func TestReserve_OnlyOneKindCanWinAName(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClient(claimScheme())
	ctx := context.Background()

	// Both read the world before either writes.
	require.NoError(t, EnsureNameFree(ctx, dyn, "shop-prod", "checkout", "app"))
	require.NoError(t, EnsureNameFree(ctx, dyn, "shop-prod", "checkout", "job"))

	_, appErr := Reserve(ctx, dyn, "shop-prod", "checkout", "app")
	_, jobErr := Reserve(ctx, dyn, "shop-prod", "checkout", "job")

	require.NoError(t, appErr, "the first claimant must win")
	require.Error(t, jobErr, "the second claimant must lose, whatever it read first")

	var taken workload.NameTakenError
	require.True(t, errors.As(jobErr, &taken))
	assert.Equal(t, "app", taken.Kind)
}

func TestReserve_ReleasesTheNameWhenTheWorkloadIsNotWritten(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClient(claimScheme())
	ctx := context.Background()

	release, err := Reserve(ctx, dyn, "shop-prod", "checkout", "app")
	require.NoError(t, err)
	release()

	_, err = dyn.Resource(ClaimGVR).Namespace("shop-prod").Get(ctx, "checkout", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "the abandoned reservation still holds the name")
}

// A cluster that predates the CRD keeps the behaviour it already had rather
// than failing every create.
func TestReserve_FallsBackWhereTheClusterHasNoClaims(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClient(claimScheme(),
		existing(manifest.AppGVR, "App", "shop-prod"))
	dyn.PrependReactor("create", "workloadnames", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(ClaimGVR.GroupResource(), "")
	})

	_, err := Reserve(context.Background(), dyn, "shop-prod", "checkout", "function")
	require.Error(t, err)

	var taken workload.NameTakenError
	require.True(t, errors.As(err, &taken), "the fallback must still name the holder")
	assert.Equal(t, "app", taken.Kind)
}

// The compatibility path itself: a cluster whose API has no WorkloadName
// resource must still be able to create workloads. Staging a competing workload
// would let the lookup answer first and leave this branch unexercised.
func TestReserve_ProceedsWhereTheClusterHasNoClaimsAndTheNameIsFree(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClient(claimScheme())
	dyn.PrependReactor("create", "workloadnames", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(ClaimGVR.GroupResource(), "")
	})

	release, err := Reserve(context.Background(), dyn, "shop-prod", "checkout", "function")
	require.NoError(t, err, "a cluster without the reservation CRD could not create a workload")
	require.NotNil(t, release)
}
