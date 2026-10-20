package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

func postgresPlan(svc manifest.SvcSpec) applyPlan {
	m := &manifest.Manifest{Project: "shop", Services: map[string]manifest.SvcSpec{"db": svc}}
	return applyPlan{m: m, project: "shop", projectCR: "shop", namespace: "shop", resources: manifest.Convert(m, "shop", nil)}
}

func liveTLS(t *testing.T, dyn dynamic.Interface) string {
	t.Helper()
	svc, err := dyn.Resource(manifest.ServiceGVR).Namespace("shop").Get(context.Background(), "db", metav1.GetOptions{})
	require.NoError(t, err)
	tls, _, _ := unstructured.NestedString(svc.Object, "spec", "tls")
	return tls
}

func setLiveTLS(t *testing.T, dyn dynamic.Interface, mode string) {
	t.Helper()
	svc, err := dyn.Resource(manifest.ServiceGVR).Namespace("shop").Get(context.Background(), "db", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedField(svc.Object, mode, "spec", "tls"))
	_, err = dyn.Resource(manifest.ServiceGVR).Namespace("shop").Update(context.Background(), svc, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func TestApplyCreatesAManifestPostgresWithTLSRequired(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	_, _, err := applyPlans(context.Background(), dyn, k8sfake.NewSimpleClientset(), []applyPlan{postgresPlan(manifest.SvcSpec{Type: "postgres"})}, false)
	require.NoError(t, err)
	assert.Equal(t, "required", liveTLS(t, dyn), "Omitting TLS must use the default required mode, which rejects plaintext connections.")
}

func TestAnExplicitTLSModeInAManifestIsKept(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	_, _, err := applyPlans(context.Background(), dyn, k8sfake.NewSimpleClientset(), []applyPlan{postgresPlan(manifest.SvcSpec{Type: "postgres", TLS: "off"})}, false)
	require.NoError(t, err)
	assert.Equal(t, "off", liveTLS(t, dyn), "an explicit choice is the operator's decision")
}

func TestApplyKeepsTheModeARunningServiceHasWhenTheManifestOmitsIt(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	ctx := context.Background()
	_, _, err := applyPlans(ctx, dyn, k8sfake.NewSimpleClientset(), []applyPlan{postgresPlan(manifest.SvcSpec{Type: "postgres", TLS: "on"})}, false)
	require.NoError(t, err)

	plan := postgresPlan(manifest.SvcSpec{Type: "postgres", Storage: "2Gi"})
	changes, err := scanChangesWith(ctx, dyn, "shop", plan.resources, newSchemaDefaults())
	require.NoError(t, err)
	for _, c := range changes {
		assert.NotEqual(t, "tls", c.change.Path, "dry run and diff must not report the mode as cleared")
	}

	// Change the mode after the scan to verify that the write uses the latest live value.
	setLiveTLS(t, dyn, "required")
	_, _, err = applyPlans(ctx, dyn, k8sfake.NewSimpleClientset(), []applyPlan{plan}, false)
	require.NoError(t, err)
	assert.Equal(t, "required", liveTLS(t, dyn), "a running database never changes mode because a manifest left the field out")
}

func TestAServiceWithoutTLSStaysWithoutItWhenTheManifestOmitsTheField(t *testing.T) {
	dyn := fakeWorkloadDynamic()
	ctx := context.Background()
	_, _, err := applyPlans(ctx, dyn, k8sfake.NewSimpleClientset(), []applyPlan{postgresPlan(manifest.SvcSpec{Type: "postgres", TLS: "off"})}, false)
	require.NoError(t, err)

	_, _, err = applyPlans(ctx, dyn, k8sfake.NewSimpleClientset(), []applyPlan{postgresPlan(manifest.SvcSpec{Type: "postgres"})}, false)
	require.NoError(t, err)
	assert.Equal(t, "off", liveTLS(t, dyn))
}
