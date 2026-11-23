package ai

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// oneShotJobTTL is how long a finished model-probe or embedding-pull job and its
// pod stay in the cluster, so 'kip ai logs' can still show why one failed.
const oneShotJobTTL = time.Hour

// LogComponents lists the components 'kip ai logs' accepts, in help order.
var LogComponents = []string{"ollama", "librechat", "mongodb", "meilisearch", "qdrant", "anythingllm", "model-probe", "embedding-pull"}

// logTarget is a long-running workload, with the chart that installs it, or
// a one-shot job identified by its name prefix.
type logTarget struct {
	title, kind, name, chart, jobPrefix string
}

var logTargets = map[string]logTarget{
	"ollama":         {title: "Ollama", kind: "Deployment", name: ollamaDeploymentName, chart: "ollama"},
	"librechat":      {title: "LibreChat", kind: "Deployment", name: LibreChatDeploymentName, chart: "librechat"},
	"mongodb":        {title: "LibreChat's MongoDB", kind: "Deployment", name: "librechat-mongodb", chart: "librechat"},
	"meilisearch":    {title: "LibreChat's Meilisearch", kind: "StatefulSet", name: "librechat-meilisearch", chart: "librechat"},
	"qdrant":         {title: "Qdrant", kind: "StatefulSet", name: QdrantServiceName, chart: "qdrant"},
	"anythingllm":    {title: "AnythingLLM", kind: "Deployment", name: AnythingLLMDeploymentName, chart: "anythingllm"},
	"model-probe":    {jobPrefix: modelProbeNamePrefix},
	"embedding-pull": {jobPrefix: embeddingPullJobNamePrefix},
}

// LogPods returns a component's namespace and pods sorted by name, using the
// workload's pod selector in Namespace. For model-probe and embedding-pull, it
// uses the newest existing job by creation time and that job's pod selector.
// If chart is true, it uses the component's helm-install-<chart> job and its
// pod selector in kube-system.
// It returns an error for unknown components, components without a chart when
// chart is true, missing workloads or jobs, empty pod lists, invalid selectors,
// and Kubernetes API failures. Errors identify the failed lookup or missing resource.
func LogPods(ctx context.Context, client kubernetes.Interface, component string, chart bool) (string, []corev1.Pod, error) {
	target, ok := logTargets[component]
	if !ok {
		return "", nil, fmt.Errorf("unknown component %q. Use one of %s", component, strings.Join(LogComponents, ", "))
	}
	switch {
	case chart && target.chart == "":
		return "", nil, fmt.Errorf("%s has no chart. Its logs come from a job kip starts, so run 'kip ai logs %s'", component, component)
	case chart:
		job, err := client.BatchV1().Jobs("kube-system").Get(ctx, "helm-install-"+target.chart, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return "", nil, fmt.Errorf("there is no Helm job for the %s chart. The Helm controller creates one to install or upgrade the chart", target.chart)
		}
		if err != nil {
			return "", nil, fmt.Errorf("reading the Helm job for the %s chart: %w", target.chart, err)
		}
		return podsFor(ctx, client, "kube-system", job.Spec.Selector, "the Helm job for the "+target.chart+" chart")
	case target.jobPrefix != "":
		return newestJobPods(ctx, client, component, target.jobPrefix)
	}
	selector, err := workloadSelector(ctx, client, target)
	if apierrors.IsNotFound(err) {
		return "", nil, fmt.Errorf("%s is not installed in this cluster", target.title)
	}
	if err != nil {
		return "", nil, fmt.Errorf("reading %s: %w", target.title, err)
	}
	return podsFor(ctx, client, Namespace, selector, component)
}

func workloadSelector(ctx context.Context, client kubernetes.Interface, t logTarget) (*metav1.LabelSelector, error) {
	if t.kind == "StatefulSet" {
		set, err := client.AppsV1().StatefulSets(Namespace).Get(ctx, t.name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return set.Spec.Selector, nil
	}
	deployment, err := client.AppsV1().Deployments(Namespace).Get(ctx, t.name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return deployment.Spec.Selector, nil
}

func newestJobPods(ctx context.Context, client kubernetes.Interface, component, prefix string) (string, []corev1.Pod, error) {
	jobs, err := client.BatchV1().Jobs(Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", nil, fmt.Errorf("listing the %s jobs: %w", component, err)
	}
	var newest *metav1.LabelSelector
	var newestAt metav1.Time
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if strings.HasPrefix(j.Name, prefix) && (newest == nil || newestAt.Before(&j.CreationTimestamp)) {
			newest, newestAt = j.Spec.Selector, j.CreationTimestamp
		}
	}
	if newest == nil {
		return "", nil, fmt.Errorf("there is no %s job in %s. The kip CLI starts these jobs during 'kip ai install' or 'kip ai rag install'", component, Namespace)
	}
	return podsFor(ctx, client, Namespace, newest, "the newest "+component+" job")
}

func podsFor(ctx context.Context, client kubernetes.Interface, namespace string, selector *metav1.LabelSelector, what string) (string, []corev1.Pod, error) {
	s, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return "", nil, fmt.Errorf("reading the pod selector of %s: %w", what, err)
	}
	list, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: s.String()})
	if err != nil {
		return "", nil, fmt.Errorf("listing the pods of %s: %w", what, err)
	}
	if len(list.Items) == 0 {
		return "", nil, fmt.Errorf("there are no pods for %s right now", what)
	}
	sort.Slice(list.Items, func(a, b int) bool { return list.Items[a].Name < list.Items[b].Name })
	return namespace, list.Items, nil
}
