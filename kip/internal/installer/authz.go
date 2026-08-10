package installer

import (
	"fmt"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// authzManifest deploys kipper-authz, the forwardAuth data plane for
// API-key-gated routes. The shape encodes the fail-closed contract:
//
//   - Two replicas with a PodDisruptionBudget and a surge-only rolling
//     update, because every unready replica narrows the gate for opted-in
//     routes.
//   - Readiness probes hit /readyz, which reports the cache-freshness
//     clock; Traefik only routes to replicas that can prove a fresh view
//     of keys and plans.
//   - The NetworkPolicy admits request traffic from Traefik and metrics
//     scrapes from monitoring; egress stays open for the API server and
//     DNS, which k3s serves from node IPs that cannot be pinned in a
//     portable policy.
const authzManifest = `apiVersion: v1
kind: ServiceAccount
metadata:
  name: kipper-authz
  namespace: kipper-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kipper-authz
rules:
  - apiGroups: ["kipper.run"]
    resources: ["apikeys", "usageplans", "usagerollups"]
    verbs: ["get", "list", "watch"]
  # The usage flusher writes rollups in the app namespaces where keys live, so
  # this write stays cluster-wide.
  - apiGroups: ["kipper.run"]
    resources: ["usagerollups"]
    verbs: ["create", "update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kipper-authz
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kipper-authz
subjects:
  - kind: ServiceAccount
    name: kipper-authz
    namespace: kipper-system
---
# The freshness canary: each replica proves its watch pipeline is alive by
# writing a timestamp annotation onto one canary per request-path type and
# observing the write come back through its informer cache. The apikeys and
# usageplans canary writes are a namespaced Role bound only in kipper-system, so
# the grant cannot touch a same-named object in any other namespace. The
# usagerollups canary write rides the cluster-wide rule above (the flusher needs
# it). No create verb by design: authz validates keys and must not be able to
# mint them, so a deleted canary is restored by re-applying this manifest.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kipper-authz-canary
  namespace: kipper-system
rules:
  - apiGroups: ["kipper.run"]
    resources: ["apikeys", "usageplans"]
    resourceNames: ["authz-canary"]
    verbs: ["update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: kipper-authz-canary
  namespace: kipper-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kipper-authz-canary
subjects:
  - kind: ServiceAccount
    name: kipper-authz
    namespace: kipper-system
---
apiVersion: kipper.run/v1alpha1
kind: ApiKey
metadata:
  name: authz-canary
  namespace: kipper-system
spec:
  displayName: authz freshness canary
  plan: canary
  enabled: false
  prefix: canary00
  hashSHA256: "0000000000000000000000000000000000000000000000000000000000000000"
---
# Freshness canaries for the other two request-path types. Each replica probes
# all three watches; a wedged UsagePlan or UsageRollup watch stalls the replica
# too. Their spec is inert: the canary prefix belongs to the disabled canary key
# above (never a real request), and the ancient day is never the current one.
apiVersion: kipper.run/v1alpha1
kind: UsagePlan
metadata:
  name: authz-canary
  namespace: kipper-system
spec:
  displayName: authz freshness canary
  rate: 1
  burst: 1
---
apiVersion: kipper.run/v1alpha1
kind: UsageRollup
metadata:
  name: authz-canary
  namespace: kipper-system
spec:
  keyPrefix: canary00
  day: "2000-01-01"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kipper-authz
  namespace: kipper-system
spec:
  replicas: 2
  strategy:
    rollingUpdate:
      maxUnavailable: 0
      maxSurge: 1
  selector:
    matchLabels:
      app: kipper-authz
  template:
    metadata:
      labels:
        app: kipper-authz
    spec:
      serviceAccountName: kipper-authz
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
            - weight: 100
              podAffinityTerm:
                topologyKey: kubernetes.io/hostname
                labelSelector:
                  matchLabels:
                    app: kipper-authz
      containers:
        - name: kipper-authz
          image: ghcr.io/getkipper/kipper-authz:latest
          ports:
            - containerPort: 8080
          resources:
            requests:
              cpu: 25m
              memory: 32Mi
            limits:
              cpu: 200m
              memory: 128Mi
          readinessProbe:
            httpGet:
              path: /readyz
              port: 8080
            periodSeconds: 5
            failureThreshold: 2
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8080
            periodSeconds: 10
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: kipper-authz
  namespace: kipper-system
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app: kipper-authz
---
apiVersion: v1
kind: Service
metadata:
  name: kipper-authz
  namespace: kipper-system
spec:
  selector:
    app: kipper-authz
  ports:
    - port: 8080
      targetPort: 8080
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kipper-authz
  namespace: kipper-system
spec:
  podSelector:
    matchLabels:
      app: kipper-authz
  policyTypes:
    - Ingress
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: traefik
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: monitoring
      ports:
        - port: 8080
`

// InstallAuthz deploys the API-key authorization service.
func InstallAuthz(client *ssh.Client) error {
	applyCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", authzManifest)
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Errorf("applying authz manifest: %w", err)
	}
	return nil
}
