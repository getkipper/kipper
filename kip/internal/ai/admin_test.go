package ai

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// libreChatDeployment returns a fake LibreChat Deployment whose
// selector exercises the dynamic-selector path in pickReadyLibreChatPod.
// The chart we ship picks unusual labels; the test keeps that surprise
// in scope so changes to the helper do not regress against arbitrary
// label sets.
func libreChatDeployment(matchLabels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: LibreChatDeploymentName, Namespace: Namespace},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: matchLabels},
		},
	}
}

func TestValidateAdminOptionsRequiresEmail(t *testing.T) {
	opts := AdminCreateOptions{Password: "longenough", Name: "Alice"}
	err := validateAdminOptions(&opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--email is required")
}

func TestValidateAdminOptionsRejectsBadEmail(t *testing.T) {
	opts := AdminCreateOptions{Email: "notanemail", Password: "longenough", Name: "Alice"}
	err := validateAdminOptions(&opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not look like an email")
}

func TestValidateAdminOptionsRequiresPassword(t *testing.T) {
	opts := AdminCreateOptions{Email: "a@b.c", Name: "Alice"}
	err := validateAdminOptions(&opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--password is required")
}

func TestValidateAdminOptionsRejectsShortPassword(t *testing.T) {
	opts := AdminCreateOptions{Email: "a@b.c", Password: "short", Name: "Alice"}
	err := validateAdminOptions(&opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 8 characters")
}

func TestValidateAdminOptionsRequiresName(t *testing.T) {
	opts := AdminCreateOptions{Email: "a@b.c", Password: "longenough"}
	err := validateAdminOptions(&opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--name is required")
}

func TestValidateAdminOptionsDefaultsUsernameToEmailLocalPart(t *testing.T) {
	opts := AdminCreateOptions{Email: "alice@example.com", Password: "longenough", Name: "Alice"}
	require.NoError(t, validateAdminOptions(&opts))
	assert.Equal(t, "alice", opts.Username)
}

func TestValidateAdminOptionsKeepsExplicitUsername(t *testing.T) {
	opts := AdminCreateOptions{Email: "alice@example.com", Password: "longenough", Name: "Alice", Username: "wonderland"}
	require.NoError(t, validateAdminOptions(&opts))
	assert.Equal(t, "wonderland", opts.Username)
}

func librechatPod(name, phase string, ready bool, labels map[string]string) *corev1.Pod {
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace, Labels: labels},
		Status: corev1.PodStatus{
			Phase:      corev1.PodPhase(phase),
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}},
		},
	}
}

func TestPickReadyLibreChatPodReturnsRunningReadyPod(t *testing.T) {
	// Whatever labels the chart picks, we should find the pod that
	// matches the Deployment's selector.
	matchLabels := map[string]string{"app.something/owner": "librechat-app"}
	clientset := fake.NewSimpleClientset( //nolint:staticcheck
		libreChatDeployment(matchLabels),
		librechatPod("librechat-librechat-abc", "Running", true, matchLabels),
	)
	got, err := pickReadyLibreChatPod(context.Background(), clientset)
	require.NoError(t, err)
	assert.Equal(t, "librechat-librechat-abc", got)
}

func TestPickReadyLibreChatPodErrorsWhenDeploymentMissing(t *testing.T) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck
	_, err := pickReadyLibreChatPod(context.Background(), clientset)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is the bundle installed")
}

func TestPickReadyLibreChatPodSkipsPendingPods(t *testing.T) {
	matchLabels := map[string]string{"foo": "bar"}
	clientset := fake.NewSimpleClientset( //nolint:staticcheck
		libreChatDeployment(matchLabels),
		librechatPod("librechat-librechat-abc", "Pending", false, matchLabels),
	)
	_, err := pickReadyLibreChatPod(context.Background(), clientset)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no Ready librechat pod")
}

func TestPickReadyLibreChatPodSkipsRunningButNotReadyPods(t *testing.T) {
	matchLabels := map[string]string{"foo": "bar"}
	clientset := fake.NewSimpleClientset( //nolint:staticcheck
		libreChatDeployment(matchLabels),
		librechatPod("librechat-librechat-abc", "Running", false, matchLabels),
	)
	_, err := pickReadyLibreChatPod(context.Background(), clientset)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no Ready librechat pod")
}

func TestPickReadyLibreChatPodIgnoresPodsWithoutMatchingLabels(t *testing.T) {
	// Mongo / Meilisearch pods in the same namespace but with different
	// labels must not be picked.
	matchLabels := map[string]string{"role": "librechat-web"}
	mongoLabels := map[string]string{"role": "mongodb"}
	clientset := fake.NewSimpleClientset( //nolint:staticcheck
		libreChatDeployment(matchLabels),
		librechatPod("librechat-mongodb-xyz", "Running", true, mongoLabels),
	)
	_, err := pickReadyLibreChatPod(context.Background(), clientset)
	require.Error(t, err, "mongodb pod must not be picked as the librechat pod")
}
