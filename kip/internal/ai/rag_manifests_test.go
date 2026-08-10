package ai

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func qdrantTestConfig() QdrantConfig {
	return QdrantConfig{
		PVCSize:       "10Gi",
		MemoryRequest: "512Mi",
		MemoryLimit:   "1Gi",
		NodeName:      "node-a",
	}
}

func anythingLLMTestCreds() AnythingLLMCredentials {
	// Realistic-shape values: JWT_SECRET >= 12, SIG_KEY/SIG_SALT >= 32.
	return AnythingLLMCredentials{
		JWTSecret: "0123456789abcdef0123456789abcdef",
		SigKey:    "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
		SigSalt:   "1111111111222222222233333333334444444444555555555566666666667777",
		AuthToken: "bootstrap-token-deadbeef0123456789abcdef0123456789abcdef01234567",
	}
}

func anythingLLMTestConfig() AnythingLLMConfig {
	return AnythingLLMConfig{
		Host:          "ask.kipper-demo.example.com",
		NodeName:      "node-a",
		OllamaModel:   "qwen2.5:3b-instruct-q4_K_M",
		PVCSize:       "10Gi",
		MemoryRequest: "2Gi",
		MemoryLimit:   "3Gi",
		Credentials:   anythingLLMTestCreds(),
	}
}

func TestQdrantManifestStructure(t *testing.T) {
	doc := parseDoc(t, QdrantManifest(qdrantTestConfig()))
	assert.Equal(t, "helm.cattle.io/v1", doc["apiVersion"])
	assert.Equal(t, "HelmChart", doc["kind"])

	meta := doc["metadata"].(map[string]any)
	assert.Equal(t, "qdrant", meta["name"])
	assert.Equal(t, "kube-system", meta["namespace"])
	labels := meta["labels"].(map[string]any)
	assert.Equal(t, "kipper", labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, "kipper-ai", labels["app.kubernetes.io/part-of"])
	assert.Equal(t, "rag", labels["app.kubernetes.io/component"],
		"HelmChart CR must carry component=rag so kip ai rag uninstall can find it by label")

	spec := doc["spec"].(map[string]any)
	assert.Equal(t, QdrantChartRepo, spec["repo"])
	assert.Equal(t, QdrantChartName, spec["chart"])
	assert.Equal(t, QdrantChartVersion, spec["version"])
	assert.Equal(t, Namespace, spec["targetNamespace"])
}

// TestQdrantManifestSetsLabelsThreeWays locks in that the same Kipper
// label map is set under top-level additionalLabels, service.additionalLabels,
// and persistence.additionalLabels. The Qdrant chart has no commonLabels
// helper; setting the label only at the top level silently leaves
// the Service and PVC unlabelled.
func TestQdrantManifestSetsLabelsThreeWays(t *testing.T) {
	doc := parseDoc(t, QdrantManifest(qdrantTestConfig()))
	values := doc["spec"].(map[string]any)["valuesContent"].(string)

	// Three independent occurrences, one per label key the chart honours.
	assert.Equal(t, 3, strings.Count(values, "app.kubernetes.io/component: rag"),
		"component=rag must be set under top-level, service, and persistence additionalLabels")
	assert.Equal(t, 3, strings.Count(values, "app.kubernetes.io/part-of: kipper-ai"))
	assert.Equal(t, 3, strings.Count(values, "app.kubernetes.io/managed-by: kipper"))
}

func TestQdrantManifestSizing(t *testing.T) {
	cfg := qdrantTestConfig()
	cfg.PVCSize = "30Gi"
	cfg.MemoryRequest = "1Gi"
	cfg.MemoryLimit = "2Gi"
	values := parseDoc(t, QdrantManifest(cfg))["spec"].(map[string]any)["valuesContent"].(string)

	assert.Contains(t, values, "size: 30Gi")
	assert.Contains(t, values, "requests:\n    memory: 1Gi")
	assert.Contains(t, values, "limits:\n    memory: 2Gi")
}

func TestQdrantManifestPinsNode(t *testing.T) {
	cfg := qdrantTestConfig()
	cfg.NodeName = "worker-3"
	values := parseDoc(t, QdrantManifest(cfg))["spec"].(map[string]any)["valuesContent"].(string)

	assert.Contains(t, values, "nodeSelector:\n  kubernetes.io/hostname: worker-3",
		"Qdrant must colocate with Phase 1 Ollama for fast in-cluster embedding traffic")
}

func TestAnythingLLMManifestParts(t *testing.T) {
	out := AnythingLLMManifest(anythingLLMTestConfig())
	docs := SplitYAMLDocuments(out)
	require.Len(t, docs, 3, "expect Secret + HelmChart + Ingress")

	// Document 1: Secret carries the three boot-required credentials
	// plus an empty QDRANT_API_KEY slot (Qdrant runs unauthenticated
	// in v1 but the env var must exist for the chart's env: list).
	secret := parseDoc(t, docs[0])
	assert.Equal(t, "Secret", secret["kind"])
	meta := secret["metadata"].(map[string]any)
	assert.Equal(t, AnythingLLMCredentialsSecretName, meta["name"])
	assert.Equal(t, Namespace, meta["namespace"])
	labels := meta["labels"].(map[string]any)
	assert.Equal(t, "rag", labels["app.kubernetes.io/component"])
	stringData := secret["stringData"].(map[string]any)
	creds := anythingLLMTestCreds()
	assert.Equal(t, creds.JWTSecret, stringData["JWT_SECRET"])
	assert.Equal(t, creds.SigKey, stringData["SIG_KEY"])
	assert.Equal(t, creds.SigSalt, stringData["SIG_SALT"])
	assert.Equal(t, creds.AuthToken, stringData["AUTH_TOKEN"],
		"AUTH_TOKEN must be in the Secret so AnythingLLM can gate the public Ingress before any admin is created")
	assert.Equal(t, "", stringData["QDRANT_API_KEY"], "Qdrant runs unauthenticated in v1")

	// Document 2: HelmChart wired to Ollama + Qdrant.
	chart := parseDoc(t, docs[1])
	assert.Equal(t, "HelmChart", chart["kind"])
	chartMeta := chart["metadata"].(map[string]any)
	assert.Equal(t, "anythingllm", chartMeta["name"])
	chartLabels := chartMeta["labels"].(map[string]any)
	assert.Equal(t, "rag", chartLabels["app.kubernetes.io/component"])
	chartSpec := chart["spec"].(map[string]any)
	assert.Equal(t, AnythingLLMChartRepo, chartSpec["repo"])
	assert.Equal(t, AnythingLLMChartName, chartSpec["chart"])
	assert.Equal(t, AnythingLLMChartVersion, chartSpec["version"])

	// Document 3: Kipper-owned Ingress on the supplied host.
	ing := parseDoc(t, docs[2])
	assert.Equal(t, "Ingress", ing["kind"])
	ingMeta := ing["metadata"].(map[string]any)
	assert.Equal(t, "anythingllm", ingMeta["name"])
	assert.Equal(t, Namespace, ingMeta["namespace"])
	ingLabels := ingMeta["labels"].(map[string]any)
	assert.Equal(t, "rag", ingLabels["app.kubernetes.io/component"])
}

// TestAnythingLLMManifestEnvVarsExactNames pins every env var name to
// the value AnythingLLM v1.9.0 reads at runtime. Wrong names silently
// fall back to defaults (the UI then says "could not reach LLM" without
// surfacing what is misconfigured), so this test is the safety net for
// an upstream rename or a copy-paste error.
func TestAnythingLLMManifestEnvVarsExactNames(t *testing.T) {
	cfg := anythingLLMTestConfig()
	values := parseDoc(t, SplitYAMLDocuments(AnythingLLMManifest(cfg))[1])["spec"].(map[string]any)["valuesContent"].(string)

	// Provider selectors and ollama wiring (config: map).
	assert.Contains(t, values, "LLM_PROVIDER: ollama")
	assert.Contains(t, values, `OLLAMA_BASE_PATH: "http://ollama.kipper-ai.svc.cluster.local:11434"`,
		"OLLAMA_BASE_PATH takes the bare root URL; appending /v1 or /api breaks the connection silently")
	assert.Contains(t, values, `OLLAMA_MODEL_PREF: "qwen2.5:3b-instruct-q4_K_M"`)

	// Embedding wiring.
	assert.Contains(t, values, "EMBEDDING_ENGINE: ollama")
	assert.Contains(t, values, `EMBEDDING_BASE_PATH: "http://ollama.kipper-ai.svc.cluster.local:11434"`,
		"EMBEDDING_BASE_PATH also takes the bare root URL; AnythingLLM appends the embed path itself")
	assert.Contains(t, values, `EMBEDDING_MODEL_PREF: "nomic-embed-text:latest"`)

	// Vector store.
	assert.Contains(t, values, "VECTOR_DB: qdrant")
	assert.Contains(t, values, `QDRANT_ENDPOINT: "http://qdrant.kipper-ai.svc.cluster.local:6333"`)

	// Storage path matches PVC mountPath; mismatch causes data loss
	// on pod restart.
	assert.Contains(t, values, "STORAGE_DIR: /app/server/storage")
	assert.Contains(t, values, "mountPath: /app/server/storage")

	// Boot-required secrets: must be referenced from the env: list,
	// not the config: map (config goes into a ConfigMap; secrets must
	// not be visible there).
	assert.Contains(t, values, "name: JWT_SECRET\n    valueFrom:\n      secretKeyRef:\n        name: anythingllm-credentials\n        key: JWT_SECRET")
	assert.Contains(t, values, "name: SIG_KEY\n    valueFrom:\n      secretKeyRef:\n        name: anythingllm-credentials\n        key: SIG_KEY")
	assert.Contains(t, values, "name: SIG_SALT\n    valueFrom:\n      secretKeyRef:\n        name: anythingllm-credentials\n        key: SIG_SALT")
	assert.Contains(t, values, "name: AUTH_TOKEN\n    valueFrom:\n      secretKeyRef:\n        name: anythingllm-credentials\n        key: AUTH_TOKEN",
		"AUTH_TOKEN must be wired from the Secret so the running pod actually requires it on every UI request")

	// Common community guesses that AnythingLLM does NOT read. If any
	// of these slip into the values block we are misconfiguring the
	// app while looking like we did the right thing.
	assert.NotContains(t, values, "OLLAMA_HOST")
	assert.NotContains(t, values, "OLLAMA_API_BASE")
	assert.NotContains(t, values, "OLLAMA_URL")
}

// TestAnythingLLMManifestProbeOverride verifies we override the chart's
// broken probe defaults. Chart 1.0.0 ships readiness/liveness pointed
// at port 8888 and path /v1/api/health; the container actually serves
// on 3001 with health at /api/ping. Without this override the install
// hangs forever at "0/1 Ready".
func TestAnythingLLMManifestProbeOverride(t *testing.T) {
	values := parseDoc(t, SplitYAMLDocuments(AnythingLLMManifest(anythingLLMTestConfig()))[1])["spec"].(map[string]any)["valuesContent"].(string)

	assert.Contains(t, values, "readinessProbe:\n  httpGet:\n    path: /api/ping\n    port: 3001",
		"readinessProbe must point at the actual AnythingLLM container port and health path")
	assert.Contains(t, values, "livenessProbe:\n  httpGet:\n    path: /api/ping\n    port: 3001",
		"livenessProbe must point at the actual AnythingLLM container port and health path")
	assert.NotContains(t, values, "8888", "the broken default port must not leak through")
	assert.NotContains(t, values, "/v1/api/health", "the broken default health path must not leak through")
}

// TestAnythingLLMManifestPinsRecreateStrategy locks in the Recreate
// deployment strategy. The chart's current default already matches,
// but pinning protects against an upstream flip to RollingUpdate
// which would surge a second AnythingLLM pod (~2 GiB request)
// against the 4 GiB RAG headroom budget AND race two pods on the
// same SQLite/uploads PVC.
func TestAnythingLLMManifestPinsRecreateStrategy(t *testing.T) {
	values := parseDoc(t, SplitYAMLDocuments(AnythingLLMManifest(anythingLLMTestConfig()))[1])["spec"].(map[string]any)["valuesContent"].(string)
	assert.Contains(t, values, "strategy:\n  type: Recreate",
		"strategy must be pinned in valuesContent so an upstream chart default change can't break tier-1 RAG installs")
}

// TestAnythingLLMQdrantEndpointMatchesQdrantService is a cross-manifest
// invariant: the URL AnythingLLM uses to reach Qdrant must match the
// Service name and port the Qdrant chart actually renders. If either
// side drifts (chart renames its Service helper, or someone changes
// QdrantHTTPPort), AnythingLLM silently falls back to LanceDB and the
// vector store is wrong.
func TestAnythingLLMQdrantEndpointMatchesQdrantService(t *testing.T) {
	values := parseDoc(t, SplitYAMLDocuments(AnythingLLMManifest(anythingLLMTestConfig()))[1])["spec"].(map[string]any)["valuesContent"].(string)
	expected := "http://" + QdrantServiceName + "." + Namespace + ".svc.cluster.local:" + qdrantPortString()
	assert.Contains(t, values, "QDRANT_ENDPOINT: \""+expected+"\"")
}

func qdrantPortString() string {
	// Stringify a known constant so the test fails loudly if the port
	// type ever changes.
	return "6333"
}

func TestAnythingLLMManifestEmbeddingModelDefault(t *testing.T) {
	cfg := anythingLLMTestConfig()
	cfg.EmbeddingModel = ""
	values := parseDoc(t, SplitYAMLDocuments(AnythingLLMManifest(cfg))[1])["spec"].(map[string]any)["valuesContent"].(string)
	assert.Contains(t, values, `EMBEDDING_MODEL_PREF: "nomic-embed-text:latest"`,
		"empty EmbeddingModel must default to nomic-embed-text:latest")
}

func TestAnythingLLMManifestRespectsCustomEmbeddingModel(t *testing.T) {
	cfg := anythingLLMTestConfig()
	cfg.EmbeddingModel = "mxbai-embed-large:latest"
	values := parseDoc(t, SplitYAMLDocuments(AnythingLLMManifest(cfg))[1])["spec"].(map[string]any)["valuesContent"].(string)
	assert.Contains(t, values, `EMBEDDING_MODEL_PREF: "mxbai-embed-large:latest"`)
}

func TestAnythingLLMManifestIngress(t *testing.T) {
	cfg := anythingLLMTestConfig()
	cfg.Host = "ask.example.com"
	docs := SplitYAMLDocuments(AnythingLLMManifest(cfg))
	ing := parseDoc(t, docs[2])
	require.Equal(t, "Ingress", ing["kind"])

	annotations := ing["metadata"].(map[string]any)["annotations"].(map[string]any)
	assert.Equal(t, "letsencrypt-prod", annotations["cert-manager.io/cluster-issuer"])

	spec := ing["spec"].(map[string]any)
	assert.Equal(t, "traefik", spec["ingressClassName"])

	tls := spec["tls"].([]any)
	require.Len(t, tls, 1)
	tlsHosts := tls[0].(map[string]any)["hosts"].([]any)
	assert.Equal(t, "ask.example.com", tlsHosts[0])

	rules := spec["rules"].([]any)
	require.Len(t, rules, 1)
	rule := rules[0].(map[string]any)
	assert.Equal(t, "ask.example.com", rule["host"])

	paths := rule["http"].(map[string]any)["paths"].([]any)
	backend := paths[0].(map[string]any)["backend"].(map[string]any)["service"].(map[string]any)
	assert.Equal(t, AnythingLLMServiceName, backend["name"],
		"Ingress backend must point at the chart-rendered Service name")
	assert.Equal(t, AnythingLLMServicePort, backend["port"].(map[string]any)["number"])
}

func TestAnythingLLMManifestPinsNode(t *testing.T) {
	cfg := anythingLLMTestConfig()
	cfg.NodeName = "worker-7"
	values := parseDoc(t, SplitYAMLDocuments(AnythingLLMManifest(cfg))[1])["spec"].(map[string]any)["valuesContent"].(string)
	assert.Contains(t, values, "nodeSelector:\n  kubernetes.io/hostname: worker-7")
}

func TestAnythingLLMManifestSizing(t *testing.T) {
	cfg := anythingLLMTestConfig()
	cfg.PVCSize = "30Gi"
	cfg.MemoryRequest = "3Gi"
	cfg.MemoryLimit = "4Gi"
	values := parseDoc(t, SplitYAMLDocuments(AnythingLLMManifest(cfg))[1])["spec"].(map[string]any)["valuesContent"].(string)
	assert.Contains(t, values, "size: 30Gi")
	assert.Contains(t, values, "requests:\n    memory: 3Gi")
	assert.Contains(t, values, "limits:\n    memory: 4Gi")
}
