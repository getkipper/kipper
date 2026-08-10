package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// Registry accounts and the objects that carry them. Two accounts implement
// least privilege: builds hold the push credential, nodes hold only the pull
// credential in their registries.yaml, so a compromised node stays short of
// push access. console-api duplicates the names it consumes (separate module,
// see console-api/builder/builder.go).
const (
	zotPushUser = "kipper-push"
	zotPullUser = "kipper-pull"

	zotNamespace      = "kipper-system"
	zotHtpasswdSecret = "zot-htpasswd"         //nolint:gosec // Secret object name, not a credential
	zotPushSecret     = "zot-push-credentials" //nolint:gosec // Secret object name, not a credential
	zotPullSecret     = "zot-pull-credentials" //nolint:gosec // Secret object name, not a credential
	zotTLSSecret      = "zot-tls"

	zotRegistryHost = "zot.kipper-system.svc.cluster.local:5000"
	zotCAFilePath   = "/etc/rancher/k3s/zot-ca.crt"
)

// zotBaseManifest carries what must exist before the TLS certificate can be
// issued: the Service, because its ClusterIP becomes an IP SAN (containerd
// verifies the mirror endpoint by IP), and the storage. Deliberately not the
// ConfigMap: on an upgrade from an earlier install, nothing that changes the
// running registry's effective configuration is applied until certificates
// and credentials are ready, so a failure in those stages leaves the old
// registry exactly as it was instead of half-migrated.
const zotBaseManifest = `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: zot-data
  namespace: kipper-system
  labels:
    app: zot
    app.kubernetes.io/managed-by: kipper
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: longhorn-single
  resources:
    requests:
      storage: 10Gi
---
apiVersion: v1
kind: Service
metadata:
  name: zot
  namespace: kipper-system
  labels:
    app: zot
    app.kubernetes.io/managed-by: kipper
spec:
  selector:
    app: zot
  ports:
    - port: 5000
      targetPort: 5000
`

// zotConfigJSON is the registry configuration. It enforces authenticated
// access only: htpasswd is the only auth method, defaultPolicy is empty and
// no anonymousPolicy exists, so every request must present one of the two
// accounts. bcrypt is the only hash zot accepts for htpasswd.
//
// accessControl belongs under http, as a peer of auth. At the root, zot rejects
// the whole config with an "invalid keys: accesscontrol" decode error, and since
// the Deployment uses a Recreate strategy that rejection is a registry outage
// rather than a failed apply.
const zotConfigJSON = `{
  "storage": {
    "rootDirectory": "/var/lib/registry",
    "gc": true,
    "gcDelay": "1h"
  },
  "http": {
    "address": "0.0.0.0",
    "port": "5000",
    "tls": {
      "cert": "/etc/zot-tls/tls.crt",
      "key": "/etc/zot-tls/tls.key"
    },
    "auth": {
      "htpasswd": {
        "path": "/etc/zot-auth/htpasswd"
      }
    },
    "accessControl": {
      "repositories": {
        "**": {
          "policies": [
            {
              "users": ["kipper-push"],
              "actions": ["read", "create", "update", "delete"]
            },
            {
              "users": ["kipper-pull"],
              "actions": ["read"]
            }
          ],
          "defaultPolicy": []
        }
      }
    }
  },
  "log": {
    "level": "warn"
  }
}`

// zotConfigMapName derives the registry ConfigMap's name from its content.
// Config changes therefore create a new object instead of mutating one a
// running pod references, which is what makes the cutover below fail
// closed: nothing the old registry uses is ever touched before the
// Deployment update is accepted.
func zotConfigMapName() string {
	sum := sha256.Sum256([]byte(zotConfigJSON))
	return "zot-config-" + hex.EncodeToString(sum[:])[:10]
}

// renderZotRuntimeManifest builds the registry's configuration and workload
// stage: an immutable content-named ConfigMap and the Deployment referencing
// it. kubectl applies the documents sequentially, not atomically — the
// sequence is safe anyway because the ConfigMap is a new object the running
// pod does not reference. The Deployment update is the single cutover point:
// if it is rejected, the old registry keeps running untouched (the orphan
// ConfigMap is cleaned up on the next successful run); once it is accepted,
// the Recreate strategy stops the old pod first, so a rollout failure leaves
// the registry stopped, never serving unauthenticated.
func renderZotRuntimeManifest(configMapName string) string {
	indented := "    " + strings.ReplaceAll(zotConfigJSON, "\n", "\n    ")
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: kipper-system
  labels:
    app: zot
    app.kubernetes.io/managed-by: kipper
immutable: true
data:
  config.json: |
%s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: zot
  namespace: kipper-system
  labels:
    app: zot
    app.kubernetes.io/managed-by: kipper
spec:
  replicas: 1
  # zot is a singleton with an RWO PVC and a boltdb cache that takes an
  # exclusive file lock. The default RollingUpdate strategy spawns the
  # new pod alongside the old one, and the new pod crash-loops trying
  # to open the locked cache.db until ProgressDeadlineExceeded fires.
  # Recreate kills the old pod first so the lock is free when the new
  # pod starts — which is also what makes the auth/TLS cutover fail
  # closed on upgrades.
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app: zot
  template:
    metadata:
      labels:
        app: zot
    spec:
      containers:
        - name: zot
          image: ghcr.io/project-zot/zot-linux-amd64:v2.1.3
          ports:
            - containerPort: 5000
          volumeMounts:
            - name: data
              mountPath: /var/lib/registry
            - name: config
              mountPath: /etc/zot
            - name: auth
              mountPath: /etc/zot-auth
            - name: tls
              mountPath: /etc/zot-tls
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
            limits:
              cpu: 200m
              memory: 256Mi
          # tcpSocket rather than an HTTP probe of /v2/, which answers
          # 401 without credentials, and the kubelet counts anything
          # outside 2xx/3xx as failure. Embedding the credential in the
          # probe would put it in the pod spec.
          readinessProbe:
            tcpSocket:
              port: 5000
            initialDelaySeconds: 5
            periodSeconds: 10
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: zot-data
        - name: config
          configMap:
            name: %s
        - name: auth
          secret:
            secretName: zot-htpasswd
        - name: tls
          secret:
            secretName: zot-tls
`, configMapName, indented, configMapName)
}

// zotCertManifestTemplate issues the registry's TLS material from a
// cluster-internal CA: selfsigned issuer, CA certificate, CA issuer, leaf.
// The single %s is the Service ClusterIP. The ten-year durations remove the
// renewal cliff from the component every image pull depends on (a silently
// expired internal cert is the cert-manager/Velero deadlock failure class
// again); k3s's own internal CAs have the same horizon. localhost and
// 127.0.0.1 are included for kip tunnel access.
const zotCertManifestTemplate = `apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: zot-selfsigned
  namespace: kipper-system
  labels:
    app: zot
    app.kubernetes.io/managed-by: kipper
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: zot-ca
  namespace: kipper-system
  labels:
    app: zot
    app.kubernetes.io/managed-by: kipper
spec:
  isCA: true
  commonName: kipper-zot-ca
  secretName: zot-ca
  duration: 87600h
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    name: zot-selfsigned
    kind: Issuer
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: zot-ca-issuer
  namespace: kipper-system
  labels:
    app: zot
    app.kubernetes.io/managed-by: kipper
spec:
  ca:
    secretName: zot-ca
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: zot-tls
  namespace: kipper-system
  labels:
    app: zot
    app.kubernetes.io/managed-by: kipper
spec:
  secretName: zot-tls
  duration: 87600h
  dnsNames:
    - zot.kipper-system.svc.cluster.local
    - zot.kipper-system.svc
    - localhost
  ipAddresses:
    - %s
    - 127.0.0.1
  issuerRef:
    name: zot-ca-issuer
    kind: Issuer
`

// renderZotHtpasswd produces the bcrypt htpasswd file zot requires. MinCost
// is a deliberate choice: both passwords are 128-bit random values whose
// strength is their entropy, not their hash cost, and containerd presents
// basic auth on every registry request, so a default-cost hash would add
// tens of milliseconds of bcrypt to each blob fetch of every image pull.
func renderZotHtpasswd(pushPassword, pullPassword string) (string, error) {
	pushHash, err := bcrypt.GenerateFromPassword([]byte(pushPassword), bcrypt.MinCost)
	if err != nil {
		return "", fmt.Errorf("hashing push password: %w", err)
	}
	pullHash, err := bcrypt.GenerateFromPassword([]byte(pullPassword), bcrypt.MinCost)
	if err != nil {
		return "", fmt.Errorf("hashing pull password: %w", err)
	}
	return fmt.Sprintf("%s:%s\n%s:%s\n", zotPushUser, pushHash, zotPullUser, pullHash), nil
}

// renderZotRegistriesConfig builds the k3s registries.yaml that lets every
// node pull from the registry: the mirror redirects the in-cluster name to
// the ClusterIP over TLS, and configs (keyed by the endpoint's host:port,
// which is how containerd looks credentials up) carries the pull credential
// and the CA. Passwords are hex from generateSecret, so quoting them is
// enough for YAML safety.
func renderZotRegistriesConfig(clusterIP, pullPassword string) string {
	return fmt.Sprintf(`mirrors:
  "%s":
    endpoint:
      - "https://%s:5000"
configs:
  "%s:5000":
    auth:
      username: "%s"
      password: "%s"
    tls:
      ca_file: %s
`, zotRegistryHost, clusterIP, clusterIP, zotPullUser, pullPassword, zotCAFilePath)
}

// ensureZotCredentials creates the registry accounts on first install and
// leaves them untouched afterwards: nodes keep the pull password in their
// registries.yaml, so a re-run (kip upgrade re-invokes InstallZot) must
// never rotate credentials as a side effect. The htpasswd is re-derived only
// when it is missing or a password was just generated. Returns the pull
// password for the node configuration.
func ensureZotCredentials(client *ssh.Client) (string, error) {
	pushPassword, err := readSecretValue(client, zotNamespace, zotPushSecret, "password")
	if err != nil {
		return "", err
	}
	pullPassword, err := readSecretValue(client, zotNamespace, zotPullSecret, "password")
	if err != nil {
		return "", err
	}

	generated := false
	if pushPassword == "" {
		if pushPassword, err = generateSecret(16); err != nil {
			return "", fmt.Errorf("generating registry push password: %w", err)
		}
		if _, err := client.RunStdin(applySecretCmd(zotNamespace, zotPushSecret, "password"), strings.NewReader(pushPassword)); err != nil {
			return "", fmt.Errorf("storing registry push credential: %w", err)
		}
		generated = true
	}
	if pullPassword == "" {
		if pullPassword, err = generateSecret(16); err != nil {
			return "", fmt.Errorf("generating registry pull password: %w", err)
		}
		if _, err := client.RunStdin(applySecretCmd(zotNamespace, zotPullSecret, "password"), strings.NewReader(pullPassword)); err != nil {
			return "", fmt.Errorf("storing registry pull credential: %w", err)
		}
		generated = true
	}

	htpasswd, err := readSecretValue(client, zotNamespace, zotHtpasswdSecret, "htpasswd")
	if err != nil {
		return "", err
	}
	if htpasswd == "" || generated {
		content, err := renderZotHtpasswd(pushPassword, pullPassword)
		if err != nil {
			return "", err
		}
		if _, err := client.RunStdin(applySecretCmd(zotNamespace, zotHtpasswdSecret, "htpasswd"), strings.NewReader(content)); err != nil {
			return "", fmt.Errorf("storing registry htpasswd: %w", err)
		}
	}
	return pullPassword, nil
}

// writeZotNodeFiles places the registry CA and the authenticated mirror
// config on one node. containerd reads both from node-local paths, so every
// node needs its own copy. Both files travel over stdin and land with mode
// 600: registries.yaml carries the pull password, and it must never appear
// in a command string, where /proc makes it world-readable while it runs.
func writeZotNodeFiles(client *ssh.Client, caPEM, clusterIP, pullPassword string) error {
	if _, err := client.Run("mkdir -p /etc/rancher/k3s"); err != nil {
		return fmt.Errorf("creating k3s config directory: %w", err)
	}
	writeCA := fmt.Sprintf("cat > %s && chmod 600 %s", zotCAFilePath, zotCAFilePath)
	if _, err := client.RunStdin(writeCA, strings.NewReader(caPEM)); err != nil {
		return fmt.Errorf("writing zot CA: %w", err)
	}
	const registriesPath = "/etc/rancher/k3s/registries.yaml"
	writeRegistries := fmt.Sprintf("cat > %s && chmod 600 %s", registriesPath, registriesPath)
	if _, err := client.RunStdin(writeRegistries, strings.NewReader(renderZotRegistriesConfig(clusterIP, pullPassword))); err != nil {
		return fmt.Errorf("writing registries config: %w", err)
	}
	return nil
}

// verifyZotAuth checks from the host that the registry actually enforces
// what was configured: anonymous /v2/ must be refused with 401 and the pull
// credential must be accepted over verified TLS. An unauthenticated registry
// must fail the install rather than survive silently. The credential goes to
// curl through a config file on stdin (-K), never through argv.
func verifyZotAuth(client *ssh.Client, clusterIP, pullPassword string) error {
	anonCmd := fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' --cacert %s https://%s:5000/v2/", zotCAFilePath, clusterIP)
	anon, err := client.Run(anonCmd)
	if err != nil {
		return fmt.Errorf("probing zot anonymously: %w", err)
	}
	if code := strings.TrimSpace(anon); code != "401" {
		return fmt.Errorf("zot answered anonymous /v2/ with %s instead of 401: the registry is not enforcing authentication", code)
	}

	authedCmd := fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' --cacert %s -K /dev/stdin https://%s:5000/v2/", zotCAFilePath, clusterIP)
	curlConfig := fmt.Sprintf("user = \"%s:%s\"\n", zotPullUser, pullPassword)
	authed, err := client.RunStdin(authedCmd, strings.NewReader(curlConfig))
	if err != nil {
		return fmt.Errorf("probing zot with the pull credential: %w", err)
	}
	if code := strings.TrimSpace(authed); code != "200" {
		return fmt.Errorf("zot answered authenticated /v2/ with %s instead of 200", code)
	}
	return nil
}

// zotRolloutDiagnosis asks the registry's pod why it is not running and returns
// a sentence to append to a rollout failure. It reads only — the cluster is
// already in the failed state and the operator needs the reason, not a second
// mutation.
func zotRolloutDiagnosis(client *ssh.Client) string {
	out, err := client.Run("kubectl -n kipper-system logs -l app=zot --tail=5 --all-containers=true 2>&1 || true")
	if err != nil {
		return ""
	}
	return formatZotDiagnosis(out)
}

// formatZotDiagnosis condenses the registry's last log lines into one
// parenthesised clause. A rejected config is the likely cause and zot names the
// offending key, so the first non-empty line is usually the whole answer.
func formatZotDiagnosis(logs string) string {
	for _, line := range strings.Split(logs, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		const limit = 300
		if len(line) > limit {
			line = line[:limit] + "…"
		}
		return " (the registry pod says: " + line + ")"
	}
	return ""
}

// InstallZot deploys the Zot OCI registry with htpasswd authentication and
// TLS from a cluster-internal CA, and configures k3s to pull from it with
// the read-only credential. Zot provides internal image storage for
// Git-based builds via Kaniko; builds authenticate with a separate push
// credential read by console-api. Safe to re-run: credentials are created
// once and reused, manifests apply idempotently.
func InstallZot(client *ssh.Client) error {
	// Service, config, and storage first: the Service's ClusterIP becomes an
	// IP SAN in the certificate, so it has to exist before issuance.
	applyBase := fmt.Sprintf("cat << 'KIPEOF' | kubectl apply -f -\n%sKIPEOF", zotBaseManifest)
	if _, err := client.Run(applyBase); err != nil {
		return fmt.Errorf("applying zot manifests: %w", err)
	}

	// containerd runs on the host and cannot resolve cluster-internal DNS
	// names like zot.kipper-system.svc.cluster.local, so the mirror endpoint
	// (and therefore the certificate) uses the ClusterIP directly.
	clusterIP, err := client.Run(`kubectl get svc zot -n kipper-system -o jsonpath='{.spec.clusterIP}'`)
	if err != nil {
		return fmt.Errorf("getting zot ClusterIP: %w", err)
	}
	clusterIP = strings.TrimSpace(clusterIP)
	if clusterIP == "" {
		return fmt.Errorf("zot service has no ClusterIP")
	}

	pullPassword, err := ensureZotCredentials(client)
	if err != nil {
		return err
	}

	applyCerts := fmt.Sprintf("cat << 'KIPEOF' | kubectl apply -f -\n%sKIPEOF", fmt.Sprintf(zotCertManifestTemplate, clusterIP))
	if _, err := client.Run(applyCerts); err != nil {
		return fmt.Errorf("applying zot certificates: %w", err)
	}
	if _, err := client.Run("kubectl -n kipper-system wait --for=condition=Ready certificate/zot-tls --timeout=120s"); err != nil {
		return fmt.Errorf("waiting for zot certificate: %w", err)
	}

	// Config and Deployment land only now, as the last stage: every earlier
	// failure leaves an existing registry untouched, and the content-named
	// ConfigMap keeps that true through this stage too (see
	// renderZotRuntimeManifest for the cutover semantics). The Deployment
	// also needs the TLS secret to exist, or its pod would sit in
	// CreateContainerConfigError until the step times out.
	configMapName := zotConfigMapName()
	applyRuntime := fmt.Sprintf("cat << 'KIPEOF' | kubectl apply -f -\n%sKIPEOF", renderZotRuntimeManifest(configMapName))
	if _, err := client.Run(applyRuntime); err != nil {
		return fmt.Errorf("applying zot config and deployment: %w", err)
	}
	if _, err := client.Run("kubectl -n kipper-system rollout status deployment/zot --timeout=180s"); err != nil {
		// Recreate means the old pod is already gone, so a pod that will not
		// start leaves no registry at all, and the rollout status says only that
		// it timed out. Carry the pod's own complaint into the error instead —
		// rolling back would restore the previous revision, which on the upgrade
		// that introduces auth and TLS is the unauthenticated registry.
		return fmt.Errorf("waiting for zot: %w%s", err, zotRolloutDiagnosis(client))
	}

	// Drop superseded config objects (including the pre-auth zot-config and
	// orphans from failed earlier runs). Cleanup only — the live config is
	// excluded by name, and a failure here must not fail the install.
	cleanupCmd := fmt.Sprintf(
		"kubectl -n kipper-system delete configmap -l app=zot,app.kubernetes.io/managed-by=kipper --field-selector 'metadata.name!=%s' --ignore-not-found",
		configMapName)
	if _, err := client.Run(cleanupCmd); err != nil {
		fmt.Printf("  ⚠  could not remove superseded zot config objects: %v\n", err)
	}

	caPEM, err := readSecretValue(client, zotNamespace, zotTLSSecret, `ca\.crt`)
	if err != nil {
		return err
	}
	if caPEM == "" {
		return fmt.Errorf("zot TLS secret carries no ca.crt")
	}
	if err := writeZotNodeFiles(client, caPEM, clusterIP, pullPassword); err != nil {
		return err
	}

	// Restart k3s to pick up the registries config. containerd and the
	// running pods survive the restart, so zot stays up through it.
	if _, err := client.Run("systemctl restart k3s"); err != nil {
		return fmt.Errorf("restarting k3s: %w", err)
	}
	if _, err := client.Run("kubectl wait --for=condition=Ready node --all --timeout=120s"); err != nil {
		return fmt.Errorf("waiting for k3s after restart: %w", err)
	}

	return verifyZotAuth(client, clusterIP, pullPassword)
}
