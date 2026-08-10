package ai

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func parseDoc(t *testing.T, doc string) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(doc), &out), "doc must be valid yaml")
	return out
}

func TestNamespaceManifestParses(t *testing.T) {
	doc := parseDoc(t, NamespaceManifest())
	assert.Equal(t, "v1", doc["apiVersion"])
	assert.Equal(t, "Namespace", doc["kind"])
	meta := doc["metadata"].(map[string]any)
	assert.Equal(t, Namespace, meta["name"])
	labels := meta["labels"].(map[string]any)
	assert.Equal(t, "kipper", labels["app.kubernetes.io/managed-by"])
}

func TestOllamaManifestStructure(t *testing.T) {
	out := OllamaManifest(OllamaConfig{
		Model:     "qwen2.5:3b-instruct-q4_K_M",
		PVCSize:   "10Gi",
		EnableGPU: false,
	})
	doc := parseDoc(t, out)
	assert.Equal(t, "helm.cattle.io/v1", doc["apiVersion"])
	assert.Equal(t, "HelmChart", doc["kind"])

	spec := doc["spec"].(map[string]any)
	assert.Equal(t, OllamaChartRepo, spec["repo"])
	assert.Equal(t, OllamaChartName, spec["chart"])
	assert.Equal(t, OllamaChartVersion, spec["version"])
	assert.Equal(t, Namespace, spec["targetNamespace"])

	values := spec["valuesContent"].(string)
	assert.Contains(t, values, "qwen2.5:3b-instruct-q4_K_M")
	assert.Contains(t, values, "enabled: false")
	assert.Contains(t, values, "size: 10Gi")
}

func TestOllamaManifestEnablesGPU(t *testing.T) {
	out := OllamaManifest(OllamaConfig{
		Model:     "qwen2.5:14b-instruct-q4_K_M",
		PVCSize:   "60Gi",
		EnableGPU: true,
	})
	doc := parseDoc(t, out)
	values := doc["spec"].(map[string]any)["valuesContent"].(string)
	assert.Contains(t, values, "enabled: true")
	assert.Contains(t, values, "type: nvidia")
	assert.Contains(t, values, "qwen2.5:14b-instruct-q4_K_M")
}

// TestOllamaManifestPinsRecreateStrategy locks in the Recreate update
// strategy on the rendered chart values. The otwld chart already
// defaults to Recreate at v1.55.0, but pinning it ourselves protects
// against an upstream default flip — RollingUpdate would surge a
// second Ollama pod that has to load the model into RAM alongside the
// running one, OOMing tier 1 and breaking GPU tiers.
func TestOllamaManifestPinsRecreateStrategy(t *testing.T) {
	out := OllamaManifest(OllamaConfig{
		Model:         "qwen2.5:3b-instruct-q4_K_M",
		PVCSize:       "10Gi",
		MemoryRequest: "6Gi",
		NodeName:      "node-a",
	})
	doc := parseDoc(t, out)
	values := doc["spec"].(map[string]any)["valuesContent"].(string)
	assert.Contains(t, values, "updateStrategy:\n  type: Recreate",
		"updateStrategy must be pinned in valuesContent so an upstream chart default change can't break tier 1 / GPU upgrades")
}

func TestOllamaManifestPinsNodeAndMemoryRequest(t *testing.T) {
	out := OllamaManifest(OllamaConfig{
		Model:         "qwen2.5:7b-instruct-q4_K_M",
		PVCSize:       "30Gi",
		MemoryRequest: "10Gi",
		NodeName:      "worker-2",
	})
	doc := parseDoc(t, out)
	values := doc["spec"].(map[string]any)["valuesContent"].(string)
	// Memory request must be tier-sized so the scheduler refuses a
	// node that cannot host the model.
	assert.Contains(t, values, "resources:\n  requests:\n    memory: 10Gi")
	// nodeSelector pins the pod to the node that passed preflight.
	assert.Contains(t, values, "nodeSelector:\n  kubernetes.io/hostname: worker-2")
}

func libreChatTestCreds() LibreChatCredentials {
	return LibreChatCredentials{
		CredsKey:         "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		CredsIV:          "0123456789abcdef0123456789abcdef",
		JWTSecret:        "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
		JWTRefreshSecret: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		MeiliMasterKey:   "1111111111222222222233333333334444444444555555555566666666667777",
	}
}

func TestLibreChatManifestParts(t *testing.T) {
	creds := libreChatTestCreds()
	out := LibreChatManifest(LibreChatConfig{
		Host:        "chat.kipper-demo.example.com",
		Model:       "qwen2.5:3b-instruct-q4_K_M",
		Credentials: creds,
	})
	docs := SplitYAMLDocuments(out)
	require.Len(t, docs, 4, "expect Secret + ConfigMap + HelmChart + Ingress")

	// Secret carries all five LibreChat-required credential values,
	// keyed by the env var names the chart expects.
	secret := parseDoc(t, docs[0])
	assert.Equal(t, "Secret", secret["kind"])
	assert.Equal(t, LibreChatCredentialsSecretName, secret["metadata"].(map[string]any)["name"])
	stringData := secret["stringData"].(map[string]any)
	assert.Equal(t, creds.CredsKey, stringData["CREDS_KEY"])
	assert.Equal(t, creds.CredsIV, stringData["CREDS_IV"])
	assert.Equal(t, creds.JWTSecret, stringData["JWT_SECRET"])
	assert.Equal(t, creds.JWTRefreshSecret, stringData["JWT_REFRESH_SECRET"])
	assert.Equal(t, creds.MeiliMasterKey, stringData["MEILI_MASTER_KEY"])

	// ConfigMap holds librechat.yaml as raw content so the chart's
	// configmap.yaml template (which double-encodes via toYaml | indent)
	// is bypassed entirely.
	cm := parseDoc(t, docs[1])
	assert.Equal(t, "ConfigMap", cm["kind"])
	assert.Equal(t, LibreChatConfigMapName, cm["metadata"].(map[string]any)["name"])
	cmData := cm["data"].(map[string]any)["librechat.yaml"].(string)
	assert.Contains(t, cmData, "version: 1.2.1")
	assert.Contains(t, cmData, "ollama.kipper-ai.svc.cluster.local:11434/v1")
	assert.Contains(t, cmData, "qwen2.5:3b-instruct-q4_K_M")
	// Sanity: the rendered file must NOT start with a literal `|` (which
	// is what the chart's own configYamlContent path produces).
	assert.False(t, strings.HasPrefix(strings.TrimSpace(cmData), "|"),
		"librechat.yaml must be raw content, not a re-encoded YAML block scalar")

	// HelmChart points at the right OCI artifact, sets the secret via the
	// global path the chart's deployment template actually reads, points
	// at our own ConfigMap via existingConfigYaml, and wires the bundled
	// Meilisearch subchart at the same Secret.
	chart := parseDoc(t, docs[2])
	assert.Equal(t, "HelmChart", chart["kind"])
	chartSpec := chart["spec"].(map[string]any)
	assert.Nil(t, chartSpec["repo"], "spec.repo must not be set for OCI charts")
	assert.Equal(t, LibreChatChartOCI, chartSpec["chart"])
	assert.Equal(t, LibreChatChartVersion, chartSpec["version"])
	values := chartSpec["valuesContent"].(string)
	assert.Contains(t, values, "global:\n  librechat:\n    existingSecretName: "+LibreChatCredentialsSecretName)
	assert.Contains(t, values, "existingConfigYaml: "+LibreChatConfigMapName)
	assert.NotContains(t, values, "configYamlContent",
		"must not pass configYamlContent — chart's toYaml double-encodes a YAML string")
	assert.Contains(t, values, "meilisearch:\n  auth:\n    existingMasterKeySecret: "+LibreChatCredentialsSecretName)
	assert.Contains(t, values, "mongodb:\n  image:\n    registry: docker.io\n    repository: bitnamilegacy/mongodb")
	assert.Contains(t, values, "configEnv:")
	assert.Contains(t, values, "https://chat.kipper-demo.example.com")
	assert.Contains(t, values, "\ningress:\n  enabled: false")
	assert.Contains(t, values, "\nlibrechat-rag-api:\n  enabled: false")

	// Ingress on the user-supplied host with cert-manager annotation.
	ing := parseDoc(t, docs[3])
	assert.Equal(t, "Ingress", ing["kind"])
	ingMeta := ing["metadata"].(map[string]any)
	annotations := ingMeta["annotations"].(map[string]any)
	assert.Equal(t, "letsencrypt-prod", annotations["cert-manager.io/cluster-issuer"])
	rules := ing["spec"].(map[string]any)["rules"].([]any)
	require.Len(t, rules, 1)
	rule := rules[0].(map[string]any)
	assert.Equal(t, "chat.kipper-demo.example.com", rule["host"])
	// The chart's librechat.fullname template renders as "librechat-librechat"
	// when the release is named "librechat" (it does not collapse the
	// matching segment). Our Ingress backend must match that or the
	// route lands on a non-existent Service.
	paths := rule["http"].(map[string]any)["paths"].([]any)
	require.Len(t, paths, 1)
	backendSvc := paths[0].(map[string]any)["backend"].(map[string]any)["service"].(map[string]any)
	assert.Equal(t, LibreChatDeploymentName, backendSvc["name"])
}

func TestLibreChatDisablesOpenRegistration(t *testing.T) {
	out := LibreChatManifest(LibreChatConfig{
		Host:        "chat.example.com",
		Model:       "qwen2.5:3b-instruct-q4_K_M",
		Credentials: libreChatTestCreds(),
	})
	docs := SplitYAMLDocuments(out)
	require.GreaterOrEqual(t, len(docs), 3)
	// Document order: Secret, ConfigMap, HelmChart, Ingress.
	chart := parseDoc(t, docs[2])
	values := chart["spec"].(map[string]any)["valuesContent"].(string)
	// Open registration must be off so a stranger cannot claim the chat
	// UI before the operator bootstraps an admin. Values must live under
	// configEnv, which is the path the chart actually reads.
	assert.Contains(t, values, "configEnv:")
	assert.Contains(t, values, `ALLOW_REGISTRATION: "false"`)
	assert.Contains(t, values, `ALLOW_SOCIAL_LOGIN: "false"`)
	assert.Contains(t, values, `ALLOW_SOCIAL_REGISTRATION: "false"`)
}

func TestSplitYAMLDocumentsDropsEmpty(t *testing.T) {
	in := strings.Join([]string{
		"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: a",
		"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: b",
		"   ",
	}, "\n---\n")

	docs := SplitYAMLDocuments(in)
	require.Len(t, docs, 2)
	assert.Contains(t, docs[0], "name: a")
	assert.Contains(t, docs[1], "name: b")
}

func TestLibreChatCredentialsQuotedSafely(t *testing.T) {
	// A credential containing quotes and colons must round-trip through
	// the YAML render unchanged. We rely on %q quoting in the manifest.
	hostile := `quoted "with" colons: and \backslashes`
	out := LibreChatManifest(LibreChatConfig{
		Host:  "chat.example.com",
		Model: "qwen2.5:3b-instruct-q4_K_M",
		Credentials: LibreChatCredentials{
			CredsKey:         hostile,
			CredsIV:          hostile,
			JWTSecret:        hostile,
			JWTRefreshSecret: hostile,
		},
	})
	docs := SplitYAMLDocuments(out)
	require.GreaterOrEqual(t, len(docs), 1)
	secret := parseDoc(t, docs[0])
	stringData := secret["stringData"].(map[string]any)
	assert.Equal(t, hostile, stringData["CREDS_KEY"])
	assert.Equal(t, hostile, stringData["JWT_SECRET"])
}
