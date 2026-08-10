package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/kip/internal/ai"
	"github.com/getkipper/kipper/kip/internal/config"
)

func TestDefaultAskHost(t *testing.T) {
	tests := []struct {
		name    string
		domain  string
		want    string
		wantErr bool
	}{
		{"custom domain", "example.com", "ask.example.com", false},
		{"kipper.run uses double-dash subdomain", "myapp.kipper.run", "ask--myapp.kipper.run", false},
		{"empty domain errors", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := defaultAskHost(&config.Cluster{Domain: tt.domain})
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPrintRAGBootstrapInstructions(t *testing.T) {
	out := &bytes.Buffer{}
	const token = "abcdef0123456789abcdef0123456789"
	printRAGBootstrapInstructions(out, token)

	s := out.String()
	assert.Contains(t, s, token,
		"the AUTH_TOKEN must appear verbatim so the operator can copy it")
	assert.Contains(t, s, "Bootstrap authentication token")
	assert.Contains(t, s, "scanner",
		"the rationale (race against an internet scanner) belongs in the printed text so the operator understands why the token matters")
	assert.Contains(t, s, "kubectl edit",
		"the rotation path must be printed so users know how to change the token")
	assert.Contains(t, s, "kubectl rollout restart",
		"editing the Secret alone does not update the running pod, so the rotation sequence must include the restart")
	assert.Contains(t, s, ai.AnythingLLMCredentialsSecretName,
		"the rotation hint must point at the actual Secret name, not a placeholder")
	assert.Contains(t, s, ai.AnythingLLMDeploymentName,
		"the restart hint must name the actual Deployment so the operator can copy-paste")
	assert.NotContains(t, s, "remove",
		"removing the token through this command is unsupported, so the output must not suggest a removal path")
}

// TestPrintRAGBundleStatusQuietWhenUninstalled confirms the status
// section stays silent when the AnythingLLM Deployment is absent so
// Phase-1-only users do not see RAG noise.
func TestPrintRAGBundleStatusQuietWhenUninstalled(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	out := &bytes.Buffer{}
	printRAGBundleStatus(out, context.Background(), clientset)
	assert.Empty(t, out.String(), "no output expected when the RAG Deployment is absent")
}

func TestPrintRAGBundleStatusReportsHealthWhenInstalled(t *testing.T) {
	one := int32(1)
	clientset := fake.NewSimpleClientset( //nolint:staticcheck
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: ai.AnythingLLMDeploymentName, Namespace: ai.Namespace},
			Spec:       appsv1.DeploymentSpec{Replicas: &one},
			Status: appsv1.DeploymentStatus{
				ReadyReplicas: 1, Replicas: 1,
				Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}},
			},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: ai.QdrantServiceName, Namespace: ai.Namespace},
			Spec:       appsv1.StatefulSetSpec{Replicas: &one},
			Status: appsv1.StatefulSetStatus{
				ReadyReplicas:   1,
				CurrentRevision: "rev-1", UpdateRevision: "rev-1",
			},
		},
		// ConfigMap intentionally omits the embedding_model key so
		// bundleModelState short-circuits with "not configured"
		// before reaching the RESTClient.Get() call. The fake
		// clientset's RESTClient is nil and will panic; the live
		// model probe is exercised only on a real cluster.
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: ai.RAGBundleStateConfigMapName, Namespace: ai.Namespace},
			Data:       map[string]string{"host": "ask.example.com"},
		},
		&networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: "anythingllm", Namespace: ai.Namespace},
			Spec: networkingv1.IngressSpec{
				Rules: []networkingv1.IngressRule{{Host: "ask.example.com"}},
			},
		},
	)
	out := &bytes.Buffer{}
	printRAGBundleStatus(out, context.Background(), clientset)
	s := out.String()
	assert.Contains(t, s, "RAG bundle:")
	assert.Contains(t, s, "qdrant")
	assert.Contains(t, s, ai.AnythingLLMDeploymentName)
	assert.Contains(t, s, "Ask URL: https://ask.example.com")
}

func TestBundleStatefulSetStateNotDeployed(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	got := bundleStatefulSetState(context.Background(), clientset, ai.QdrantServiceName)
	assert.Contains(t, got, "not deployed")
}

func TestBundleStatefulSetStateRollingOut(t *testing.T) {
	one := int32(1)
	clientset := fake.NewSimpleClientset(&appsv1.StatefulSet{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: ai.QdrantServiceName, Namespace: ai.Namespace},
		Spec:       appsv1.StatefulSetSpec{Replicas: &one},
		Status: appsv1.StatefulSetStatus{
			ReadyReplicas: 0, CurrentRevision: "old", UpdateRevision: "new",
		},
	})
	got := bundleStatefulSetState(context.Background(), clientset, ai.QdrantServiceName)
	assert.Contains(t, got, "rolling out")
}

func TestBundleAskHostReadsIngress(t *testing.T) {
	clientset := fake.NewSimpleClientset(&networkingv1.Ingress{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "anythingllm", Namespace: ai.Namespace},
		Spec:       networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "ask.example.com"}}},
	})
	assert.Equal(t, "ask.example.com", bundleAskHost(context.Background(), clientset))
}

func TestBundleAskHostEmptyWhenAbsent(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	assert.Empty(t, bundleAskHost(context.Background(), clientset))
}
