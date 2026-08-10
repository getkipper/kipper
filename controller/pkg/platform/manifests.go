package platform

import "fmt"

// Chart versions pinned for the platform layer. Bumping these requires
// re-running install or upgrade; the reconciler does not migrate releases
// between major versions on its own.
const (
	LokiChartVersion           = "6.55.0"
	PromtailChartVersion       = "6.16.6"
	KubePrometheusStackVersion = "82.10.5"
)

// HelmChartNamespace is where helm-controller HelmChart resources live and
// where the resulting releases land. Both the installer and the reconciler
// agree on this so create/delete/patch all target the same object.
const HelmChartNamespace = "kube-system"

// Grafana admin credentials. The password is generated once into a Secret
// the chart reads via grafana.admin.existingSecret, so it is never the
// static value the chart would otherwise bake in, and it stays stable across
// reinstall, upgrade, and reconcile re-apply. The installer and the
// reconciler both provision this Secret, so its coordinates live here as the
// one definition they share. Grafana has no public ingress (reached over
// kubectl port-forward), so the password gates local access only.
const (
	// MonitoringNamespace is the target namespace for the monitoring stack.
	MonitoringNamespace = "monitoring"
	// GrafanaAdminSecretName is the Kipper-owned Secret holding the admin
	// credentials. Distinct from the chart's own grafana Secret so the two
	// never collide.
	GrafanaAdminSecretName = "grafana-admin"
	// GrafanaAdminUserKey and GrafanaAdminPasswordKey are the Secret keys the
	// chart's userKey/passwordKey point at.
	GrafanaAdminUserKey     = "admin-user"
	GrafanaAdminPasswordKey = "admin-password"
	// GrafanaAdminUser is the fixed admin username stored under the user key.
	GrafanaAdminUser = "admin"
)

// LokiHelmChart renders the loki HelmChart YAML for the given memory
// resources. Callers compose Resources from a profile (and optionally
// per-component overrides) via ResourcesForProfile or EffectiveResources.
func LokiHelmChart(res Resources) string {
	return fmt.Sprintf(`apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: loki
  namespace: %s
spec:
  repo: https://grafana.github.io/helm-charts
  chart: loki
  version: %s
  targetNamespace: monitoring
  createNamespace: true
  valuesContent: |-
    deploymentMode: SingleBinary
    loki:
      auth_enabled: false
      commonConfig:
        replication_factor: 1
      schemaConfig:
        configs:
          - from: "2024-01-01"
            store: tsdb
            object_store: filesystem
            schema: v13
            index:
              prefix: loki_index_
              period: 24h
      storage:
        type: filesystem
      limits_config:
        retention_period: 72h
    singleBinary:
      replicas: 1
      resources:
        requests:
          cpu: 50m
          memory: %s
        limits:
          memory: %s
      persistence:
        enabled: true
        size: 10Gi
    minio:
      enabled: false
    read:
      replicas: 0
    write:
      replicas: 0
    backend:
      replicas: 0
    chunksCache:
      enabled: false
    resultsCache:
      enabled: false
    monitoring:
      selfMonitoring:
        enabled: false
        grafanaAgent:
          installOperator: false
      lokiCanary:
        enabled: false
`, HelmChartNamespace, LokiChartVersion, res.LokiMemoryRequest, res.LokiMemoryLimit)
}

// PromtailHelmChart renders the promtail HelmChart that ships container logs
// to Loki. It is profile-independent today; promtail's footprint is small
// across all profiles.
func PromtailHelmChart() string {
	return fmt.Sprintf(`apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: promtail
  namespace: %s
spec:
  repo: https://grafana.github.io/helm-charts
  chart: promtail
  version: %s
  targetNamespace: monitoring
  createNamespace: true
  valuesContent: |-
    config:
      clients:
        - url: http://loki-gateway.monitoring.svc.cluster.local/loki/api/v1/push
      snippets:
        # The Kubernetes API audit log is a host file on the server node
        # (kube-apiserver audit-log-path); shipping it into Loki is what
        # makes per-operator attribution queryable in Grafana. The mount
        # resolves to an empty directory on agent nodes, where the scrape
        # simply finds no file.
        extraScrapeConfigs: |
          - job_name: kubernetes-audit
            static_configs:
              - targets:
                  - localhost
                labels:
                  job: kubernetes-audit
                  __path__: /var/log/k3s-server/audit.log
    extraVolumes:
      - name: k3s-server-logs
        hostPath:
          path: /var/lib/rancher/k3s/server/logs
          # DirectoryOrCreate: agent nodes have no server/logs directory,
          # and a missing hostPath source would fail promtail's volume
          # setup there — dropping every container log on the node, not
          # just the audit scrape.
          type: DirectoryOrCreate
    extraVolumeMounts:
      - name: k3s-server-logs
        mountPath: /var/log/k3s-server
        readOnly: true
    resources:
      requests:
        cpu: 25m
        memory: 32Mi
      limits:
        memory: 128Mi
`, HelmChartNamespace, PromtailChartVersion)
}

// KubePrometheusStackHelmChart renders the kube-prometheus-stack HelmChart
// for the given memory resources. Grafana carries a datasource spanning every
// tenant's logs and metrics, so it is deployed without a public ingress and is
// reached over the cluster API instead.
func KubePrometheusStackHelmChart(res Resources) string {
	return fmt.Sprintf(`apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: kube-prometheus-stack
  namespace: %s
spec:
  repo: https://prometheus-community.github.io/helm-charts
  chart: kube-prometheus-stack
  version: %s
  targetNamespace: monitoring
  createNamespace: true
  valuesContent: |-
    alertmanager:
      enabled: false
    prometheus:
      prometheusSpec:
        retention: 3d
        resources:
          requests:
            cpu: 100m
            memory: %s
          limits:
            memory: %s
        serviceMonitorSelectorNilUsesHelmValues: false
        podMonitorSelectorNilUsesHelmValues: false
    grafana:
      enabled: true
      # The admin password is generated once into the grafana-admin Secret
      # (provisioned by the installer and the PlatformConfig reconciler) and
      # read from there, so it is never a static default and stays stable
      # across upgrade and reconcile re-apply.
      admin:
        existingSecret: %s
        userKey: %s
        passwordKey: %s
      # Grafana is an admin tool with a datasource spanning every tenant's
      # logs and metrics, so it is not exposed on a public ingress. Reach it
      # over the cluster API (kubectl port-forward svc/kube-prometheus-stack-grafana).
      ingress:
        enabled: false
      resources:
        requests:
          cpu: 50m
          memory: 64Mi
        limits:
          memory: 128Mi
      additionalDataSources:
        - name: Loki
          type: loki
          access: proxy
          url: http://loki.monitoring.svc.cluster.local:3100
          jsonData:
            maxLines: 1000
    kube-state-metrics:
      # kube_pod_labels only carries name and namespace by default. The
      # usage-history queries join on the pod "app" and "app.kubernetes.io/
      # managed-by" labels to attribute container metrics to Kipper apps, so
      # those labels have to be allow-listed here or the join returns nothing.
      metricLabelsAllowlist:
        - pods=[app,app.kubernetes.io/managed-by]
      resources:
        requests:
          cpu: 10m
          memory: 32Mi
        limits:
          memory: 64Mi
    prometheus-node-exporter:
      resources:
        requests:
          cpu: 10m
          memory: 32Mi
        limits:
          memory: 64Mi
`, HelmChartNamespace, KubePrometheusStackVersion,
		res.PrometheusMemoryRequest, res.PrometheusMemoryLimit,
		GrafanaAdminSecretName, GrafanaAdminUserKey, GrafanaAdminPasswordKey)
}

// TraefikServiceMonitor scrapes Traefik's dedicated metrics Service (created
// by the Traefik chart when metrics.prometheus.service is enabled). It lives
// with the observability stack rather than the Traefik chart because the
// ServiceMonitor CRD only exists once kube-prometheus-stack is installed,
// and Traefik installs first. The endpoint port name keeps the selector from
// picking up Traefik's main service, which carries the same labels.
// ConsoleAPIServiceMonitor tells Prometheus to scrape the controller manager's
// own metrics, which is what makes a reconcile that fails every pass countable.
func ConsoleAPIServiceMonitor() string {
	return `apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: console-api
  namespace: monitoring
  labels:
    app.kubernetes.io/managed-by: kipper
spec:
  namespaceSelector:
    matchNames:
      - kipper-system
  selector:
    matchLabels:
      app: console-api
  endpoints:
    - port: metrics
      interval: 30s
`
}

// KipperControllerAlerts alerts on reconcilers that keep failing.
//
// The window is an hour, and that is the whole difficulty. controller-runtime
// retries a failed reconcile on an exponential backoff capped at 1000 seconds
// (client-go's DefaultTypedControllerRateLimiter), so a workload that has been
// failing for a while errors roughly three or four times an hour and nothing at
// all in most five-minute windows. A rate over five minutes would read zero for
// the very case these alerts exist to catch, and would never hold long enough
// to fire. Counting over an hour and requiring three errors distinguishes a
// workload stuck at the backoff ceiling from one transient conflict.
//
// The second alert asks a different question. A queue depth above zero means
// work is arriving, which a busy cluster does honestly; it says nothing about a
// reconcile that went in and never came out. longest_running_processor_seconds
// is the one that notices a pass wedged mid-flight, and depth cannot, because a
// wedged item has already left the queue.
func KipperControllerAlerts() string {
	return `apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: kipper-controllers
  namespace: monitoring
  labels:
    app.kubernetes.io/managed-by: kipper
    release: kube-prometheus-stack
spec:
  groups:
    - name: kipper-controllers
      rules:
        - alert: KipperReconcileFailing
          expr: sum by (controller) (increase(controller_runtime_reconcile_errors_total[1h])) >= 3
          for: 10m
          labels:
            severity: warning
          annotations:
            summary: "the {{ $labels.controller }} reconciler has recorded repeated errors in the last hour"
            description: >-
              Three or more reconcile errors landed within the last hour, counted
              across every workload this controller manages. That can be one
              workload failing over and over, or three failing once each — the
              counter carries no workload label, so telling those apart starts
              with the log. The threshold is deliberately low, because a workload
              that fails every pass leaves each step after the failing one
              unapplied and can sit that way indefinitely. Start with the controller's own errors in Loki under
              namespace="kipper-system", app="console-api". If the cause is an
              object the controller may not touch, the workload holding it
              carries a ChildrenAdopted condition set to False that names it. See
              the Observability guide, "When a reconcile keeps failing".
        - alert: KipperReconcileWedged
          expr: max by (controller) (workqueue_longest_running_processor_seconds) > 300
          for: 10m
          labels:
            severity: warning
          annotations:
            summary: "a {{ $labels.controller }} reconcile has been running for over five minutes"
            description: >-
              One pass went in and has not come out, so nothing else on that
              controller's queue is moving behind it. This is a wedge rather than
              a failure — a failing pass returns and retries — so look for a call
              with no timeout: a hanging API request, a registry read, or a
              remote host that stopped answering.
`
}

func TraefikServiceMonitor() string {
	return `apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: traefik
  namespace: monitoring
  labels:
    app.kubernetes.io/managed-by: kipper
spec:
  namespaceSelector:
    matchNames:
      - traefik
  selector:
    matchLabels:
      app.kubernetes.io/name: traefik
  endpoints:
    - port: metrics
      interval: 30s
`
}
