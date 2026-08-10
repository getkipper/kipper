// Package ai detects drift on the Kipper AI bundles. The kip CLI
// records each installed bundle in a ConfigMap; the drift checker
// reads those ConfigMaps and confirms every workload the bundle
// requires is still on the cluster.
//
// Today's diagnosis when AnythingLLM's Deployment + Ingress went
// missing on an AI cluster took ssh + `kubectl get` to spot. The
// drift report is what the console surface and any future alerting
// rule read so the gap shows up without leaving the dashboard.
package ai

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Namespace holds every Kipper AI bundle workload. Mirrors the
// constant in `kip/internal/ai/manifests.go` — duplicated here so
// console-api can read it without importing the kip CLI module.
const Namespace = "kipper-ai"

// Bundle-state ConfigMap names. Their presence means the operator
// ran `kip ai install` / `kip ai install --rag`; that's the signal
// the drift checker uses to decide whether each bundle's workloads
// should exist.
const (
	BundleStateConfigMapName    = "kipper-ai-bundle-state"
	RAGBundleStateConfigMapName = "kipper-rag-bundle-state"
)

// Resource identifies one Kubernetes object the drift checker
// expects to find when a given bundle is installed. Kept simple —
// kind, name, namespace — so the report can name what's gone
// without depending on apimachinery types in the wire format.
type Resource struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// BundleStatus is the drift result for one bundle.
//   - Installed=false means the bundle's state ConfigMap was not
//     found; the operator never ran the install command for it.
//     Missing is meaningless in that case.
//   - Installed=true means the bundle should be running. Missing
//     lists every expected workload that no longer exists.
type BundleStatus struct {
	Installed bool       `json:"installed"`
	Missing   []Resource `json:"missing"`
}

// HasDrift returns true when an installed bundle has lost at least
// one workload. Used by the console as the alert trigger.
func (s BundleStatus) HasDrift() bool {
	return s.Installed && len(s.Missing) > 0
}

// Report is the full drift result returned to callers.
type Report struct {
	Phase1 BundleStatus `json:"phase1"`
	RAG    BundleStatus `json:"rag"`
}

// HasDrift returns true when any installed bundle has missing
// workloads.
func (r Report) HasDrift() bool {
	return r.Phase1.HasDrift() || r.RAG.HasDrift()
}

// CheckBundleDrift inspects the cluster's `kipper-ai` namespace and
// returns the drift report. Each bundle's state ConfigMap acts as
// the "should be installed" signal; the expected workload list per
// bundle is hard-coded (it tracks the manifests in
// `kip/internal/ai/manifests.go` and `rag_manifests.go`).
//
// The function never errors on a missing workload — that's drift,
// reported in `Missing`. It only errors when the apiserver itself
// is unreachable, since the caller wants to distinguish "checked
// and found drift" from "could not check".
func CheckBundleDrift(ctx context.Context, client kubernetes.Interface) (Report, error) {
	var report Report

	phase1Installed, err := configMapExists(ctx, client, Namespace, BundleStateConfigMapName)
	if err != nil {
		return report, fmt.Errorf("checking phase 1 bundle state: %w", err)
	}
	if phase1Installed {
		report.Phase1.Installed = true
		report.Phase1.Missing, err = findMissing(ctx, client, phase1ExpectedResources())
		if err != nil {
			return report, err
		}
	}

	ragInstalled, err := configMapExists(ctx, client, Namespace, RAGBundleStateConfigMapName)
	if err != nil {
		return report, fmt.Errorf("checking RAG bundle state: %w", err)
	}
	if ragInstalled {
		report.RAG.Installed = true
		report.RAG.Missing, err = findMissing(ctx, client, ragExpectedResources())
		if err != nil {
			return report, err
		}
	}

	return report, nil
}

// phase1ExpectedResources lists every workload the Phase 1 install
// (LibreChat + Ollama) leaves on the cluster. The HelmCharts live in
// kube-system but their workloads land in kipper-ai — checking the
// workloads is what tells us whether helm-controller actually
// reconciled the chart.
func phase1ExpectedResources() []Resource {
	return []Resource{
		{Kind: "Deployment", Name: "ollama", Namespace: Namespace},
		{Kind: "Deployment", Name: "librechat-librechat", Namespace: Namespace},
		{Kind: "Ingress", Name: "librechat", Namespace: Namespace},
	}
}

// ragExpectedResources lists every workload the RAG install
// (AnythingLLM + Qdrant) leaves on the cluster. The chart-rendered
// names are stable per `kip/internal/ai/rag_manifests.go`.
func ragExpectedResources() []Resource {
	return []Resource{
		{Kind: "StatefulSet", Name: "qdrant", Namespace: Namespace},
		{Kind: "Deployment", Name: "anythingllm", Namespace: Namespace},
		{Kind: "Ingress", Name: "anythingllm", Namespace: Namespace},
	}
}

// findMissing returns the subset of expected resources that do not
// exist on the cluster. A NotFound is the only "missing" signal; any
// other apiserver error stops the check and is returned to the
// caller — partial reports would mis-classify drift.
func findMissing(ctx context.Context, client kubernetes.Interface, expected []Resource) ([]Resource, error) {
	var missing []Resource
	for _, r := range expected {
		ok, err := resourceExists(ctx, client, r)
		if err != nil {
			return nil, err
		}
		if !ok {
			missing = append(missing, r)
		}
	}
	return missing, nil
}

func resourceExists(ctx context.Context, client kubernetes.Interface, r Resource) (bool, error) {
	switch r.Kind {
	case "Deployment":
		_, err := client.AppsV1().Deployments(r.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
		return notFoundOrErr(err)
	case "StatefulSet":
		_, err := client.AppsV1().StatefulSets(r.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
		return notFoundOrErr(err)
	case "Ingress":
		_, err := client.NetworkingV1().Ingresses(r.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
		return notFoundOrErr(err)
	default:
		return false, fmt.Errorf("ai drift: unknown resource kind %q", r.Kind)
	}
}

func notFoundOrErr(err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

func configMapExists(ctx context.Context, client kubernetes.Interface, namespace, name string) (bool, error) {
	_, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}
