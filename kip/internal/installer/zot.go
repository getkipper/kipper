package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"golang.org/x/crypto/bcrypt"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// Builds use the push account; nodes use the read-only pull account.
// console-api defines the account and secret names it uses separately
// (see console-api/builder/builder.go).
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

// zotBaseManifestTemplate creates storage and the Service before certificate
// issuance, which needs the Service's ClusterIP. The ConfigMap and Deployment
// are applied later, once credentials and certificates are ready.
const zotBaseManifestTemplate = `apiVersion: v1
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
      storage: %s
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

const zotDefaultStorage = "10Gi"

// zotStorageQuery wraps the requested size and capacity in zot-size[...]
// so the parser can separate them from surrounding SSH output.
// A missing claim produces no output.
const zotStorageQuery = `kubectl -n kipper-system get pvc zot-data --ignore-not-found ` +
	`-o jsonpath='{"zot-size["}{.spec.resources.requests.storage}{"|"}{.status.capacity.storage}{"]"}' 2>/dev/null`

const zotStorageMarker = "zot-size["

var zotStoragePayload = regexp.MustCompile(`zot-size\[([^|\]]*)\|([^|\]]*)\]`)

func renderZotBaseManifest(storage string) string {
	return fmt.Sprintf(zotBaseManifestTemplate, storage)
}

// zotStorageRequest preserves expanded volumes by choosing the largest of
// the default size, requested size, and capacity. Output without a marker
// uses the default; marked output must contain a valid size record.
func zotStorageRequest(out string) (string, error) {
	if !strings.Contains(out, zotStorageMarker) {
		return zotDefaultStorage, nil
	}
	matches := zotStoragePayload.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		return "", fmt.Errorf("could not read registry volume size from %q", out)
	}
	request, capacity := matches[len(matches)-1][1], matches[len(matches)-1][2]
	if request == "" {
		return "", fmt.Errorf("registry volume has no requested size")
	}
	size := resource.MustParse(zotDefaultStorage)
	for _, f := range []string{request, capacity} {
		if f == "" {
			continue
		}
		q, err := resource.ParseQuantity(f)
		if err != nil {
			return "", fmt.Errorf("reading registry volume size %q: %w", f, err)
		}
		if q.Cmp(size) > 0 {
			size = q
		}
	}
	return size.String(), nil
}

// zotConfigJSON gives the push account read/write access and the pull account
// read-only access. Other users have no permissions.
// Keep accessControl under http, alongside auth.
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

// zotConfigMapName includes a content hash so configuration changes create
// a separate ConfigMap for the new Deployment revision.
func zotConfigMapName() string {
	sum := sha256.Sum256([]byte(zotConfigJSON))
	return "zot-config-" + hex.EncodeToString(sum[:])[:10]
}

// renderZotRuntimeManifest pairs an immutable ConfigMap with its Deployment.
// Applying a changed configuration creates a new ConfigMap before updating
// the Deployment, preserving the running pod's ConfigMap until rollout.
// Recreate stops the old pod before starting its replacement.
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
  # Stop the old pod before its replacement accesses the shared registry data.
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
          # A TCP probe checks the listener without putting credentials in the pod spec.
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

// zotCertManifestTemplate creates an internal CA and registry certificate,
// both with a requested lifetime of ten years to reduce renewal frequency.
// The %s placeholder is the Service ClusterIP; localhost and 127.0.0.1
// support access through kip tunnel.
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

// renderZotHtpasswd uses bcrypt.MinCost to limit authentication overhead.
// Generated passwords contain 128 bits of randomness.
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

// renderZotRegistriesConfig routes registry pulls to the ClusterIP over TLS.
// Credentials and the CA are configured under the mirror endpoint's host:port.
// Generated passwords are hexadecimal, so they are safe in quoted YAML.
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

// ensureZotCredentials reuses existing passwords because nodes store the
// pull password locally. It creates missing passwords and rebuilds htpasswd
// when it is missing or a password was generated, then returns the pull password.
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

// writeZotNodeFiles writes the registry CA and pull configuration on one node
// with mode 600. Passing file contents through stdin keeps the pull password
// out of command arguments.
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

// verifyZotAuth requires /v2/ to return 401 for anonymous requests and 200
// with the pull credential, both over verified TLS. The credential reaches
// curl through stdin to keep it out of command arguments.
func verifyZotAuth(client *ssh.Client, clusterIP, pullPassword string) error {
	anonCmd := fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' --cacert %s https://%s:5000/v2/", zotCAFilePath, clusterIP)
	anon, err := client.Run(anonCmd)
	if err != nil {
		return fmt.Errorf("probing zot anonymously: %w", err)
	}
	if code := strings.TrimSpace(anon); code != "401" {
		return fmt.Errorf("zot returned HTTP %s for an anonymous request to /v2/; expected 401 to confirm authentication is required", code)
	}

	authedCmd := fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' --cacert %s -K /dev/stdin https://%s:5000/v2/", zotCAFilePath, clusterIP)
	curlConfig := fmt.Sprintf("user = \"%s:%s\"\n", zotPullUser, pullPassword)
	authed, err := client.RunStdin(authedCmd, strings.NewReader(curlConfig))
	if err != nil {
		return fmt.Errorf("probing zot with the pull credential: %w", err)
	}
	if code := strings.TrimSpace(authed); code != "200" {
		return fmt.Errorf("zot returned HTTP %s for an authenticated request to /v2/; expected 200", code)
	}
	return nil
}

// zotRolloutDiagnosis reads recent pod logs to add context to rollout errors.
func zotRolloutDiagnosis(client *ssh.Client) string {
	out, err := client.Run("kubectl -n kipper-system logs -l app=zot --tail=5 --all-containers=true 2>&1 || true")
	if err != nil {
		return ""
	}
	return formatZotDiagnosis(out)
}

// formatZotDiagnosis includes the first non-empty log line in a rollout error,
// truncated after 300 bytes to keep the message readable.
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
		return " (registry pod log: " + line + ")"
	}
	return ""
}

// InstallZot deploys the Zot OCI registry with authentication and TLS, then
// configures k3s to use the pull account. Builds use a separate push account.
// Repeated installs reuse existing credentials.
func InstallZot(client *ssh.Client) error {
	// Preserve the existing volume size before applying the storage manifest.
	// Stop on lookup errors to avoid requesting a smaller volume.
	existing, err := client.Run(zotStorageQuery)
	if err != nil {
		return fmt.Errorf("reading the registry volume size: %w", err)
	}
	storage, err := zotStorageRequest(existing)
	if err != nil {
		return err
	}
	applyBase := fmt.Sprintf("cat << 'KIPEOF' | kubectl apply -f -\n%sKIPEOF", renderZotBaseManifest(storage))
	if _, err := client.Run(applyBase); err != nil {
		return fmt.Errorf("applying zot manifests: %w", err)
	}

	// Use the ClusterIP for the mirror endpoint and certificate so host-side
	// image pulls work without cluster DNS.
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

	// Apply the runtime configuration once credentials and the TLS certificate
	// are ready, so the new pod can mount its required secrets.
	configMapName := zotConfigMapName()
	applyRuntime := fmt.Sprintf("cat << 'KIPEOF' | kubectl apply -f -\n%sKIPEOF", renderZotRuntimeManifest(configMapName))
	if _, err := client.Run(applyRuntime); err != nil {
		return fmt.Errorf("applying zot config and deployment: %w", err)
	}
	if _, err := client.Run("kubectl -n kipper-system rollout status deployment/zot --timeout=180s"); err != nil {
		// Include pod logs to help diagnose the rollout failure. Avoid rollback:
		// an older revision may allow unauthenticated access.
		return fmt.Errorf("waiting for zot: %w%s", err, zotRolloutDiagnosis(client))
	}

	// Remove old ConfigMaps after a successful rollout, preserving the current
	// one. Cleanup failures are warnings so installation can continue.
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
		return fmt.Errorf("zot TLS secret is missing ca.crt")
	}
	if err := writeZotNodeFiles(client, caPEM, clusterIP, pullPassword); err != nil {
		return err
	}

	// Restart k3s to load the registry configuration.
	if _, err := client.Run("systemctl restart k3s"); err != nil {
		return fmt.Errorf("restarting k3s: %w", err)
	}
	// Allow time for the node to become ready after the restart.
	if err := WaitForNodeReady(client, 5*time.Minute); err != nil {
		return fmt.Errorf("waiting for k3s after restart: %w", err)
	}

	return verifyZotAuth(client, clusterIP, pullPassword)
}
