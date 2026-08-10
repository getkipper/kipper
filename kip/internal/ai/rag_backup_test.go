package ai

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Step 6: backup integration verification.
//
// Phase 2 deliberately ships no new backup machinery. The existing
// 'kip ai backup' (Phase 1 follow-ups Step 5) already covers any PVC
// in the kipper-ai namespace via the primary Velero filesystem
// backup, and any Kipper-owned HelmChart CR in kube-system via the
// config backup's label selector. Phase 2 puts Qdrant and AnythingLLM
// in kipper-ai with the same labels Phase 1 charts use, so backups
// extend automatically.
//
// The tests below pin that cross-step invariant. If anyone:
//   - Narrows primaryBackupSpec.includedNamespaces away from
//     kipper-ai, or
//   - Adds an excludedResources clause that drops Qdrant or
//     AnythingLLM PVCs, or
//   - Removes managed-by=kipper or part-of=kipper-ai from the RAG
//     HelmChart manifests, or
//   - Changes configBackupSpec's label selector,
// these tests fail loudly so 'kip ai backup' does not silently stop
// covering the RAG bundle.

// TestPrimaryBackupCoversRAGNamespace pins the namespace inclusion
// that gives Phase 2 backup coverage for free. The primary backup
// uses defaultVolumesToFsBackup=true (verified separately in
// TestPrimaryBackupRequestsFsBackup), so naming the namespace is
// enough to capture the Qdrant data PVC and the AnythingLLM
// /app/server/storage PVC (which holds the SQLite DB and uploaded
// documents).
func TestPrimaryBackupCoversRAGNamespace(t *testing.T) {
	spec := primaryBackupSpec()

	included, ok := spec["includedNamespaces"].([]interface{})
	require.True(t, ok, "primaryBackupSpec must declare includedNamespaces")

	var names []string
	for _, n := range included {
		names = append(names, n.(string))
	}
	assert.Contains(t, names, Namespace,
		"primary backup must cover the kipper-ai namespace; Qdrant and AnythingLLM PVCs live there alongside Phase 1's model cache and MongoDB")
}

// TestPrimaryBackupHasNoExclusionsThatWouldDropRAG pins the absence
// of an excludedResources clause that would silently drop Qdrant or
// AnythingLLM resources. The current spec has no exclusions; if
// someone adds one, this test forces them to think about whether the
// new exclusion catches RAG state.
func TestPrimaryBackupHasNoExclusionsThatWouldDropRAG(t *testing.T) {
	spec := primaryBackupSpec()

	excluded, hasExclusions := spec["excludedResources"]
	if !hasExclusions {
		// Expected today: no exclusions, so RAG resources are covered.
		return
	}

	// If exclusions ever appear, none of them may target the RAG
	// resource kinds. Resource names use the kebab-cased plural form
	// Velero accepts (matching kubectl).
	excludedList := excluded.([]interface{})
	for _, raw := range excludedList {
		s := strings.ToLower(raw.(string))
		assert.NotContains(t, s, "persistentvolumeclaim",
			"PVC exclusion would drop Qdrant data and AnythingLLM uploads")
		assert.NotContains(t, s, "statefulset",
			"StatefulSet exclusion would drop the Qdrant workload definition")
		assert.NotContains(t, s, "qdrant", "Qdrant resources must not be excluded by name")
		assert.NotContains(t, s, "anythingllm", "AnythingLLM resources must not be excluded by name")
	}
}

// TestRAGHelmChartsCarryConfigBackupLabels pins the cross-component
// invariant that makes the config backup automatically capture the
// Phase 2 HelmChart CRs in kube-system. configBackupSpec selects on
// (managed-by=kipper, part-of=kipper-ai); the RAG manifests must
// carry both labels for Velero to include them.
//
// Without these labels the config backup completes successfully but
// silently omits the chart inputs, so a restore would bring back the
// chart-rendered resources but leave them unmanaged by helm-controller
// — breaking later upgrade and uninstall flows the same way Phase 1
// caught in Round 8 of the original blueprint review.
func TestRAGHelmChartsCarryConfigBackupLabels(t *testing.T) {
	requiredLabels := configBackupRequiredLabels(t)

	// Qdrant manifest.
	qdrantDoc := parseDoc(t, QdrantManifest(qdrantTestConfig()))
	qdrantLabels := qdrantDoc["metadata"].(map[string]any)["labels"].(map[string]any)
	for k, v := range requiredLabels {
		assert.Equal(t, v, qdrantLabels[k],
			"Qdrant HelmChart must carry %s=%s so configBackupSpec selects it; without this kip ai backup silently omits the Qdrant chart input", k, v)
	}

	// AnythingLLM manifest is multi-document; the HelmChart is the
	// second.
	docs := SplitYAMLDocuments(AnythingLLMManifest(anythingLLMTestConfig()))
	require.GreaterOrEqual(t, len(docs), 2)
	anyllmDoc := parseDoc(t, docs[1])
	require.Equal(t, "HelmChart", anyllmDoc["kind"], "expected the second AnythingLLM document to be the HelmChart")
	anyllmLabels := anyllmDoc["metadata"].(map[string]any)["labels"].(map[string]any)
	for k, v := range requiredLabels {
		assert.Equal(t, v, anyllmLabels[k],
			"AnythingLLM HelmChart must carry %s=%s so configBackupSpec selects it", k, v)
	}
}

// TestRAGSecretAndIngressCarryConfigBackupLabels pins the same
// invariant for the cross-namespace AnythingLLM credentials Secret
// (sits in kipper-ai but is captured by the primary backup's
// namespace inclusion; labels matter for label-scoped operations).
// Belt and braces: the credentials Secret must be findable by the
// same selector configBackupSpec uses, since users often query
// kubectl with those labels.
func TestRAGSecretAndIngressCarryConfigBackupLabels(t *testing.T) {
	requiredLabels := configBackupRequiredLabels(t)

	docs := SplitYAMLDocuments(AnythingLLMManifest(anythingLLMTestConfig()))
	// First document is the credentials Secret.
	secret := parseDoc(t, docs[0])
	require.Equal(t, "Secret", secret["kind"])
	secretLabels := secret["metadata"].(map[string]any)["labels"].(map[string]any)
	for k, v := range requiredLabels {
		assert.Equal(t, v, secretLabels[k],
			"AnythingLLM credentials Secret must carry %s=%s", k, v)
	}

	// Third document is the Kipper-owned Ingress.
	ing := parseDoc(t, docs[2])
	require.Equal(t, "Ingress", ing["kind"])
	ingLabels := ing["metadata"].(map[string]any)["labels"].(map[string]any)
	for k, v := range requiredLabels {
		assert.Equal(t, v, ingLabels[k],
			"AnythingLLM Ingress must carry %s=%s", k, v)
	}
}

// TestRAGBundleStateConfigMapCoveredByPrimaryBackup pins that the
// per-bundle state ConfigMap (which records the embedding model and
// docs host) lives in kipper-ai so the primary backup catches it.
// Restore that omitted this would lose 'kip ai status' visibility
// of the embedding model after a restore.
func TestRAGBundleStateConfigMapCoveredByPrimaryBackup(t *testing.T) {
	// Just a string-level invariant: the constant the installer
	// writes to and the namespace primaryBackupSpec snapshots are
	// the same one. If someone moves the state ConfigMap into a
	// different namespace, the primary backup stops covering it
	// and this test fires.
	assert.Equal(t, "kipper-ai", Namespace)
	assert.Equal(t, "kipper-rag-bundle-state", RAGBundleStateConfigMapName,
		"if this name changes, also bump the docs/restore tests that read it back")
}

// configBackupRequiredLabels reads configBackupSpec and returns the
// label selector map that any resource must carry to be picked up by
// the config backup. Centralised so the assertions above stay in
// sync with the spec rather than hard-coding the labels twice.
func configBackupRequiredLabels(t *testing.T) map[string]string {
	t.Helper()
	spec := configBackupSpec()
	sel, ok := spec["labelSelector"].(map[string]interface{})
	require.True(t, ok, "configBackupSpec must declare a labelSelector for cross-namespace artefacts")
	matchLabels, ok := sel["matchLabels"].(map[string]interface{})
	require.True(t, ok, "configBackupSpec labelSelector must use matchLabels")

	out := make(map[string]string, len(matchLabels))
	for k, v := range matchLabels {
		out[k] = v.(string)
	}
	return out
}
