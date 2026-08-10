package ai

import "fmt"

// Pinned chart versions for the Phase 2 RAG bundle. Same verification
// rule as Phase 1: bump intentionally and update the snapshot tests
// when upgrading. Verify against the project's release page or the
// chart's values.yaml before bumping; do not guess.
//
// Both Qdrant and AnythingLLM ship via plain HTTPS Helm repos (not
// OCI), so spec.repo + spec.chart + spec.version is the right form for
// k3s helm-controller.
const (
	QdrantChartRepo    = "https://qdrant.github.io/qdrant-helm"
	QdrantChartName    = "qdrant"
	QdrantChartVersion = "1.17.1"

	AnythingLLMChartRepo    = "https://mintplex-labs.github.io/helm-charts/"
	AnythingLLMChartName    = "anythingllm"
	AnythingLLMChartVersion = "1.0.0"
)

// Names and ports of resources rendered into the kipper-ai namespace
// by the RAG bundle.
const (
	// QdrantServiceName is the in-cluster Service rendered by the
	// Qdrant chart. The chart's qdrant.fullname helper collapses
	// release name + chart name when one contains the other, so a
	// release named "qdrant" produces a Service named "qdrant"
	// (verified against templates/_helpers.tpl at qdrant-1.17.1).
	QdrantServiceName = "qdrant"
	// QdrantHTTPPort is the port the Qdrant Service exposes for the
	// REST API. AnythingLLM connects here.
	QdrantHTTPPort = 6333

	// AnythingLLMServiceName follows the same release-equals-chart
	// collapse pattern. Service is rendered as "anythingllm".
	AnythingLLMServiceName = "anythingllm"
	// AnythingLLMServicePort is the container and Service port
	// AnythingLLM serves on. The chart's values.yaml at 1.0.0
	// defaults probes to port 8888 (a chart bug); the actual
	// container only exposes service.port=3001, so we override
	// readinessProbe and livenessProbe to 3001.
	AnythingLLMServicePort = 3001
	// AnythingLLMDeploymentName is the rendered Deployment name. Used
	// by the installer's wait-for-rollout step in Step 4.
	AnythingLLMDeploymentName = "anythingllm"

	// AnythingLLMCredentialsSecretName carries the boot-required
	// secrets (JWT_SECRET >= 12 chars, SIG_KEY >= 32 chars,
	// SIG_SALT >= 32 chars) and an optional QDRANT_API_KEY. Each
	// value is referenced from chart values via env: with
	// valueFrom.secretKeyRef.
	AnythingLLMCredentialsSecretName = "anythingllm-credentials"

	// DefaultEmbeddingModel is the Ollama tag pulled for embeddings.
	// nomic-embed-text is Apache 2.0, ~270 MB on disk, ~1 GiB resident
	// in Ollama including the 8K-context KV cache. Verified against
	// ollama.com/library/nomic-embed-text on 2026-05-10.
	DefaultEmbeddingModel = "nomic-embed-text:latest"
)

// componentLabel and componentValueRAG distinguish Phase 2 resources
// from Phase 1 chat resources within the same kipper-ai namespace.
// Used for label-scoped uninstall and for `kubectl get -l` queries.
const (
	componentLabel    = "app.kubernetes.io/component"
	componentValueRAG = "rag"
)

// QdrantConfig parameterises the Qdrant HelmChart manifest.
type QdrantConfig struct {
	// PVCSize sizes the Qdrant data volume that holds vectors and
	// collection metadata. Tier-aware default in the installer.
	PVCSize string
	// MemoryRequest is the Qdrant container's memory request. The
	// chart ships with resources unset, so the scheduler treats the
	// pod as best-effort; we set request and limit so it competes
	// honestly for capacity.
	MemoryRequest string
	// MemoryLimit caps Qdrant memory growth. Qdrant scales gently
	// with vector count under mmap mode (~135 MiB at 1M vectors), so
	// the limit can stay modest.
	MemoryLimit string
	// NodeName pins Qdrant to the same node Phase 1 pinned Ollama to.
	// Embedding traffic stays node-local.
	NodeName string
}

// AnythingLLMCredentials are the four random values AnythingLLM
// requires at boot. Verified against server/.env.example at v1.9.0:
//
//   - JWT_SECRET >= 12 chars: signs session tokens
//   - SIG_KEY    >= 32 chars: file signature key
//   - SIG_SALT   >= 32 chars: file signature salt
//   - AUTH_TOKEN: bootstrap password gating every UI request when
//     AnythingLLM runs behind a public Ingress
//
// AUTH_TOKEN closes the first-visit-admin claim race: AnythingLLM
// creates the first admin account on initial UI visit, so a
// well-known ask.<domain> URL would otherwise let any internet
// scanner claim the instance before the operator does. With
// AUTH_TOKEN set, every request must present the token before any
// page (including the setup wizard) is reachable. The installer
// prints the token after a successful install so the operator can
// log in and create the real admin account, then optionally rotate
// the token by editing the Secret and restarting the pod.
type AnythingLLMCredentials struct {
	JWTSecret string
	SigKey    string
	SigSalt   string
	AuthToken string
}

// AnythingLLMConfig parameterises the AnythingLLM manifest set: a
// Secret carrying the boot credentials, the HelmChart, and a
// Kipper-owned Ingress.
type AnythingLLMConfig struct {
	// Host is the external hostname for the Ask UI, e.g.
	// "ask.example.com". Required for the Ingress.
	Host string
	// NodeName pins AnythingLLM to the same node as Qdrant and Ollama
	// for fast in-cluster traffic.
	NodeName string
	// OllamaModel is the chat model AnythingLLM defaults to. The
	// installer reads this from Phase 1's bundle state ConfigMap so
	// users do not have to retype it.
	OllamaModel string
	// EmbeddingModel is the Ollama tag used for embeddings. Defaults
	// to DefaultEmbeddingModel when empty.
	EmbeddingModel string
	// PVCSize sizes AnythingLLM's persistent storage at
	// /app/server/storage. Tier-aware default in the installer.
	PVCSize string
	// MemoryRequest and MemoryLimit. AnythingLLM is OOM-prone above
	// 2 GiB under agent workloads; size accordingly.
	MemoryRequest string
	MemoryLimit   string
	// Credentials carry the three random values AnythingLLM requires
	// at boot. Caller generates them; the manifest stores them in a
	// Kubernetes Secret and references each value from chart `env:`
	// with valueFrom.secretKeyRef.
	Credentials AnythingLLMCredentials
}

// QdrantManifest returns the YAML for the Qdrant HelmChart resource.
// The chart is reconciled by k3s helm-controller into the kipper-ai
// namespace. Service is exposed cluster-internally only on port 6333.
//
// The Qdrant chart renders a StatefulSet (not a Deployment) and
// hardcodes RollingUpdate as the update strategy, with no value to
// override it. With one replica and a single PVC this is functionally
// fine: the StatefulSet rolls one pod at a time anyway.
//
// The chart has no commonLabels helper. To label everything Kipper
// owns, the same label map has to be set under three separate keys:
// top-level additionalLabels (StatefulSet + pods), service.additionalLabels
// (Service), and persistence.additionalLabels (PVC). Setting only the
// top-level value silently leaves Service and PVC unlabelled.
//
// Verified against charts/qdrant/values.yaml at tag qdrant-1.17.1.
func QdrantManifest(cfg QdrantConfig) string {
	return fmt.Sprintf(`apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: qdrant
  namespace: kube-system
  labels:
    app.kubernetes.io/managed-by: kipper
    app.kubernetes.io/part-of: kipper-ai
    app.kubernetes.io/component: rag
spec:
  repo: %[1]s
  chart: %[2]s
  version: %[3]s
  targetNamespace: %[4]s
  valuesContent: |-
    replicaCount: 1
    service:
      type: ClusterIP
      additionalLabels:
        app.kubernetes.io/component: rag
        app.kubernetes.io/part-of: kipper-ai
        app.kubernetes.io/managed-by: kipper
    persistence:
      size: %[5]s
      additionalLabels:
        app.kubernetes.io/component: rag
        app.kubernetes.io/part-of: kipper-ai
        app.kubernetes.io/managed-by: kipper
    additionalLabels:
      app.kubernetes.io/component: rag
      app.kubernetes.io/part-of: kipper-ai
      app.kubernetes.io/managed-by: kipper
    resources:
      requests:
        memory: %[6]s
      limits:
        memory: %[7]s
    nodeSelector:
      kubernetes.io/hostname: %[8]s
`,
		QdrantChartRepo,    // 1
		QdrantChartName,    // 2
		QdrantChartVersion, // 3
		Namespace,          // 4
		cfg.PVCSize,        // 5
		cfg.MemoryRequest,  // 6
		cfg.MemoryLimit,    // 7
		cfg.NodeName,       // 8
	)
}

// AnythingLLMManifest returns the YAML for AnythingLLM: a Secret
// carrying the boot-required credentials, a HelmChart wired to the
// in-cluster Ollama (LLM + embeddings) and Qdrant (vector store), and
// a Kipper-owned Ingress on the user-supplied host with cert-manager
// TLS. The chart's bundled Ingress is left disabled; we own routing.
//
// AUTH_TOKEN gates every UI request behind a bootstrap password so a
// scanner cannot claim the first-admin slot before the operator does.
// strategy.type: Recreate is pinned even though the chart's current
// default already matches; an upstream flip to RollingUpdate would
// surge a second AnythingLLM pod that competes for the 4 GiB RAG
// headroom budget AND races on the same SQLite/uploads PVC.
//
// The chart at version 1.0.0 has two defaults that need overriding:
//
//   - Probes target port 8888 with path /v1/api/health. The container
//     actually serves on port 3001 and the AnythingLLM health route is
//     /api/ping (server/endpoints/system.js mounts /ping under the
//     /api router prefix at server/index.js: app.use("/api", apiRouter)).
//     We override both path and port so the install does not hang at
//     0/1 Ready.
//
//   - persistentVolume.size defaults to 8Gi. We size it by tier in the
//     installer; AnythingLLM's PVC holds uploaded documents, the SQLite
//     database, and per-workspace state.
//
// Plain (non-secret) env goes into chart `config:` which renders as a
// ConfigMap and is applied via envFrom. Secret-derived env goes into
// chart `env:` as a list with valueFrom.secretKeyRef pointing at our
// Secret.
//
// OLLAMA_BASE_PATH and EMBEDDING_BASE_PATH take the bare host:port URL.
// AnythingLLM appends /api or /v1 internally; including either path
// segment here breaks the connection silently and the UI falls back to
// "could not reach LLM" without surfacing a useful error.
//
// Env var names verified against server/.env.example at AnythingLLM
// v1.9.0 (the appVersion the chart pins). OLLAMA_HOST, OLLAMA_API_BASE,
// and OLLAMA_URL are common community guesses that AnythingLLM does
// NOT read; only OLLAMA_BASE_PATH (LLM) and EMBEDDING_BASE_PATH
// (when EMBEDDING_ENGINE=ollama) are honoured.
func AnythingLLMManifest(cfg AnythingLLMConfig) string {
	embeddingModel := cfg.EmbeddingModel
	if embeddingModel == "" {
		embeddingModel = DefaultEmbeddingModel
	}
	ollamaURL := fmt.Sprintf("http://ollama.%s.svc.cluster.local:11434", Namespace)
	qdrantURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", QdrantServiceName, Namespace, QdrantHTTPPort)
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %[2]s
  namespace: %[1]s
  labels:
    app.kubernetes.io/managed-by: kipper
    app.kubernetes.io/part-of: kipper-ai
    app.kubernetes.io/component: rag
type: Opaque
stringData:
  JWT_SECRET: %[3]q
  SIG_KEY: %[4]q
  SIG_SALT: %[5]q
  AUTH_TOKEN: %[20]q
  QDRANT_API_KEY: ""
---
apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: anythingllm
  namespace: kube-system
  labels:
    app.kubernetes.io/managed-by: kipper
    app.kubernetes.io/part-of: kipper-ai
    app.kubernetes.io/component: rag
spec:
  repo: %[6]s
  chart: %[7]s
  version: %[8]s
  targetNamespace: %[1]s
  valuesContent: |-
    replicaCount: 1
    strategy:
      type: Recreate
    service:
      type: ClusterIP
      port: %[9]d
    nodeSelector:
      kubernetes.io/hostname: %[10]s
    resources:
      requests:
        memory: %[11]s
      limits:
        memory: %[12]s
    podLabels:
      app.kubernetes.io/component: rag
      app.kubernetes.io/part-of: kipper-ai
      app.kubernetes.io/managed-by: kipper
    persistentVolume:
      enabled: true
      size: %[13]s
      mountPath: /app/server/storage
      labels:
        app.kubernetes.io/component: rag
        app.kubernetes.io/part-of: kipper-ai
        app.kubernetes.io/managed-by: kipper
    readinessProbe:
      httpGet:
        path: /api/ping
        port: %[9]d
      initialDelaySeconds: 15
    livenessProbe:
      httpGet:
        path: /api/ping
        port: %[9]d
      initialDelaySeconds: 15
    ingress:
      enabled: false
    config:
      STORAGE_DIR: /app/server/storage
      DISABLE_TELEMETRY: "true"
      LLM_PROVIDER: ollama
      OLLAMA_BASE_PATH: %[14]q
      OLLAMA_MODEL_PREF: %[15]q
      OLLAMA_MODEL_TOKEN_LIMIT: "4096"
      EMBEDDING_ENGINE: ollama
      EMBEDDING_BASE_PATH: %[14]q
      EMBEDDING_MODEL_PREF: %[16]q
      EMBEDDING_MODEL_MAX_CHUNK_LENGTH: "2048"
      VECTOR_DB: qdrant
      QDRANT_ENDPOINT: %[17]q
    env:
      - name: JWT_SECRET
        valueFrom:
          secretKeyRef:
            name: %[2]s
            key: JWT_SECRET
      - name: SIG_KEY
        valueFrom:
          secretKeyRef:
            name: %[2]s
            key: SIG_KEY
      - name: SIG_SALT
        valueFrom:
          secretKeyRef:
            name: %[2]s
            key: SIG_SALT
      - name: QDRANT_API_KEY
        valueFrom:
          secretKeyRef:
            name: %[2]s
            key: QDRANT_API_KEY
      - name: AUTH_TOKEN
        valueFrom:
          secretKeyRef:
            name: %[2]s
            key: AUTH_TOKEN
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: anythingllm
  namespace: %[1]s
  labels:
    app.kubernetes.io/managed-by: kipper
    app.kubernetes.io/part-of: kipper-ai
    app.kubernetes.io/component: rag
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
spec:
  ingressClassName: traefik
  tls:
    - hosts:
        - %[18]q
      secretName: anythingllm-tls
  rules:
    - host: %[18]q
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: %[19]s
                port:
                  number: %[9]d
`,
		Namespace,                        // 1
		AnythingLLMCredentialsSecretName, // 2
		cfg.Credentials.JWTSecret,        // 3
		cfg.Credentials.SigKey,           // 4
		cfg.Credentials.SigSalt,          // 5
		AnythingLLMChartRepo,             // 6
		AnythingLLMChartName,             // 7
		AnythingLLMChartVersion,          // 8
		AnythingLLMServicePort,           // 9
		cfg.NodeName,                     // 10
		cfg.MemoryRequest,                // 11
		cfg.MemoryLimit,                  // 12
		cfg.PVCSize,                      // 13
		ollamaURL,                        // 14
		cfg.OllamaModel,                  // 15
		embeddingModel,                   // 16
		qdrantURL,                        // 17
		cfg.Host,                         // 18
		AnythingLLMServiceName,           // 19
		cfg.Credentials.AuthToken,        // 20
	)
}
