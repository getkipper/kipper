package ai

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// hostnameLabel is the well-known Kubernetes node label used as the
// nodeSelector key when Phase 1 pins Ollama to a specific node. Phase 2
// reads it to colocate Qdrant and AnythingLLM with Ollama for fast
// in-cluster embedding traffic.
const hostnameLabel = "kubernetes.io/hostname"

// ollamaDeploymentName is the name the Ollama HelmChart renders inside
// the kipper-ai namespace. Phase 2 needs this to verify Phase 1 is
// installed and Available before allowing a RAG install.
const ollamaDeploymentName = "ollama"

// ragRequiredHeadroom is the additional free RAM Phase 2 requires on
// the node where Phase 1's Ollama is pinned. Phase 2 colocates Qdrant
// and AnythingLLM with Ollama so embedding traffic stays node-local;
// the headroom budget covers the three new memory consumers:
//
//   - Qdrant ~512 MiB resident at a few thousand vectors (mmap-friendly
//     and grows gently; 1M vectors fit in ~135 MiB with full mmap).
//   - AnythingLLM ~2 GiB. The project's documented minimum, and the
//     Node.js process is OOM-prone above the 2 GiB ceiling under agent
//     workloads.
//   - nomic-embed-text loaded in the existing Ollama process: ~1 GiB
//     including the KV cache for the 8K-token context window.
//
// Plus ~512 MiB of slack for query-time spikes. Web-verified
// 2026-05-10 against qdrant.tech, docs.anythingllm.com, and the
// nomic-embed-text Ollama model card.
const ragRequiredHeadroom int64 = 4 * 1024 * 1024 * 1024

// RAGReport summarises whether the cluster can host the Phase 2 RAG
// bundle. Eligibility requires Phase 1 already installed (Ollama
// Deployment present and Available) and at least ragRequiredHeadroom
// of free memory on the node Phase 1 is pinned to.
type RAGReport struct {
	// Phase1Installed is true when the Ollama Deployment exists in the
	// kipper-ai namespace and reports Available. False covers both
	// "namespace missing" and "Ollama not yet ready".
	Phase1Installed bool
	// Phase1Reason explains why Phase1Installed is false. Empty when
	// Phase 1 is installed.
	Phase1Reason string
	// TargetNode is the node Phase 1 pinned Ollama to. Phase 2's
	// installer will use the same nodeSelector. Empty when Phase 1 is
	// not installed or has no nodeSelector.
	TargetNode string
	// TargetFreeMemory is the free RAM on TargetNode after subtracting
	// every running pod's memory request, including Phase 1's Ollama.
	// Zero when Phase 1 is not installed or TargetNode is not Ready.
	TargetFreeMemory int64
	// RequiredHeadroomBytes is the constant ragRequiredHeadroom, copied
	// in so callers can show a "X required vs Y available" comparison
	// without importing the constant.
	RequiredHeadroomBytes int64
	// HeadroomSufficient is true when TargetFreeMemory meets or exceeds
	// RequiredHeadroomBytes.
	HeadroomSufficient bool
}

// Eligible reports whether DetectRAG considers the cluster capable of
// hosting the Phase 2 RAG bundle.
func (r RAGReport) Eligible() bool {
	return r.Phase1Installed && r.HeadroomSufficient
}

// DetectRAG inspects the cluster and reports whether a Phase 2 RAG
// install can proceed. Two checks must pass:
//
//   - Phase 1 is installed: the Ollama Deployment in the kipper-ai
//     namespace exists and is Available. RAG without an LLM is
//     meaningless.
//   - The node Phase 1 pinned Ollama to has at least
//     ragRequiredHeadroom of free memory after subtracting every
//     running pod's memory request.
//
// On a Phase 1 misconfiguration (Ollama Deployment missing the
// kubernetes.io/hostname nodeSelector, or pinned to a node that no
// longer schedules), DetectRAG returns an error rather than a
// not-eligible report; those are install bugs, not capacity decisions.
func DetectRAG(ctx context.Context, client kubernetes.Interface) (RAGReport, error) {
	report := RAGReport{RequiredHeadroomBytes: ragRequiredHeadroom}

	dep, err := client.AppsV1().Deployments(Namespace).Get(ctx, ollamaDeploymentName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		report.Phase1Reason = fmt.Sprintf(
			"deployment %s/%s not found; run 'kip ai install' before installing the RAG bundle",
			Namespace, ollamaDeploymentName,
		)
		return report, nil
	}
	if err != nil {
		return report, fmt.Errorf("reading deployment %s/%s: %w", Namespace, ollamaDeploymentName, err)
	}
	if !isDeploymentAvailable(dep) {
		report.Phase1Reason = fmt.Sprintf(
			"deployment %s/%s is not Available; wait for Phase 1 to finish rolling out before installing the RAG bundle",
			Namespace, ollamaDeploymentName,
		)
		return report, nil
	}
	report.Phase1Installed = true

	report.TargetNode = dep.Spec.Template.Spec.NodeSelector[hostnameLabel]
	if report.TargetNode == "" {
		return report, fmt.Errorf(
			"deployment %s/%s has no %s nodeSelector; cannot determine where to colocate the RAG bundle",
			Namespace, ollamaDeploymentName, hostnameLabel,
		)
	}

	cluster, err := Detect(ctx, client)
	if err != nil {
		return report, err
	}
	for _, n := range cluster.Nodes {
		if n.Name == report.TargetNode {
			report.TargetFreeMemory = n.FreeMemory
			report.HeadroomSufficient = n.FreeMemory >= ragRequiredHeadroom
			return report, nil
		}
	}

	return report, fmt.Errorf(
		"deployment %s/%s is pinned to node %q, but the node is not Ready or schedulable",
		Namespace, ollamaDeploymentName, report.TargetNode,
	)
}
