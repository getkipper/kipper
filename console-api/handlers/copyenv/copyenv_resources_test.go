package copyenv

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
)

// A copy carries the values the user set and leaves the old auto-sizer's
// numbers behind, so the new environment sizes itself.
func TestCopierCarriesTheUsersResourceValuesOnly(t *testing.T) {
	ctx := context.Background()
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithReturnManagedFields().Build()

	// The old auto-sizer wrote the CPU; the user set memory with kip.
	src := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo-test"},
		Spec: kipperv1.AppSpec{Image: "nginx", Port: 80, Resources: kipperv1.AppResources{
			CPURequest: "300m", CPULimit: "300m",
		}},
	}
	require.NoError(t, crclient.WithFieldOwner(crClient, "console-api").Create(ctx, src))
	src.Spec.Resources.MemoryRequest, src.Spec.Resources.MemoryLimit = "512Mi", "2Gi"
	require.NoError(t, crclient.WithFieldOwner(crClient, "kip").Update(ctx, src))

	c := &Copier{CRClient: crClient, Client: fake.NewClientset()}
	_, err := c.Run(ctx, Options{Source: "demo-test", Target: "demo-prod", TargetEnv: "prod", ClusterDomain: "example.com"})
	require.NoError(t, err)

	var copied kipperv1.App
	require.NoError(t, crClient.Get(ctx, crclient.ObjectKey{Namespace: "demo-prod", Name: "web"}, &copied))
	assert.Equal(t, "", copied.Spec.Resources.CPURequest, "the auto-sizer's CPU stays behind")
	assert.Equal(t, "512Mi", copied.Spec.Resources.MemoryRequest)
	assert.Equal(t, "2Gi", copied.Spec.Resources.MemoryLimit)

	spec, err := resourcebounds.AppSpec(&copied)
	require.NoError(t, err)
	assert.Equal(t, resourcebounds.User, spec.MemoryRequest.Source, "the user's memory stays the user's on the target")
}

// When the copy cannot claim the user's values, they stay held on the target:
// nothing automatic changes them until the user confirms them.
func TestCopierLeavesValuesHeldWhenTheClaimFails(t *testing.T) {
	ctx := context.Background()
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithReturnManagedFields().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(context.Context, crclient.WithWatch, runtime.ApplyConfiguration, ...crclient.ApplyOption) error {
				return errors.New("the API server did not answer")
			},
		}).Build()

	src := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "demo-test"},
		Spec: kipperv1.AppSpec{Image: "nginx", Port: 80, Resources: kipperv1.AppResources{
			MemoryRequest: "512Mi", MemoryLimit: "2Gi",
		}},
	}
	require.NoError(t, crclient.WithFieldOwner(crClient, "kip").Create(ctx, src))

	c := &Copier{CRClient: crClient, Client: fake.NewClientset()}
	summary, err := c.Run(ctx, Options{Source: "demo-test", Target: "demo-prod", TargetEnv: "prod", ClusterDomain: "example.com"})
	require.NoError(t, err)
	assert.NotEmpty(t, summary.Warnings, "the user is told the values are held")

	var copied kipperv1.App
	require.NoError(t, crClient.Get(ctx, crclient.ObjectKey{Namespace: "demo-prod", Name: "web"}, &copied))
	assert.Equal(t, "2Gi", copied.Spec.Resources.MemoryLimit, "the value is carried")
	spec, err := resourcebounds.AppSpec(&copied)
	require.NoError(t, err)
	assert.Equal(t, resourcebounds.Held, spec.MemoryLimit.Source, "and held, never sized automatically")
}
