package ai

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func labeledPod(namespace, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}}
}

func selectorDeployment(name string, labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
	}
}

func selectorJob(namespace, name string, created time.Time, labels map[string]string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, CreationTimestamp: metav1.NewTime(created)},
		Spec:       batchv1.JobSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
	}
}

func podNames(pods []corev1.Pod) []string {
	names := make([]string, 0, len(pods))
	for _, p := range pods {
		names = append(names, p.Name)
	}
	return names
}

func TestLogPodsUsesTheWorkloadSelector(t *testing.T) {
	mongo := map[string]string{"app.kubernetes.io/name": "mongodb", "app.kubernetes.io/instance": "librechat"}
	client := fake.NewSimpleClientset( //nolint:staticcheck
		selectorDeployment("librechat-mongodb", mongo),
		labeledPod(Namespace, "librechat-mongodb-7c9", mongo),
		labeledPod(Namespace, "librechat-librechat-5f2", map[string]string{"app.kubernetes.io/name": "librechat", "app.kubernetes.io/instance": "librechat"}),
	)

	ns, pods, err := LogPods(context.Background(), client, "mongodb", false)

	require.NoError(t, err)
	assert.Equal(t, Namespace, ns)
	assert.Equal(t, []string{"librechat-mongodb-7c9"}, podNames(pods))
}

func TestLogPodsReadsAStatefulSet(t *testing.T) {
	labels := map[string]string{"app": "qdrant"}
	client := fake.NewSimpleClientset( //nolint:staticcheck
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: QdrantServiceName, Namespace: Namespace},
			Spec:       appsv1.StatefulSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
		},
		labeledPod(Namespace, "qdrant-0", labels),
	)

	_, pods, err := LogPods(context.Background(), client, "qdrant", false)

	require.NoError(t, err)
	assert.Equal(t, []string{"qdrant-0"}, podNames(pods))
}

func TestLogPodsReadsTheChartJob(t *testing.T) {
	job := map[string]string{"batch.kubernetes.io/controller-uid": "u-1"}
	client := fake.NewSimpleClientset( //nolint:staticcheck
		selectorJob("kube-system", "helm-install-librechat", time.Now(), job),
		labeledPod("kube-system", "helm-install-librechat-x2", job),
	)

	ns, pods, err := LogPods(context.Background(), client, "meilisearch", true)

	require.NoError(t, err)
	assert.Equal(t, "kube-system", ns)
	assert.Equal(t, []string{"helm-install-librechat-x2"}, podNames(pods))
}

func TestLogPodsReadsTheNewestOneShotJob(t *testing.T) {
	older := map[string]string{"batch.kubernetes.io/controller-uid": "old"}
	newer := map[string]string{"batch.kubernetes.io/controller-uid": "new"}
	now := time.Now()
	client := fake.NewSimpleClientset( //nolint:staticcheck
		selectorJob(Namespace, modelProbeNamePrefix+"aaaaa", now.Add(-time.Hour), older),
		selectorJob(Namespace, modelProbeNamePrefix+"bbbbb", now, newer),
		selectorJob(Namespace, embeddingPullJobNamePrefix+"ccccc", now.Add(time.Minute), map[string]string{"x": "y"}),
		labeledPod(Namespace, "probe-old", older),
		labeledPod(Namespace, "probe-new", newer),
	)

	_, pods, err := LogPods(context.Background(), client, "model-probe", false)

	require.NoError(t, err)
	assert.Equal(t, []string{"probe-new"}, podNames(pods))
}

func TestLogPodsExplainsWhatIsMissing(t *testing.T) {
	for name, tc := range map[string]struct {
		component string
		chart     bool
		objects   []runtime.Object
		want      string
	}{
		"an unknown component":          {component: "redis", want: `unknown component "redis"`},
		"a chart for a one-shot job":    {component: "model-probe", chart: true, want: "model-probe has no chart"},
		"a workload not installed":      {component: "anythingllm", want: "AnythingLLM is not installed"},
		"a chart without a job":         {component: "ollama", chart: true, want: "no Helm job for the ollama chart"},
		"a one-shot job that never ran": {component: "embedding-pull", want: "no embedding-pull job"},
		"a workload without pods": {component: "ollama", objects: []runtime.Object{selectorDeployment(ollamaDeploymentName, map[string]string{"app": "ollama"})},
			want: "no pods for ollama"},
	} {
		t.Run(name, func(t *testing.T) {
			client := fake.NewSimpleClientset(tc.objects...) //nolint:staticcheck

			_, _, err := LogPods(context.Background(), client, tc.component, tc.chart)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestOneShotJobsKeepTheirLogsForAnHour checks that finished jobs remain available
// long enough to follow the install error's 'kip ai logs model-probe' or
// 'kip ai logs embedding-pull' instructions. The console API can repeat alerts
// for failed Kipper jobs after a six-hour cooldown while they still exist, even
// if the underlying problem has been fixed. The one-hour TTL avoids those repeats.
func TestOneShotJobsKeepTheirLogsForAnHour(t *testing.T) {
	for name, job := range map[string]*batchv1.Job{
		"model-probe":    buildModelProbeJob("qwen2.5:0.5b"),
		"embedding-pull": buildEmbeddingPullJob(DefaultEmbeddingModel),
	} {
		require.NotNil(t, job.Spec.TTLSecondsAfterFinished, name)
		assert.Equal(t, int32(oneShotJobTTL/time.Second), *job.Spec.TTLSecondsAfterFinished, name)
	}
	assert.Equal(t, time.Hour, oneShotJobTTL)
}
