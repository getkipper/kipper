package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

// serviceCRs builds a fake cluster holding one Service CR per entry. A key may
// carry an "@" suffix so the same service name can appear in two namespaces.
func serviceCRs(t *testing.T, services map[string]string) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: "Service"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: "ServiceList"},
		&unstructured.UnstructuredList{},
	)
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)

	for key, namespace := range services {
		name, _, _ := strings.Cut(key, "@")
		cr := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "Service",
			"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
		}}
		_, err := dyn.Resource(manifest.ServiceGVR).Namespace(namespace).
			Create(context.Background(), cr, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	return dyn
}

// An alert names a namespace, and the recovery command has to be able to reach
// it. Without these flags the command works only against whatever project
// happens to be persisted, which for a fresh cluster is none at all: the
// namespace resolves empty and the Service CR is never found.
func TestServiceRestartTakesTheSameTargetingFlagsAsTheRestOfServiceCommands(t *testing.T) {
	for _, name := range []string{"project", "environment"} {
		f := serviceRestartCmd.Flags().Lookup(name)
		require.NotNil(t, f, "kip service restart must accept --%s, like every other service command", name)
	}

	assert.Equal(t, "default", serviceRestartCmd.Flags().Lookup("project").DefValue,
		"a cluster with no persisted project must still restart a service in default")
}

// An alert names a namespace, and the operator running the command it prints
// should not have to work out which project that namespace belongs to. `kip app
// restart` already finds its workload by name across the cluster; the recovery
// command for a service does the same when no project was named.
func TestServiceRestartFindsTheServiceWhenNoProjectWasNamed(t *testing.T) {
	dyn := serviceCRs(t, map[string]string{"db": "shop-test"})

	ns, err := findServiceNamespace(context.Background(), dyn, "db")
	require.NoError(t, err)
	assert.Equal(t, "shop-test", ns)
}

// Two projects can each run a service called db. Guessing between them would
// restart the wrong database, so the command has to say so and stop.
func TestServiceRestartRefusesAnAmbiguousName(t *testing.T) {
	dyn := serviceCRs(t, map[string]string{"db": "shop-test", "db@2": "shop-prod"})

	_, err := findServiceNamespace(context.Background(), dyn, "db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shop-prod")
	assert.Contains(t, err.Error(), "shop-test")
	assert.Contains(t, err.Error(), "--project", "the error has to name the way out")
}

func TestServiceRestartSaysWhenThereIsNoSuchService(t *testing.T) {
	dyn := serviceCRs(t, nil)

	_, err := findServiceNamespace(context.Background(), dyn, "db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db")
}
