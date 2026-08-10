# Cluster Health Dashboard

The dashboard is the first screen you see after logging into the Kipper web console. It provides a real-time overview of your cluster's health, showing the status of every infrastructure component, node resource usage, and any recent stability issues.

## What the dashboard shows

### Component status

The dashboard monitors 13 infrastructure components that make up a Kipper cluster:

| Component     | What it does                                      |
|---------------|---------------------------------------------------|
| k3s           | Lightweight Kubernetes distribution                |
| Traefik       | Ingress controller and reverse proxy               |
| cert-manager  | Automatic TLS certificate management               |
| Longhorn      | Distributed block storage                          |
| KEDA          | Event-driven autoscaling                           |
| Loki          | Log aggregation                                    |
| Promtail      | Log collection agent                               |
| Prometheus    | Metrics collection and alerting                    |
| Grafana       | Metrics visualisation                              |
| Velero        | Cluster backup and restore                         |
| Dex           | Identity and authentication provider               |
| Console API   | Backend API for the web console                    |
| Console       | The web console frontend                           |

Each component shows a green or red indicator:

- **Green** means the component has at least one healthy replica running.
- **Red** means the component is not found or has zero available replicas.

### Node information

The nodes table shows every node in your cluster with:

- **Status:** whether the node is Ready or NotReady.
- **CPU usage:** current CPU consumption reported by metrics-server.
- **Memory usage:** current memory consumption reported by metrics-server.
- **Disk usage:** reported when available, otherwise shown as "n/a".

CPU and memory values require metrics-server to be installed (it is included in a standard Kipper installation). If metrics-server is unavailable, the values display as "n/a".

### OOM-killed pods

An amber warning banner appears if any pod was terminated due to running out of memory (OOMKilled) in the last 24 hours. Each entry shows the pod name, namespace, and the time the kill occurred.

If the kill is on a Kipper-managed app, OOM kills typically indicate that the app needs more memory than its resource limit allows. To resolve this:

1. Open the app's resource settings in the console or run `kip resources set`.
2. Increase the memory limit.
3. Monitor the dashboard to confirm the kills stop.

If the kill is on a pod in the `monitoring` namespace (Prometheus or Loki), the row links to the [Platform](/en/platform-resources) page. Kipper's auto-bump controller doubles the memory limit on system-component OOMs automatically, so you usually do not need to do anything. Open the Platform page to see the bump history, the current limit, and whether the component has hit its ceiling.

### Node resource pressure

Two cards show real-time memory and CPU utilisation for the cluster:

- **Utilisation bar:** colour-coded green (<70%), amber (70-85%), or red (>85%)
- **Sparkline chart:** a trend line showing the last hour of usage at a glance
- **Totals:** current usage vs allocatable capacity

When memory exceeds 80%, the resource controller generates a warning alert with the top consumers and any anomalies. At 90%+, alerts are marked critical.

If Prometheus is unreachable, the dashboard shows a "Metrics are unavailable" banner above these cards. That tells you the charts are stale or empty because monitoring is down, so an empty chart reads as an outage to fix rather than an idle cluster.

### Workload memory trends

A table showing every Kipper-managed workload with:

- **Current memory:** how much the workload is using right now
- **Sparkline:** a mini trend chart of the last hour. Blue means stable, amber means growing, red means anomaly
- **Change indicator:** percentage growth compared to ~10 minutes ago. Workloads that grew more than 30% are flagged as anomalies and highlighted in red

Anomalies are sorted to the top of the list, making it easy to spot a workload that is leaking memory or experiencing unexpected load growth before the node runs out of resources.

### Resource management mode

The dashboard displays the current resource management mode (**Auto** or **Expert**) in the header area. In auto mode, the resource controller runs in the background and adjusts CPU/memory for your apps based on actual usage. A summary of recent auto-mode changes is shown on the dashboard when available. See [Resource Management](/en/resource-management) for details on how the controller works and how to switch between modes.

## Auto-refresh

The dashboard refreshes automatically every 30 seconds. You can also click the **Refresh** button to fetch the latest data immediately.

## Troubleshooting

If a component shows red, check the following:

1. **Verify the component is deployed.** Run `kubectl get pods -n <namespace>` to see if the pod exists.
2. **Check pod logs.** Run `kubectl logs -n <namespace> <pod-name>` for error messages.
3. **Check events.** Run `kubectl get events -n <namespace> --sort-by=.lastTimestamp` for recent issues.
4. **Restart the component.** Run `kubectl rollout restart deployment/<name> -n <namespace>` if the pod is stuck.

If all components show red and you have just installed the cluster, wait a few minutes for everything to start. Initial provisioning can take 2-5 minutes depending on your server.
