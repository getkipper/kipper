package workload

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/workload"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

func workloadScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for gvr, list := range map[schema.GroupVersionResource]string{
		manifest.AppGVR:      "AppList",
		manifest.FunctionGVR: "FunctionList",
		manifest.JobGVR:      "JobList",
	} {
		s.AddKnownTypeWithName(gvr.GroupVersion().WithKind(list), &unstructured.UnstructuredList{})
	}
	return s
}

func existing(gvr schema.GroupVersionResource, kind, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvr.GroupVersion().String(),
		"kind":       kind,
		"metadata":   map[string]any{"name": "checkout", "namespace": namespace},
	}}
}

func TestEnsureNameFree_RefusesANameAnotherKindHolds(t *testing.T) {
	for _, tc := range []struct {
		creating string
		holder   string
		obj      *unstructured.Unstructured
		want     string
	}{
		{"function", "app", existing(manifest.AppGVR, "App", "shop-prod"), `already used by an app`},
		{"function", "job", existing(manifest.JobGVR, "Job", "shop-prod"), `already used by a job`},
		{"app", "function", existing(manifest.FunctionGVR, "Function", "shop-prod"), `already used by a function`},
		{"job", "app", existing(manifest.AppGVR, "App", "shop-prod"), `already used by an app`},
	} {
		t.Run(tc.creating+" over "+tc.holder, func(t *testing.T) {
			dyn := dynfake.NewSimpleDynamicClient(workloadScheme(), tc.obj)

			err := EnsureNameFree(context.Background(), dyn, "shop-prod", "checkout", tc.creating)
			require.Error(t, err)

			var taken workload.NameTakenError
			require.True(t, errors.As(err, &taken), "a taken name must be distinguishable from a lookup failure")
			require.Equal(t, tc.holder, taken.Kind)
			require.Contains(t, err.Error(), tc.want, "the message has to read as English and name the kind")
		})
	}
}

// The same-kind case is left to the API's own AlreadyExists, so each caller
// keeps whatever it already says about it.
func TestEnsureNameFree_IgnoresTheKindBeingCreated(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClient(workloadScheme(),
		existing(manifest.FunctionGVR, "Function", "shop-prod"))

	require.NoError(t, EnsureNameFree(context.Background(), dyn, "shop-prod", "checkout", "function"))
}

func TestEnsureNameFree_AcceptsANameHeldInAnotherNamespace(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClient(workloadScheme(),
		existing(manifest.AppGVR, "App", "shop-staging"))

	require.NoError(t, EnsureNameFree(context.Background(), dyn, "shop-prod", "checkout", "function"))
}

// A cluster that cannot answer is not a cluster saying the name is taken. The
// two call for different things: one is "rename it", the other is "try again".
func TestEnsureNameFree_ALookupFailureIsNotATakenName(t *testing.T) {
	dyn := dynfake.NewSimpleDynamicClient(workloadScheme())
	dyn.PrependReactor("get", "apps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("etcd leader changed")
	})

	err := EnsureNameFree(context.Background(), dyn, "shop-prod", "checkout", "function")
	require.Error(t, err)

	var taken workload.NameTakenError
	require.False(t, errors.As(err, &taken), "an unreachable apiserver was reported as a taken name")
	require.Contains(t, err.Error(), "checking whether")
}
