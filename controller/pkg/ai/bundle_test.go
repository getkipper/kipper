package ai

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCheckBundleDrift_NoBundlesInstalled(t *testing.T) {
	// A clean cluster without `kip ai install` ever run reports both
	// bundles as not installed, no drift.
	client := fake.NewSimpleClientset()

	report, err := CheckBundleDrift(context.Background(), client)
	require.NoError(t, err)
	assert.False(t, report.Phase1.Installed)
	assert.False(t, report.RAG.Installed)
	assert.False(t, report.HasDrift(), "no installed bundles means nothing to drift from")
}

func TestCheckBundleDrift_Phase1Healthy(t *testing.T) {
	// State ConfigMap present + every expected workload present →
	// installed, no drift.
	client := fake.NewSimpleClientset(
		bundleStateCM(BundleStateConfigMapName),
		deploy("ollama"),
		deploy("librechat-librechat"),
		ingress("librechat"),
	)

	report, err := CheckBundleDrift(context.Background(), client)
	require.NoError(t, err)
	assert.True(t, report.Phase1.Installed)
	assert.Empty(t, report.Phase1.Missing, "every Phase 1 workload present, no drift expected")
	assert.False(t, report.HasDrift())
}

func TestCheckBundleDrift_Phase1MissingIngress(t *testing.T) {
	// State ConfigMap present, both Deployments present, Ingress gone.
	// The exact failure mode that prompted the manual diagnosis dance
	// on the cluster — the Ingress disappeared and "Ask" started
	// 404ing without any console signal.
	client := fake.NewSimpleClientset(
		bundleStateCM(BundleStateConfigMapName),
		deploy("ollama"),
		deploy("librechat-librechat"),
	)

	report, err := CheckBundleDrift(context.Background(), client)
	require.NoError(t, err)
	assert.True(t, report.Phase1.Installed)
	require.Len(t, report.Phase1.Missing, 1)
	assert.Equal(t, Resource{Kind: "Ingress", Name: "librechat", Namespace: Namespace}, report.Phase1.Missing[0])
	assert.True(t, report.HasDrift(), "missing Ingress on an installed bundle is drift")
}

func TestCheckBundleDrift_RAGBundleAllMissing(t *testing.T) {
	// State ConfigMap is present (operator installed the bundle at
	// some point) but every workload is gone — the AnythingLLM-disappeared
	// scenario captured in the TO-DO that prompted this checker.
	client := fake.NewSimpleClientset(
		bundleStateCM(RAGBundleStateConfigMapName),
	)

	report, err := CheckBundleDrift(context.Background(), client)
	require.NoError(t, err)
	assert.True(t, report.RAG.Installed)
	assert.Len(t, report.RAG.Missing, 3, "all RAG workloads gone — every expected resource should be in Missing")
	assert.True(t, report.HasDrift())

	// Phase 1 was never installed → not drift, not Missing.
	assert.False(t, report.Phase1.Installed)
	assert.Empty(t, report.Phase1.Missing)
}

func TestCheckBundleDrift_BothBundlesHealthy(t *testing.T) {
	// The common production state: both bundles installed and every
	// workload present. No drift.
	client := fake.NewSimpleClientset(
		bundleStateCM(BundleStateConfigMapName),
		bundleStateCM(RAGBundleStateConfigMapName),
		deploy("ollama"),
		deploy("librechat-librechat"),
		ingress("librechat"),
		statefulSet("qdrant"),
		deploy("anythingllm"),
		ingress("anythingllm"),
	)

	report, err := CheckBundleDrift(context.Background(), client)
	require.NoError(t, err)
	assert.True(t, report.Phase1.Installed)
	assert.True(t, report.RAG.Installed)
	assert.False(t, report.HasDrift())
}

func TestBundleStatus_HasDrift(t *testing.T) {
	// Drift is only meaningful when the bundle was installed. A
	// not-installed bundle with no missing workloads must report
	// no drift, never "installed=false with 3 missing" (which would
	// be a logic bug elsewhere).
	cases := []struct {
		name   string
		status BundleStatus
		want   bool
	}{
		{"not installed, nothing missing", BundleStatus{}, false},
		{"installed, nothing missing", BundleStatus{Installed: true}, false},
		{
			"installed, one missing", BundleStatus{
				Installed: true,
				Missing:   []Resource{{Kind: "Ingress", Name: "librechat", Namespace: Namespace}},
			}, true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.status.HasDrift())
		})
	}
}

func bundleStateCM(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace},
		Data:       map[string]string{"installed": "true"},
	}
}

func deploy(name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace},
	}
}

func statefulSet(name string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace},
	}
}

func ingress(name string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace},
	}
}
