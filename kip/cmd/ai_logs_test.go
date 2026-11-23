package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/kip/internal/ai"
)

func TestAILogsPrintsTheComponentsPods(t *testing.T) {
	labels := map[string]string{"app.kubernetes.io/name": "ollama"}
	client := fake.NewSimpleClientset( //nolint:staticcheck
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "ollama", Namespace: ai.Namespace},
			Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "ollama-5d8", Namespace: ai.Namespace, Labels: labels},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "ollama"}}},
		},
	)
	var out, errOut bytes.Buffer

	err := aiLogs(context.Background(), client, "ollama", false, logRequest{Tail: 10}, &out, &errOut)

	require.NoError(t, err)
	assert.Contains(t, out.String(), "fake logs")
	assert.Empty(t, errOut.String())
}

func TestAILogsIsAnAICommand(t *testing.T) {
	found, _, err := aiCmd.Find([]string{"logs"})

	require.NoError(t, err)
	assert.Equal(t, "logs <component>", found.Use)
	assert.Equal(t, ai.LogComponents, found.ValidArgs)
	assert.NotNil(t, found.Flags().Lookup("chart"))
	assert.NotNil(t, found.Flags().Lookup("follow"))
}
