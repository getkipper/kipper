package migration

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// An app whose HPA keeps it serving through a replica freeze goes on writing
// while its data is copied, and everything written after its copy stays on this
// cluster. The operator has to be told, and the one thing that must never
// happen is the flow saying nothing because the check could not run: silence
// reads as "nothing is still taking writes", and the migration goes ahead.
func autoscaledApp(namespace, name string) *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: kipperv1.AppSpec{
			Image:     "web:v1",
			Autoscale: &kipperv1.AppAutoscale{Enabled: true},
		},
	}
}

func TestTheWriteFreezeCheckNamesAnAutoscaledApp(t *testing.T) {
	h := &Handler{
		Client: fake.NewSimpleClientset(projectNamespace("shop-prod", "shop")),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).
			WithObjects(autoscaledApp("shop-prod", "storefront"), ownerOf("shop-prod")).Build(),
	}

	step := h.autoscaledAppsWarning(context.Background(), []string{"shop"})

	require.NotNil(t, step, "an autoscaled app was not reported, so the operator freezes replicas and it keeps serving")
	assert.Contains(t, step.Detail, "shop-prod/storefront")
	assert.NotContains(t, step.Detail, "could not be checked")
}

// A project whose ownership cannot be read is not a project with nothing
// running. Reporting nothing would delete the warning for it silently.
func TestTheWriteFreezeCheckSaysWhatItCouldNotCheck(t *testing.T) {
	failing := crfake.NewClientBuilder().WithScheme(migrationScheme()).
		WithObjects(ownerOf("shop-prod")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				return assert.AnError
			},
		}).Build()
	h := &Handler{
		Client:   fake.NewSimpleClientset(projectNamespace("shop-prod", "shop")),
		CRClient: failing,
	}

	step := h.autoscaledAppsWarning(context.Background(), []string{"shop"})

	require.NotNil(t, step, "the check could not run and said nothing, which reads as nothing still taking writes")
	assert.Contains(t, step.Detail, "could not check")
	assert.Contains(t, step.Detail, "shop")
}

// A project that holds no namespaces has been checked and has nothing, which is
// a different answer from one that could not be looked at.
func TestTheWriteFreezeCheckIsQuietForAProjectWithNothingRunning(t *testing.T) {
	h := &Handler{
		Client: fake.NewSimpleClientset(),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).
			WithObjects(ownerOf("shop-prod")).Build(),
	}

	step := h.autoscaledAppsWarning(context.Background(), []string{"shop"})

	assert.Nil(t, step, "a project holding no namespaces was reported as unable to be checked: %+v", step)
}
