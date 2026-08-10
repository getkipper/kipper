package installer

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

const dexManifestTemplate = `apiVersion: v1
kind: Namespace
metadata:
  name: dex
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: dex-config
  namespace: dex
data:
  config.yaml: |
    issuer: https://%s/dex
    storage:
      type: kubernetes
      config:
        inCluster: true
    web:
      http: 0.0.0.0:5556
    # Token lifetimes are the platform's session policy, so every value is
    # explicit. ID tokens are minutes-lived because the Kubernetes API
    # server will accept them as cluster credentials and a bearer token
    # cannot be revoked, only outlived; both the CLI and the console renew
    # silently through rotating refresh tokens. Refresh rotation stays on
    # (the default); reuseInterval absorbs near-simultaneous refreshes from
    # retried requests, concurrent tabs, and multiple machines. The idle
    # and absolute lifetimes bound how long an unused or stolen refresh
    # token stays alive.
    expiry:
      idTokens: "15m"
      authRequests: "10m"
      refreshTokens:
        reuseInterval: "30s"
        validIfNotUsedFor: "168h"
        absoluteLifetime: "720h"
    frontend:
      issuer: Kipper
      logoURL: https://%s/logo-stacked-light.svg
      theme: light
    enablePasswordDB: true
    connectors: []
    staticPasswords:
      - email: "admin@%s"
        hash: "%s"
        username: admin
    oauth2:
      skipApprovalScreen: true
    staticClients:
      - id: kipper-console
        name: Kipper Console
        redirectURIs:
          - https://%s/callback
        secretEnv: DEX_CLIENT_SECRET
      - id: kipper-cli
        name: Kipper CLI
        public: true
        redirectURIs:
          - http://localhost:18741/callback
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: dex
  namespace: dex
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: dex
rules:
  - apiGroups: ["dex.coreos.com"]
    resources: ["*"]
    verbs: ["*"]
  - apiGroups: ["apiextensions.k8s.io"]
    resources: ["customresourcedefinitions"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: dex
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: dex
subjects:
  - kind: ServiceAccount
    name: dex
    namespace: dex
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dex
  namespace: dex
spec:
  replicas: 1
  selector:
    matchLabels:
      app: dex
  template:
    metadata:
      labels:
        app: dex
    spec:
      serviceAccountName: dex
      containers:
        - name: dex
          # Digest-pinned: Dex signs the identities the whole platform (and
          # soon the Kubernetes API server) trusts, and a version tag is
          # mutable where a digest is not. Digest of v2.41.1, verified
          # 2026-07-20 against ghcr.
          image: ghcr.io/dexidp/dex:v2.41.1@sha256:bc7cfce7c17f52864e2bb2a4dc1d2f86a41e3019f6d42e81d92a301fad0c8a1d
          command: ["dex", "serve", "/etc/dex/config.yaml"]
          env:
            # Dex expands secretEnv (staticClients[0].secretEnv) from this. The
            # Secret is created before this Deployment, and Dex reads it with an
            # unchecked os.Getenv, so an empty value silently breaks the console
            # OAuth flow — the Secret must exist and be non-empty first.
            - name: DEX_CLIENT_SECRET
              valueFrom:
                secretKeyRef:
                  name: dex-oidc-client
                  key: secret
          ports:
            - containerPort: 5556
          volumeMounts:
            - name: config
              mountPath: /etc/dex
      volumes:
        - name: config
          configMap:
            name: dex-config
---
apiVersion: v1
kind: Service
metadata:
  name: dex
  namespace: dex
spec:
  selector:
    app: dex
  ports:
    - port: 5556
      targetPort: 5556
`

// dexOIDCClientSecretName is the Secret holding the kipper-console OAuth client
// secret. It lives in both the dex namespace (Dex expands it via secretEnv) and
// kipper-system (console-api mounts it via secretKeyRef), so the render never
// generates the secret and a reconfigure never regenerates it.
const dexOIDCClientSecretName = "dex-oidc-client" //nolint:gosec // G101: a Secret object name, not a credential

// renderDexManifest renders the Dex ConfigMap, Deployment, Service, and
// Ingress. Split from InstallDex so tests exercise the exact manifest the
// installer applies.
func renderDexManifest(dexHost, consoleHost, adminEmailDomain, adminPasswordHash string) string {
	manifest := fmt.Sprintf(dexManifestTemplate, dexHost, consoleHost, adminEmailDomain, adminPasswordHash, consoleHost)
	manifest += securityHeadersMiddleware("dex")
	manifest += platformIngress("dex", "dex", dexHost, "dex-tls",
		ingressBackendPath("/", "dex", 5556))
	return manifest
}

// InstallDex deploys Dex as the identity provider for the cluster.
//
// dexHost and consoleHost are the resolved hostnames for Dex and the
// console (admin overrides applied). adminEmailDomain is the bare
// cluster domain used to form the admin email "admin@<adminEmailDomain>".
//
// The console OAuth client secret is provisioned into the dex-oidc-client Secret
// once (generated on first install, reused on every re-run) before the Dex
// Deployment, which references it by env — so re-running install never mints a
// new secret and breaks the console flow.
func InstallDex(client *ssh.Client, dexHost, consoleHost, adminEmailDomain, adminPasswordHash string) error {
	if err := ensureDexClientSecret(client); err != nil {
		return err
	}

	manifest := renderDexManifest(dexHost, consoleHost, adminEmailDomain, adminPasswordHash)

	applyCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", manifest)
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Errorf("applying dex manifest: %w", err)
	}

	// Restart Dex to pick up any ConfigMap changes. On a fresh install
	// this is a no-op (the deployment was just created). On a re-run it
	// ensures Dex reloads the updated config.
	restartCmd := "kubectl rollout restart deployment/dex -n dex"
	if _, err := client.Run(restartCmd); err != nil {
		return fmt.Errorf("restarting dex: %w", err)
	}

	waitCmd := "kubectl -n dex rollout status deployment/dex --timeout=120s"
	if _, err := client.Run(waitCmd); err != nil {
		return fmt.Errorf("waiting for dex: %w", err)
	}

	return nil
}

// ensureDexClientSecret provisions the kipper-console client secret into the
// dex-oidc-client Secret in both the dex and kipper-system namespaces, reusing an
// existing value so a re-run preserves it. It runs before the Dex Deployment so
// the secretEnv reference resolves to a non-empty value on first boot.
func ensureDexClientSecret(client *ssh.Client) error {
	// Ensure both namespaces exist; the Secret has to precede the Dex Deployment
	// that references it, and dex-oidc-client also mirrors into kipper-system for
	// console-api.
	for _, ns := range []string{"dex", "kipper-system"} {
		if _, err := client.Run(ensureNamespaceCmd(ns)); err != nil {
			return fmt.Errorf("ensuring namespace %s: %w", ns, err)
		}
	}

	value, err := readDexClientSecret(client)
	if err != nil {
		return fmt.Errorf("reading existing dex client secret: %w", err)
	}
	if value == "" {
		value, err = generateSecret(32)
		if err != nil {
			return fmt.Errorf("generating client secret: %w", err)
		}
	}

	for _, ns := range []string{"dex", "kipper-system"} {
		if _, err := client.RunStdin(applySecretCmd(ns, dexOIDCClientSecretName, "secret"), strings.NewReader(value)); err != nil {
			return fmt.Errorf("applying %s secret in %s: %w", dexOIDCClientSecretName, ns, err)
		}
	}
	return nil
}

// readDexClientSecret returns the existing client secret from either namespace,
// or empty only when the Secret is genuinely absent from both. A read that fails
// for any other reason (API unavailable, RBAC) is returned as an error so the
// caller stops rather than minting a fresh secret over a live one.
func readDexClientSecret(client *ssh.Client) (string, error) {
	for _, ns := range []string{"dex", "kipper-system"} {
		value, err := readSecretValue(client, ns, dexOIDCClientSecretName, "secret")
		if err != nil {
			return "", err
		}
		if v := strings.TrimSpace(value); v != "" {
			return v, nil
		}
	}
	return "", nil
}

func generateSecret(bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
