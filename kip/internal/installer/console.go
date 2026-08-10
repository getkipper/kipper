package installer

import (
	"fmt"
	"regexp"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"

	"github.com/getkipper/kipper/controller/pkg/hostnames"
	"github.com/getkipper/kipper/controller/pkg/serving"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// adminEmailPattern is a conservative email check. It rejects anything
// that is not a plain address, which also guarantees the value is safe to
// interpolate into the seed shell command and the JSON it builds (no
// quotes, semicolons, spaces, or shell metacharacters can appear).
var adminEmailPattern = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$`)

// ConsoleRBACManifest is the ServiceAccount, ClusterRole, and
// ClusterRoleBinding for console-api. It lives outside the deployment
// template because kip upgrade must re-apply it: a permission added for a
// new feature has to reach existing clusters, not only fresh installs.
const ConsoleRBACManifest = `apiVersion: v1
kind: ServiceAccount
metadata:
  name: console-api
  namespace: kipper-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: console-api
rules:
  # Workloads, config and storage the console manages across every project namespace.
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch", "create", "delete"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["get", "create"]
  - apiGroups: [""]
    resources: ["secrets", "configmaps", "services", "persistentvolumeclaims"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  # The project reconciler projects membership onto per-namespace
  # RoleBindings. The bind verb on exactly the three staged project roles
  # lets it reference them without holding their permissions itself; RBAC's
  # escalation prevention rejects the binding writes otherwise.
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["rolebindings"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["clusterroles"]
    verbs: ["bind"]
    resourceNames: ["kipper:project-viewer", "kipper:project-deployer", "kipper:project-owner"]
  # The ClusterIdentity cutover gate reads the API server's active
  # authentication config hash from /metrics to confirm kip has staged the
  # new issuer before the in-cluster Dex flip.
  - nonResourceURLs: ["/metrics"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["namespaces"]
    verbs: ["get", "list", "watch", "create", "update", "delete"]
  # delete: the project reconciler removes both objects when a project
  # goes tierless.
  - apiGroups: [""]
    resources: ["resourcequotas", "limitranges"]
    verbs: ["get", "list", "watch", "create", "update", "delete"]
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["get", "list", "create", "patch"]
  - apiGroups: ["apps"]
    resources: ["deployments", "statefulsets"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["apps"]
    resources: ["daemonsets"]
    verbs: ["get", "list"]
  # Read-only, and both for the same question: which pods a Deployment actually
  # owns, and what still reads an environment before it is retired. A selector
  # match is not ownership, and a template with no pod right now still produces
  # one later.
  - apiGroups: ["apps"]
    resources: ["replicasets"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["replicationcontrollers"]
    verbs: ["get", "list"]
  - apiGroups: ["batch"]
    resources: ["jobs", "cronjobs"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["networking.k8s.io"]
    resources: ["ingresses", "networkpolicies"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["autoscaling"]
    resources: ["horizontalpodautoscalers"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["metrics.k8s.io"]
    resources: ["pods", "nodes"]
    verbs: ["get", "list"]
  # Kipper custom resources and their status subresources.
  - apiGroups: ["kipper.run"]
    resources: ["apps", "services", "functions", "jobs", "volumes", "projects", "datatransfers"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["kipper.run"]
    resources: ["apps/status", "services/status", "functions/status", "jobs/status", "volumes/status", "projects/status", "platformconfigs/status", "datatransfers/status"]
    verbs: ["get", "update", "patch"]
  - apiGroups: ["kipper.run"]
    resources: ["platformconfigs"]
    verbs: ["get", "list", "watch", "update", "patch"]
  # The serving-identity reconciler watches the cluster-scoped ClusterIdentity
  # singleton and writes its status; the Ingress, ConfigMap, Secret and
  # Deployment rules above already grant the objects it drives at cutover
  # (dex-config, kipper-users, dex-oidc-client, the dex and console-api rollouts).
  - apiGroups: ["kipper.run"]
    resources: ["clusteridentities"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["kipper.run"]
    resources: ["clusteridentities/status"]
    verbs: ["get", "update", "patch"]
  - apiGroups: ["kipper.run"]
    resources: ["resourceadjustments"]
    verbs: ["get", "list", "watch", "create"]
  - apiGroups: ["kipper.run"]
    resources: ["usageplans", "apikeys"]
    verbs: ["get", "list", "watch", "create", "update", "delete"]
  # Rollups are written by kipper-authz; the console only reads them for
  # usage views and deletes them past retention.
  - apiGroups: ["kipper.run"]
    resources: ["usagerollups"]
    verbs: ["get", "list", "watch", "delete"]
  # Backup/restore (Velero), scale-to-zero (KEDA), routing (Traefik), packaged installs (Helm).
  - apiGroups: ["velero.io"]
    resources: ["backups", "restores"]
    verbs: ["get", "list", "create", "delete"]
  - apiGroups: ["velero.io"]
    resources: ["schedules"]
    verbs: ["get", "list", "update"]
  - apiGroups: ["keda.sh"]
    resources: ["scaledobjects", "triggerauthentications"]
    verbs: ["get", "list", "watch", "create", "update", "delete"]
  - apiGroups: ["http.keda.sh"]
    resources: ["httpscaledobjects"]
    verbs: ["get", "list", "watch", "create", "update", "delete"]
  # "patch" covers the server-side apply the serving reconciler uses for the
  # platform security-headers Middleware.
  - apiGroups: ["traefik.io"]
    resources: ["middlewares"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  # The hop-cert reconciler owns the default TLSStore (the stable serving
  # certificate the kipper.run gateway pins) and lists every namespace for a
  # competing "default" store, which would displace the pinned certificate.
  - apiGroups: ["traefik.io"]
    resources: ["tlsstores"]
    verbs: ["get", "list", "watch", "create", "update", "patch"]
  - apiGroups: ["helm.cattle.io"]
    resources: ["helmcharts"]
    verbs: ["get", "list", "watch", "create", "update", "delete"]
  - apiGroups: ["monitoring.coreos.com"]
    resources: ["servicemonitors"]
    verbs: ["get", "list", "watch", "create", "update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: console-api
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: console-api
subjects:
  - kind: ServiceAccount
    name: console-api
    namespace: kipper-system
`

const consoleManifestTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: console-api
  namespace: kipper-system
spec:
  replicas: 1
  selector:
    matchLabels:
      app: console-api
  template:
    metadata:
      labels:
        app: console-api
    spec:
      serviceAccountName: console-api
      # console-api holds a cluster-powerful ServiceAccount, so its own pod is
      # locked down: non-root, no privilege escalation, all capabilities
      # dropped, the default seccomp profile, and a read-only root filesystem.
      # The binary reads config from env and talks to the API server; it never
      # writes to its rootfs, so the only writable mount is an ephemeral /tmp.
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: console-api
          image: ghcr.io/getkipper/kipper-console-api:latest
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop:
                - ALL
          ports:
            - containerPort: 8080
            - name: metrics
              containerPort: 8081
          volumeMounts:
            - name: tmp
              mountPath: /tmp
          env:
            - name: DEX_ISSUER
              value: "https://%s/dex"
            - name: DEX_CLIENT_SECRET
              valueFrom:
                secretKeyRef:
                  name: dex-oidc-client
                  key: secret
            - name: DEX_REDIRECT_URI
              value: "https://%s/callback"
            - name: CLUSTER_DOMAIN
              value: "%s"
            # Base domain a post-login redirect or SSO-code mint may target,
            # beyond the console host. No cookie is scoped to it. Empty on
            # *.kipper.run clusters, where hosts are siblings under the shared
            # apex with no safe base to allow (see UIDomainFor).
            - name: UI_DOMAIN
              value: "%s"
            # Resolved hostnames for each Kipper component. These reflect
            # admin overrides set via --console-domain/--console-api-domain/
            # --dex-domain at install time, or default to the SubdomainFor
            # convention when no override was given. Handlers read these to
            # build outbound URLs without re-applying the convention.
            - name: CONSOLE_DOMAIN
              value: "%s"
            - name: CONSOLE_API_DOMAIN
              value: "%s"
            - name: DEX_DOMAIN
              value: "%s"
            # Used by the gateway heartbeat to keep the kipper.run
            # subdomain alive (see console-api/gateway_heartbeat.go).
            # CLUSTER_DOMAIN may later be overwritten with a custom
            # domain by 'kip cluster domain', so we keep the original
            # kipper.run domain in a separate env var.
            - name: KIPPER_RUN_DOMAIN
              value: "%s"
            - name: CLUSTER_HOST
              value: "%s"
            - name: SIDECAR_IMAGE
              value: "ghcr.io/getkipper/kipper-sidecar:latest"
            - name: DATAMOVER_IMAGE
              value: "ghcr.io/getkipper/kipper-datamover:latest"
            # Host-level security controls. Uncommenting requires editing
            # this Deployment on the cluster, which is exactly the point:
            # these outrank any console login.
            #
            # Refuse every outbound cluster migration from this cluster:
            # - name: KIPPER_DISABLE_OUTBOUND_MIGRATION
            #   value: "1"
            #
            # Days a newly enrolled 2FA factor waits before it can authorise
            # a migration (default 7):
            # - name: KIPPER_MIGRATION_2FA_MIN_AGE_DAYS
            #   value: "7"
            #
            # Security events (migration lifecycle, 2FA changes) additionally
            # deliver via these console-independent channels:
            # - name: KIPPER_SECURITY_WEBHOOK
            #   value: "https://hooks.slack.com/services/..."
            # - name: KIPPER_SECURITY_SMTP_HOST
            #   value: "mail.example.com"
            # - name: KIPPER_SECURITY_SMTP_TO
            #   value: "security@example.com"
            # - name: KIPPER_SECURITY_SMTP_USERNAME
            #   value: "alerts@example.com"
            # - name: KIPPER_SECURITY_SMTP_PASSWORD
            #   valueFrom:
            #     secretKeyRef:
            #       name: security-smtp
            #       key: password
      volumes:
        - name: tmp
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: console-api
  namespace: kipper-system
spec:
  selector:
    app: console-api
  ports:
    - name: http
      port: 8080
      targetPort: 8080
    - name: metrics
      port: 8081
      targetPort: metrics
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: console
  namespace: kipper-system
spec:
  replicas: 1
  selector:
    matchLabels:
      app: console
  template:
    metadata:
      labels:
        app: console
    spec:
      containers:
        - name: console
          image: ghcr.io/getkipper/kipper-console:latest
          ports:
            - containerPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: console
  namespace: kipper-system
spec:
  selector:
    app: console
  ports:
    - port: 80
      targetPort: 80
`

// platformIngressTemplate renders one platform Ingress. The first %s pair is
// name/namespace, the third the annotation block, then the TLS host, the
// secretName fragment, the rule host, and the backend paths block.
const platformIngressTemplate = `---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: %s
  namespace: %s%s
spec:
  ingressClassName: traefik
  tls:
    - hosts:
        - "%s"%s
  rules:
    - host: "%s"
      http:
        paths:
%s`

// ingressBackendPath renders one backend path entry for platformIngress.
func ingressBackendPath(path, service string, port int) string {
	return fmt.Sprintf(`          - path: %s
            pathType: Prefix
            backend:
              service:
                name: %s
                port:
                  number: %d
`, path, service, port)
}

// DesiredConsoleAPIDeployment returns the console-api Deployment exactly as a
// fresh install renders it, parsed from that same manifest so an upgrade cannot
// drift from an install. The host-derived values are irrelevant here: callers
// use it for the parts of the pod spec the installer owns — the security context
// and the writable /tmp mount — while the serving reconciler owns the env family
// and image pinning is `kip upgrade`'s own concern.
func DesiredConsoleAPIDeployment() (*appsv1.Deployment, error) {
	manifest := renderConsoleManifest("dex", "console", "console-api", "example.com", "", "")
	for _, doc := range strings.Split(manifest, "\n---\n") {
		var dep appsv1.Deployment
		if err := yaml.Unmarshal([]byte(doc), &dep); err != nil {
			continue
		}
		if dep.Kind == "Deployment" && dep.Name == "console-api" {
			return &dep, nil
		}
	}
	return nil, fmt.Errorf("console-api Deployment not found in the rendered install manifest")
}

// renderConsoleManifest renders the console and console-api Deployments,
// Services, and Ingresses. Split from DeployConsole so tests exercise the
// exact manifest the installer applies.
func renderConsoleManifest(dexHost, consoleHost, consoleAPIHost, bareDomain, kipperRunDomain, host string) string {
	manifest := fmt.Sprintf(consoleManifestTemplate,
		dexHost, consoleHost, bareDomain, UIDomainFor(bareDomain),
		consoleHost, consoleAPIHost, dexHost,
		kipperRunDomain, host)
	// The console and console-api ingresses both live in kipper-system, so one
	// security-headers Middleware there covers both.
	manifest += securityHeadersMiddleware("kipper-system")
	manifest += platformIngress("console", "kipper-system", consoleHost, "console-tls",
		ingressBackendPath("/", "console", 80),
		ingressBackendPath("/api", "console-api", 8080),
		ingressBackendPath("/auth", "console-api", 8080))
	manifest += platformIngress("console-api", "kipper-system", consoleAPIHost, "console-api-tls",
		ingressBackendPath("/", "console-api", 8080))
	return manifest
}

// platformIngress renders a platform Ingress with domain-aware TLS. A
// gateway-fronted *.kipper.run host gets no cert-manager annotation and no
// secretName: issuance is impossible there (the HTTP-01 challenge 404s at the
// gateway), and the host must fall through to the Traefik default store,
// which serves the hop certificate the gateway pins (see
// console-api/internal/hopcert). Custom domains keep cert-manager issuance.
// The rendered spec must match the serving package's ingress builders, which
// re-apply these objects at every identity transition.
func platformIngress(name, namespace, host, tlsSecret string, paths ...string) string {
	// Every platform host gets the security-headers middleware (HSTS etc.); a
	// custom domain additionally gets cert-manager issuance (a kipper.run host
	// serves the pinned hop cert). The middleware lives in the ingress's own
	// namespace, so the reference is <namespace>-<name>@kubernetescrd, and the
	// name comes from the object the same package renders.
	annotations := []string{
		fmt.Sprintf("traefik.ingress.kubernetes.io/router.middlewares: %s-%s@kubernetescrd",
			namespace, serving.SecurityHeadersMiddleware(namespace).GetName()),
	}
	secretFragment := ""
	if !hostnames.IsKipperRun(host) {
		annotations = append(annotations, "cert-manager.io/cluster-issuer: letsencrypt-prod")
		secretFragment = "\n      secretName: " + tlsSecret
	}
	annotationBlock := "\n  annotations:"
	for _, a := range annotations {
		annotationBlock += "\n    " + a
	}
	return fmt.Sprintf(platformIngressTemplate,
		name, namespace, annotationBlock, host, secretFragment, host, strings.Join(paths, ""))
}

// securityHeadersMiddleware renders the Traefik Middleware that sets HSTS and
// the clickjacking / MIME-sniffing / referrer headers on a platform host. The
// spec comes from the serving package, which is also what the reconciler
// applies: two hand-maintained copies would each overwrite the other on every
// reconcile.
func securityHeadersMiddleware(namespace string) string {
	body, err := yaml.Marshal(serving.SecurityHeadersMiddleware(namespace).Object)
	if err != nil {
		// The object is a literal built in this process; marshalling it can only
		// fail if that literal is malformed, which the installer tests catch.
		panic(fmt.Sprintf("rendering security-headers middleware: %v", err))
	}
	return "---\n" + string(body)
}

// DeployConsole installs the Kipper web console and its API backend
// into the kipper-system namespace with an Ingress for the console domain.
//
// dexHost / consoleHost / consoleAPIHost are the resolved component
// hostnames (admin overrides applied or SubdomainFor defaults).
// bareDomain is the cluster's serving base domain (CLUSTER_DOMAIN).
// kipperRunDomain is the registered *.kipper.run domain the gateway
// heartbeat keeps alive — after a custom-domain move it differs from
// bareDomain, so it is a separate input. host is the cluster's public IP
// or hostname.
func DeployConsole(client *ssh.Client, dexHost, consoleHost, consoleAPIHost, bareDomain, kipperRunDomain, host string) error {
	applyRBACCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", ConsoleRBACManifest)
	if _, err := client.Run(applyRBACCmd); err != nil {
		return fmt.Errorf("applying console rbac: %w", err)
	}

	manifest := renderConsoleManifest(dexHost, consoleHost, consoleAPIHost, bareDomain, kipperRunDomain, host)

	applyCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", manifest)
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Errorf("applying console manifest: %w", err)
	}

	// The admin login identity is the Dex static-password account, which
	// InstallDex creates as admin@<bareDomain> from the same domain value.
	// Seed the role store with that exact address — not opts.AdminEmail,
	// which is the Let's Encrypt contact and can differ — so the admin is
	// authorized on first login instead of authenticating and getting 403.
	if err := seedAdminUser(client, "admin@"+bareDomain); err != nil {
		return err
	}

	waitCmd := "kubectl -n kipper-system rollout status deployment/console-api --timeout=120s"
	if _, err := client.Run(waitCmd); err != nil {
		return fmt.Errorf("waiting for console-api: %w", err)
	}

	waitConsoleCmd := "kubectl -n kipper-system rollout status deployment/console --timeout=120s"
	if _, err := client.Run(waitConsoleCmd); err != nil {
		return fmt.Errorf("waiting for console: %w", err)
	}

	return nil
}

// seedAdminUser creates the kipper-users ConfigMap with adminEmail as the
// sole admin, but only when it does not already exist. The console-api
// RoleStore reads this ConfigMap and fails closed for anyone not listed,
// so the admin must be seeded at install rather than auto-granted on first
// login. Create-if-absent (not apply) is deliberate: the RoleStore manages
// this ConfigMap at runtime, so a re-install or upgrade must not reset the
// user list back to just the admin.
func seedAdminUser(client *ssh.Client, adminEmail string) error {
	if !adminEmailPattern.MatchString(adminEmail) {
		return fmt.Errorf("invalid admin email %q: must be a plain email address", adminEmail)
	}
	users := fmt.Sprintf(`{%q:"admin"}`, adminEmail)
	seedCmd := fmt.Sprintf(
		"kubectl -n kipper-system get configmap kipper-users >/dev/null 2>&1 || "+
			"kubectl -n kipper-system create configmap kipper-users "+
			"--from-literal=users='%s'",
		users,
	)
	if _, err := client.Run(seedCmd); err != nil {
		return fmt.Errorf("seeding admin user: %w", err)
	}
	return nil
}
