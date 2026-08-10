package ai

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// Namespace holds every resource the AI bundle creates. Single namespace
// keeps RBAC and uninstall simple.
const Namespace = "kipper-ai"

// Pinned chart versions. Bump intentionally and update the snapshot tests
// when upgrading any of these. Verify against the project's own release
// page or OCI registry before bumping; do not guess.
//
// k3s helm-controller wants the full OCI URL in spec.chart with no
// spec.repo for OCI charts (see k3s-io/helm-controller types.go: Chart
// is "Helm Chart name in repository, or complete HTTPS URL to chart
// archive (.tgz)" and Helm 3.8.1+ extends that to OCI). For HTTPS repos
// the split repo+chart form is correct, so Ollama keeps the split.
const (
	OllamaChartRepo    = "https://helm.otwld.com/"
	OllamaChartName    = "ollama"
	OllamaChartVersion = "1.55.0"

	LibreChatChartOCI     = "oci://ghcr.io/danny-avila/librechat-chart/librechat"
	LibreChatChartVersion = "1.9.2"
)

// OllamaConfig parameterises the Ollama HelmChart manifest.
type OllamaConfig struct {
	// Model is the Ollama tag to preload at startup, e.g.
	// "qwen2.5:3b-instruct-q4_K_M". Required.
	Model string
	// PVCSize is the size of the model cache volume, e.g. "10Gi".
	PVCSize string
	// EnableGPU sets gpu.enabled in chart values. True for tier 3+.
	EnableGPU bool
	// MemoryRequest is the tier-appropriate memory request for the
	// Ollama container (e.g. "6Gi"). Required so the scheduler refuses
	// to place the pod on a node that cannot host the model.
	MemoryRequest string
	// NodeName pins the Ollama Deployment to the node that passed tier
	// preflight. Without this, on a multi-node cluster the scheduler
	// could pick a smaller node that just happens to be schedulable.
	NodeName string
}

// LibreChatConfig parameterises the LibreChat HelmChart manifest.
type LibreChatConfig struct {
	// Host is the external hostname for the chat UI, e.g.
	// "chat.example.com". Required for the Ingress.
	Host string
	// Model is the default Ollama model exposed in the LibreChat picker.
	Model string
	// Credentials carry the four required LibreChat secrets. Caller
	// generates them; we store them in a Kubernetes Secret and point the
	// chart at it via existingSecretName.
	Credentials LibreChatCredentials
}

// LibreChatCredentials are the five random values LibreChat needs to run.
// CredsKey/CredsIV encrypt user credentials, the JWT pair signs session
// tokens, and MeiliMasterKey is the master key Meilisearch uses (the
// chart's bundled subchart consumes it from the same Secret). The chart's
// own values.yaml comment block lists exactly these five keys as
// required, see helm/librechat/values.yaml on danny-avila/LibreChat.
type LibreChatCredentials struct {
	CredsKey         string
	CredsIV          string
	JWTSecret        string
	JWTRefreshSecret string
	MeiliMasterKey   string
}

// NamespaceManifest returns the YAML for the single namespace that holds
// every AI bundle resource.
func NamespaceManifest() string {
	return `apiVersion: v1
kind: Namespace
metadata:
  name: ` + Namespace + `
  labels:
    app.kubernetes.io/managed-by: kipper
    app.kubernetes.io/part-of: kipper-ai
`
}

// OllamaManifest returns the YAML for the Ollama HelmChart resource. The
// chart is reconciled by k3s helm-controller into the kipper-ai namespace.
//
// Memory request is tier-sized so the scheduler refuses to place the pod
// on a node that cannot host the model. nodeSelector pins the pod to
// the node that passed tier preflight; without it, a multi-node cluster
// where one node passes and others do not could schedule Ollama on a
// failing node and the install would OOM at runtime.
//
// updateStrategy.type is pinned to Recreate. The chart already defaults
// to Recreate at v1.55.0, but pinning it in our valuesContent protects
// against an upstream default flip: a RollingUpdate would surge a
// second Ollama pod that has to load the model into RAM (6–10 GiB)
// alongside the running one, OOMing tier 1 (8 GiB free) and breaking
// GPU tiers where the device is exclusive. Brief downtime during chart
// upgrades is the right tradeoff for a single-replica workload.
func OllamaManifest(cfg OllamaConfig) string {
	return fmt.Sprintf(`apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: ollama
  namespace: kube-system
  labels:
    app.kubernetes.io/managed-by: kipper
    app.kubernetes.io/part-of: kipper-ai
spec:
  repo: %[1]s
  chart: %[2]s
  version: %[3]s
  targetNamespace: %[4]s
  valuesContent: |-
    ollama:
      gpu:
        enabled: %[5]t
        type: nvidia
        number: 1
      models:
        pull:
          - %[6]s
        run:
          - %[6]s
    persistentVolume:
      enabled: true
      size: %[7]s
    service:
      type: ClusterIP
      port: 11434
    resources:
      requests:
        memory: %[8]s
    nodeSelector:
      kubernetes.io/hostname: %[9]s
    updateStrategy:
      type: Recreate
    # Keep models hot in RAM. The chart's extraEnv lands on the
    # Ollama container; OLLAMA_KEEP_ALIVE=24h means a model loaded
    # on the first request stays resident until 24h of idleness,
    # instead of unloading after the 5-minute default. Saves the
    # multi-second model-load tax on every cold question against
    # the RAG bundle and LibreChat.
    extraEnv:
      - name: OLLAMA_KEEP_ALIVE
        value: "24h"
`,
		OllamaChartRepo,    // 1
		OllamaChartName,    // 2
		OllamaChartVersion, // 3
		Namespace,          // 4
		cfg.EnableGPU,      // 5
		cfg.Model,          // 6
		cfg.PVCSize,        // 7
		cfg.MemoryRequest,  // 8
		cfg.NodeName,       // 9
	)
}

// LibreChatCredentialsSecretName is the name of the Secret carrying the
// four LibreChat credential values referenced by the chart through
// existingSecretName.
const LibreChatCredentialsSecretName = "librechat-credentials" //nolint:gosec // kubernetes Secret object name, not a credential value

// LibreChatDeploymentName is the rendered Deployment / Service name. The
// chart's `librechat.fullname` template emits "<release>-<chart>" without
// the contains-shortcut, so a release named "librechat" produces
// "librechat-librechat" (verified against the chart's _helpers.tpl).
const LibreChatDeploymentName = "librechat-librechat"

// LibreChatConfigMapName holds the LibreChat librechat.yaml config we
// generate ourselves and reference via `existingConfigYaml`. The chart's
// own configmap.yaml template runs the value through `toYaml | indent`,
// which double-encodes a YAML string as a literal block scalar; our
// own ConfigMap bypasses that bug.
const LibreChatConfigMapName = "librechat-config"

// BundleStateConfigMapName is the name of the ConfigMap that records the
// installed model and chat host. `kip ai status` reads from this rather
// than the local AI client config so a user with a hosted provider (or
// who declined auto-config) still sees the right bundle state.
const BundleStateConfigMapName = "kipper-ai-bundle-state"

// libreChatConfigHash computes a content hash that captures every
// LibreChat input we control: the credential set, the model, the
// rendered librechat.yaml in the ConfigMap, and the public host. Any
// change here flows into chart values as a podAnnotations entry,
// which bumps the HelmChart's generation and forces helm-controller
// to roll the LibreChat pods.
//
// Without this, a re-apply that only changes a non-chart input (e.g.
// the model name, which lives in our ConfigMap referenced via
// existingConfigYaml) would leave the HelmChart spec unchanged. The
// install flow's `expectNewJob=false` shortcut would then accept the
// previous Succeeded helm-install Job and return success while the
// running LibreChat pods still serve the old startup config.
func libreChatConfigHash(cfg LibreChatConfig) string {
	h := sha256.New()
	_, _ = io.WriteString(h, cfg.Host+"|"+cfg.Model+"|")
	_, _ = io.WriteString(h, cfg.Credentials.CredsKey+"|")
	_, _ = io.WriteString(h, cfg.Credentials.CredsIV+"|")
	_, _ = io.WriteString(h, cfg.Credentials.JWTSecret+"|")
	_, _ = io.WriteString(h, cfg.Credentials.JWTRefreshSecret+"|")
	_, _ = io.WriteString(h, cfg.Credentials.MeiliMasterKey)
	return hex.EncodeToString(h.Sum(nil))
}

// LibreChatManifest returns the YAML for LibreChat: a Secret carrying the
// chart's required credentials, the HelmChart wired to the in-cluster
// Ollama service, and a Kipper-owned Ingress on the user-supplied host
// with cert-manager TLS. The chart's bundled Ingress is disabled since
// we own the routing layer here.
//
// The chart's deployment template envFroms `global.librechat.existingSecretName`
// (verified against danny-avila/LibreChat helm/librechat/templates/deployment.yaml)
// and the bundled Meilisearch subchart reads MEILI_MASTER_KEY from the
// same Secret via `meilisearch.auth.existingMasterKeySecret`. Setting
// the secret name only under `librechat:` (without `global:`) silently
// no-ops, so the chart would fall back to defaults.
//
// The mongodb subchart's image is overridden to `bitnamilegacy/mongodb`
// because Bitnami removed the original `bitnami/mongodb` tags from
// Docker Hub in late 2025 and republished the same images under the
// legacy namespace. Without this override the install hangs in
// Init:ImagePullBackOff and LibreChat itself crash-loops on a Mongo it
// can never reach.
func LibreChatManifest(cfg LibreChatConfig) string {
	ollamaURL := fmt.Sprintf("http://ollama.%s.svc.cluster.local:11434/v1", Namespace)
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %[2]s
  namespace: %[1]s
  labels:
    app.kubernetes.io/managed-by: kipper
    app.kubernetes.io/part-of: kipper-ai
type: Opaque
stringData:
  CREDS_KEY: %[3]q
  CREDS_IV: %[4]q
  JWT_SECRET: %[5]q
  JWT_REFRESH_SECRET: %[6]q
  MEILI_MASTER_KEY: %[13]q
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: %[14]s
  namespace: %[1]s
  labels:
    app.kubernetes.io/managed-by: kipper
    app.kubernetes.io/part-of: kipper-ai
data:
  librechat.yaml: |
    version: 1.2.1
    cache: true
    endpoints:
      custom:
        - name: "Local AI"
          apiKey: "ollama"
          baseURL: "%[11]s"
          models:
            default: ["%[12]s"]
            fetch: true
          titleConvo: true
          titleModel: "%[12]s"
          modelDisplayLabel: "Kipper AI"
---
apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: librechat
  namespace: kube-system
  labels:
    app.kubernetes.io/managed-by: kipper
    app.kubernetes.io/part-of: kipper-ai
spec:
  chart: %[7]s
  version: %[9]s
  targetNamespace: %[1]s
  valuesContent: |-
    global:
      librechat:
        existingSecretName: %[2]s
    podAnnotations:
      kipper.run/config-hash: %[15]q
    librechat:
      configEnv:
        DOMAIN_CLIENT: "https://%[10]s"
        DOMAIN_SERVER: "https://%[10]s"
        ALLOW_REGISTRATION: "false"
        ALLOW_SOCIAL_LOGIN: "false"
        ALLOW_SOCIAL_REGISTRATION: "false"
      existingConfigYaml: %[14]s
    ingress:
      enabled: false
    librechat-rag-api:
      enabled: false
    mongodb:
      image:
        registry: docker.io
        repository: bitnamilegacy/mongodb
    meilisearch:
      auth:
        existingMasterKeySecret: %[2]s
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: librechat
  namespace: %[1]s
  labels:
    app.kubernetes.io/managed-by: kipper
    app.kubernetes.io/part-of: kipper-ai
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
spec:
  ingressClassName: traefik
  tls:
    - hosts:
        - %[10]q
      secretName: librechat-tls
  rules:
    - host: %[10]q
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: librechat-librechat
                port:
                  number: 3080
`,
		Namespace,                        // 1
		LibreChatCredentialsSecretName,   // 2
		cfg.Credentials.CredsKey,         // 3
		cfg.Credentials.CredsIV,          // 4
		cfg.Credentials.JWTSecret,        // 5
		cfg.Credentials.JWTRefreshSecret, // 6
		LibreChatChartOCI,                // 7
		"",                               // 8 (unused, kept for stable indices)
		LibreChatChartVersion,            // 9
		cfg.Host,                         // 10
		ollamaURL,                        // 11
		cfg.Model,                        // 12
		cfg.Credentials.MeiliMasterKey,   // 13
		LibreChatConfigMapName,           // 14
		libreChatConfigHash(cfg),         // 15
	)
}

// SplitYAMLDocuments breaks a multi-document YAML string into individual
// documents on the standard "---" separator and returns only non-empty
// documents.
func SplitYAMLDocuments(manifest string) []string {
	parts := strings.Split(manifest, "\n---\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}
