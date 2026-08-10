# Resource Management

Kipper automatically manages CPU and memory for your apps so you do not have to think about Kubernetes resource requests and limits. It monitors actual usage and adjusts allocations to match. It scales up when apps need more and scales down when they are over-provisioned.

## Auto mode (default)

A background controller monitors resource usage via metrics-server every 60 seconds. When it detects sustained high or low usage, it adjusts CPU and memory requests and limits automatically.

### How it works

| Condition | Threshold | Action |
|---|---|---|
| High usage | Above 80% for 3 consecutive checks | Increase by 50% |
| Low usage | Below 20% for 3 consecutive checks | Halve (with minimums) |
| OOM kill | Immediate | Double memory (capped, see [OOM memory cap](#oom-memory-cap)) |
| Stuck pod | In `ContainerCreating` for 5+ minutes | Delete pod to trigger recreation |

The controller only acts when usage is **consistently** high or low. A single spike does not trigger a scale-up, and a brief idle period does not trigger a scale-down. That way, temporary load changes don't cause thrashing.

### Profile-based minimums

The controller never scales below the resource profile defaults. This prevents databases and heavy applications from being starved:

| Profile | Min CPU | Min memory |
|---|---|---|
| `lightweight` | 50m | 64 Mi |
| `standard` | 100m | 128 Mi |
| `compute-heavy` | 500m | 256 Mi |
| `memory-heavy` | 100m | 512 Mi |
| `database` | 500m | 1 Gi |
| `jvm` | 500m | 2 Gi |

These are the floors the auto controller scales down to. The deploy-time `jvm` profile itself is burstable (100m request, 1000m limit): the request stays low so pods schedule on small nodes, and the high limit lets cold-start JIT compilation use a full core for a few minutes without that capacity being permanently reserved. JVM apps spend most of their time idle and only need that headroom during startup.

Database services (PostgreSQL, MySQL, MongoDB, OpenSearch) automatically get the `database` profile.

### OOM memory cap

OOM doubling is capped at 50% of total node allocatable memory (minimum 8 Gi). On a 16 GB node, the cap is 8 Gi. If an OOM-killed pod is already at the cap, the controller creates a critical alert instead of doubling further.

All values are rounded to clean boundaries: CPU to the nearest 50m, memory to the nearest 64 Mi. If rounding would produce the same value as the current setting, the controller skips the update.

### Startup grace period

Pods younger than 5 minutes are excluded from CPU and memory calculations. Without this grace period, the controller would react to transient startup spikes. JVM applications, for example, often use 100% CPU during class loading and JIT compilation for several minutes before settling to idle. OOM detection is unaffected and works immediately regardless of pod age.

### Saturation override

The grace period protects against transient startup noise, but a pod that is **pinned at its CPU limit** is not transient. The cgroup is the bottleneck. When any pod sits at 95% or more of its CPU limit, the controller bypasses both the startup grace and the 3-tick hysteresis and bumps CPU.

This catches a specific failure mode: a JVM app whose CPU limit is too low to ever finish JIT compilation. Without the override, the pod would sit at 100% forever and the grace period would keep classifying it as "still starting up". With the override, the controller raises the limit, the JIT can finish, and the pod settles to idle.

Pods younger than 2 minutes don't count towards the override. A booting app legitimately pins its CPU limit for a minute or two, and reacting to that would roll the pod into another boot, endlessly. A pod still pinned past the warmup is genuinely bottlenecked and gets its bump well before the 5-minute grace expires.

Stateful services get extra caution. Restarting a database mid-operation can kill a running restore or bulk import, so a service only receives a saturation bump after staying pinned for 90 seconds of continuous observations, and never from a single hot reading.

The override only triggers an increase, never a decrease. The hysteresis still applies to scale-downs.

### Single-replica apps

For apps with a single replica, the controller only scales **up** and never scales down. Every resource change triggers a pod restart, and with one replica that means a brief outage. Scaling down is only safe with 2+ replicas, where Kubernetes performs a rolling update and at least one pod stays up.

The Scale tab in the web console shows a message explaining this when an app has one replica and auto mode is active.

## Autoscaling (HPA)

Autoscaling adjusts the **number of pods** based on CPU and memory utilisation. It works independently from the resource controller, which adjusts **CPU and memory per pod**. Together, they give you both vertical scaling (right-sized pods) and horizontal scaling (right number of pods).

### How the two controllers interact

| Concern | Who owns it | What it does |
|---|---|---|
| CPU and memory per pod | Resource controller (auto mode) | Monitors usage, adjusts requests and limits |
| Number of pods (replicas) | HPA (Kubernetes built-in) | Monitors utilisation %, scales between min and max |
| Deployment shape (image, env, volumes) | App reconciler | Syncs the Deployment to match the App CR |

When autoscaling is enabled, the App reconciler stops writing `spec.replicas` to the Deployment and lets the HPA own that field. When autoscaling is disabled, the App reconciler owns replicas again.

### When to use what

The resource controller and autoscaling solve different problems. They complement each other, but you don't always need both.

**Resource management only (auto mode, no autoscaling)**

Best for apps with predictable traffic where you don't know the right CPU and memory values yet. Kipper figures out the right size over time. A small internal tool, a staging environment, a service that handles a steady number of background jobs. You don't need multiple replicas, you just need the pod to be the right size.

**Autoscaling only (expert mode with HPA)**

Best when you know exactly how much CPU and memory each pod needs, but traffic varies. A public API that gets 10 requests per second at night and 500 during business hours. You've profiled the app and set the resources yourself. You just need Kubernetes to add and remove pods as load changes.

**Both together**

Best for production apps where traffic varies AND you want Kipper to handle the right-sizing automatically. The resource controller finds the right CPU and memory per pod over time. The HPA handles traffic spikes by adding pods quickly, without any restarts. When a traffic spike hits, the HPA responds in seconds by adding pods. The resource controller only adjusts resources after sustained changes over minutes.

Here's a typical sequence with both enabled:

1. App starts with standard profile defaults (250m CPU, 256Mi memory)
2. Resource controller watches usage over a few minutes and adjusts. Maybe the app actually needs 500m CPU. That triggers one rolling restart, but the HPA ensures 2+ pods, so there's no downtime.
3. A traffic spike hits. CPU goes above 70% across all pods.
4. The HPA adds pods within seconds. No restarts, just more pods handling requests.
5. The resource controller still watches per-pod usage. While the HPA has scaled out, the controller will not **decrease** per-pod resources (more pods doesn't justify shrinking each one), but it can still increase CPU or memory if pods are saturated. This handles the case where horizontal scaling alone is not enough, for example JVM apps stuck at a too-low per-pod CPU ceiling.
6. Traffic drops. The HPA removes the extra pods.
7. If baseline usage is still higher than before, the resource controller will eventually adjust. But only after sustained readings, not from a temporary spike.

**Common scenarios**

| Your situation | Recommended setup |
|---|---|
| Small internal tool, one user | Auto mode only, 1 replica |
| Staging environment, testing | Auto mode only, 1 replica |
| Production API, steady traffic | Auto mode, 2 replicas (no autoscaling) |
| Production API, variable traffic | Auto mode + autoscaling, min 2 / max 5 |
| JVM app you've already tuned | Expert mode + autoscaling |
| Database or cache | Auto mode only (databases should not be horizontally scaled) |
| Batch worker, periodic spikes | Auto mode + autoscaling based on CPU |

### Enabling autoscaling

From the **Scale** tab in the web console, toggle **Autoscaling** on. Set the minimum and maximum replicas and a CPU target percentage. Click **Save autoscaling**.

From the CLI or GitOps:

```yaml
apiVersion: kipper.run/v1alpha1
kind: App
metadata:
  name: api
  namespace: blog-prod
spec:
  image: registry.git.example.com/api:latest
  port: 8080
  autoscale:
    enabled: true
    minReplicas: 2
    maxReplicas: 5
    cpuTarget: 70
```

The HPA checks metrics every 15 seconds. When average CPU across all pods exceeds the target, it adds pods. When utilisation drops, it removes pods (down to `minReplicas`).

### Recommended settings

Set `minReplicas` to at least **2** when using auto mode. This gives two benefits:

1. The resource controller can safely scale resources **down**. With 2+ replicas, Kubernetes performs a rolling update so at least one pod stays available during the restart
2. Your app has basic high availability. If one pod crashes, the other continues serving traffic

A good starting point for most apps:

| Setting | Value |
|---|---|
| Min replicas | 2 |
| Max replicas | 5 |
| CPU target | 70% |
| Memory target | 0 (disabled) |

Memory-based autoscaling is usually less useful because most applications do not release memory when load drops. CPU-based scaling responds faster to actual load changes.

### What happens under the hood

1. You enable autoscaling on the App CR
2. The App reconciler creates an HPA targeting the app's Deployment
3. The HPA reads CPU metrics from metrics-server and adjusts `deployment.spec.replicas`
4. The resource controller independently adjusts CPU and memory requests based on per-pod usage
5. When the App reconciler runs (e.g. after an image update), it updates the Deployment template but preserves the replica count set by the HPA

### Disabling autoscaling

Toggle autoscaling off in the Scale tab and click **Save autoscaling**. The HPA is deleted and the App reconciler takes over replica management again, setting replicas to `app.Spec.Replicas` (defaults to 1).

### OOM recovery

When a pod is terminated by the kernel for exceeding its memory limit (OOMKilled), the controller doubles the memory immediately, without waiting for 3 consecutive checks. This handles cases where an app needs significantly more memory than its initial allocation, such as a Java application starting with 64 Mi but requiring 512 Mi+ for the JVM.

The controller detects OOM kills even when the pod is in a crash loop and has no metrics. It checks the pod's termination state directly from the Kubernetes API, not just from metrics data.

### Resource profiles

When an app has no resource requests configured, the controller applies defaults based on the app's resource profile label (`kipper.run/resource-profile`):

| Profile | CPU request | CPU limit | Memory | Use case |
|---|---|---|---|---|
| `lightweight` | 50m | 50m | 64 Mi | Static sites, proxies, lightweight APIs |
| `standard` | 100m | 100m | 128 Mi | Typical web applications (default) |
| `compute-heavy` | 500m | 500m | 256 Mi | Image processing, data transformation |
| `memory-heavy` | 100m | 100m | 512 Mi | Caching layers, in-memory databases, ML inference |
| `database` | 250m | 250m | 256 Mi | PostgreSQL, MySQL, MongoDB, OpenSearch |
| `jvm` | 100m | 1000m | 2 Gi | Java/JVM applications, Spring Boot, heavy runtimes |

The `jvm` profile is the only burstable profile by default. Most workloads run with request equal to limit (Guaranteed QoS) so they get exactly what they ask for. JVMs are different: they spike during startup and idle the rest of the time, so the request stays low (so the pod schedules) but the limit is high (so JIT can finish). On the same node, you can run six JVM apps reserving 600m total but each one able to burst to a full core during cold start.

If no profile label is set, `standard` is used. Database services automatically get the `database` profile.

### Custom resources

For workloads that don't fit any profile (like a Java application with `-Xms 4G` or a data pipeline needing 8 Gi), you can set explicit CPU and memory values at deploy time.

**From the CLI:**

```bash
kip app deploy --name exchange-service --image registry.git.example.com/exchange:latest \
  --port 8080 --memory 4Gi --cpu 1
```

The CLI's `--memory` and `--cpu` flags set request and limit to the same value (Guaranteed QoS). If you need burstable CPU (a different request and limit), set them in the web console or by editing the App CR directly.

**From the web console:**

Select **Custom...** from the resource profile dropdown when deploying an app. Two fields appear for memory and CPU. Use Kubernetes resource notation: `256Mi`, `1Gi`, `4Gi` for memory; `250m`, `500m`, `1`, `2` for CPU.

For an existing app, open the Settings tab and click **Advanced (request & limit)** in the resource limits panel. Four fields appear: CPU request, CPU limit, memory request, memory limit. Set request lower than limit for burstable workloads. The form opens in advanced mode automatically when an app already has different request and limit values.

Custom values override the profile defaults. The auto controller still adjusts from there based on actual usage. Your values are the starting point, not a ceiling.

### Resource log

Every change the controller makes is logged and visible under **Settings** in the web console. The log shows:

- **Time:** when the change happened
- **App and namespace:** which workload was adjusted
- **Action:** what changed (increased memory, decreased CPU, applied defaults)
- **From / To:** old and new values
- **Reason:** why the change was made (usage at 92%, OOM kill detected)

The system retains the most recent 50 log entries.

## Expert mode

Switch to expert mode when you want full control over resource allocation. The auto controller stops making changes, and all CPU and memory values are set manually through the Resources tab in the app detail panel.

Toggle between modes in **Settings** in the web console (only admins can change the mode), or from the CLI:

```bash
kip platform tuning show     # print the active mode
kip platform tuning expert   # hands off — no automatic resource changes
kip platform tuning auto     # default — resources adjust automatically
```

The console uses the same setting through the API:

```
PUT /api/v1/settings/mode
{"mode": "auto"}    // or "expert"
```

Bulk data loads don't need a mode switch: `kip service import` and `kip service export` pause tuning for the affected service automatically while they run.

In expert mode, you can still view the resource log to see what the controller changed before you switched.

## Alerts

Every action the controller takes generates an alert visible in the console bell icon:

- **Critical** (red): OOM kills, emergency memory doubling
- **Warning** (yellow): resource increases, stuck pod recovery
- **Info** (green): scale-downs, default profile application

See [Alerts](/en/alerts) for details on the alerting system and Slack integration.

## Slack notifications

Resource changes can be forwarded to Slack. See [Configuration](/en/configuration#slack-notifications) for setup.

## What Kipper manages

The auto controller manages resources for Kipper workloads defined as Custom Resources (`kipper.run/v1alpha1`):

- **Apps:** web apps, APIs, frontends
- **Services:** databases, caches, message queues
- **Functions:** serverless workloads (resources set at creation, not auto-tuned while idle)
- **Jobs:** scheduled and one-off batch tasks

It does not manage system components (Traefik, cert-manager, Longhorn) or the KEDA autoscaler itself.

## Project quotas

Project quotas are optional. A new project starts without a tier: its apps simply use whatever the cluster has free, and only the cluster's own capacity limits apply. That is the right mode for most single-team clusters, where the platform should just work without capacity planning.

Assign a **tier** when you want a ceiling on how much CPU and memory a project's workloads can claim in total, for example on a cluster shared by several teams, where one runaway project must never starve the rest. The ceiling applies per environment namespace, and each environment can override it individually.

### Tiers

A tier is a capacity label on the project. It sets the default quota for each of the project's environment namespaces, and it caps how many environments the project can have:

| Tier | CPU requests | CPU limits | Memory requests | Memory limits | Environments |
|---|---|---|---|---|---|
| *(no tier)* | — | — | — | — | up to 6 |
| `small` | 2 | 6 | 6 Gi | 12 Gi | up to 4 |
| `medium` | 4 | 12 | 12 Gi | 24 Gi | up to 6 |
| `large` | 8 | 24 | 24 Gi | 48 Gi | up to 10 |

Limits are roomier than requests. Requests are what the scheduler reserves; limits only cap bursts. A tier must fit a workload's whole lifecycle, so the ceilings leave room for a rolling update (old and new pods run at once) plus a Git build pod on top of the steady state.

The quota applies per environment namespace. A `small` project with `test` and `prod` environments gets two independent 2-CPU quotas, one per namespace, rather than one shared pool. The project's total granted capacity is therefore the per-environment quota times the number of environments, which is why the tier also caps the environment count. A `small` project can grant at most 4 × 2 = 8 CPU requests across its four environments.

```yaml
apiVersion: kipper.run/v1alpha1
kind: Project
metadata:
  name: shop
spec:
  displayName: "Webshop"
  tier: medium
  environments:
    - name: test
    - name: prod
```

### The quota tab in the console

Open a project's settings with the gear icon on its card in the **Projects** screen. The **Quota** tab shows the project's tier and, per environment, the four caps with live usage bars. An environment running with an explicit override carries an **Override** badge; one that has hit a cap shows **Over quota**.

An environment can also show neither state. Whether a namespace is over its cap is a comparison
against live usage, and that comparison does not always run: a namespace whose ResourceQuota has not
published usage yet has nothing to compare, and a read that fails leaves the question unanswered.
The API reports that as `over_quota: null` rather than `false`, and the console shows no badge, which
is the difference between "measured and within the cap" and "not measured". A quota change that
commits while usage cannot be read still succeeds and still reports the new caps; the usage fills in
on the next read.

Cluster admins can change the tier from the dropdown and edit or reset per-environment overrides with the pencil and reset buttons. Raising a quota hands out cluster capacity, so this stays with admins; project owners and members see the panel read-only. When a new cap would land below what an environment currently uses, the console shows exactly which values are affected and asks for confirmation before applying.

### Per-environment overrides

When one environment genuinely needs more than its tier, set an explicit quota on that environment. The override replaces the tier default for that namespace only:

```yaml
spec:
  tier: small
  environments:
    - name: test
    - name: prod
      quota:
        cpuRequest: "6"
        cpuLimit: "12"
        memoryRequest: "12Gi"
        memoryLimit: "24Gi"
```

Values use Kubernetes quantity notation, the same as app resources: `500m`, `2`, `4Gi`.

### The environment limit

Every project has a cap on how many environments it can have. Without a tier the cap is 6, which covers a dev/test/acc/prod pipeline plus a hotfix and a preview environment. A tierless environment has no quota, so the cap is what keeps a scripted loop (say, an environment per feature branch) from filling the cluster with unbounded namespaces before anyone notices.

For tiered projects the tier sets the cap (4 / 6 / 10). Each environment gets its own full tier-sized quota there, so adding environments adds granted capacity, and the cap stops a project owner multiplying the project's footprint past its tier without a cluster admin. Project owners create environments up to the limit themselves. At the limit the console stops offering **Add environment** and shows the count, and the API rejects an over-limit create.

When a project genuinely needs more environments, a cluster admin sets `maxEnvironments` on the project, which overrides the default for that project only. Make sure the cluster has the hardware to back the extra environments first, especially for tierless projects where nothing else caps their usage. Assigning a tier is the answer to a different question: it adds managed capacity envelopes, and picking `small` also lowers the environment cap to 4. Deleting an environment frees a slot straight away.

```yaml
spec:
  tier: small
  maxEnvironments: 6   # admin override; small would otherwise allow 4
  environments:
    - name: test
    - name: acc
    - name: prod
```

Lowering a tier or `maxEnvironments` below the environments a project already has does not delete anything. The existing environments keep running and keep their quota, new ones are blocked, and the project carries an `EnvironmentLimitExceeded` status condition until it fits again. The admin quota change asks for confirmation before applying that state, the same way a below-usage quota change does.

### What happens when the quota is reached

Running pods keep running. Kubernetes rejects the *next* pod that would push the namespace over the ceiling, so a deploy, build, or migration fails at admission with a quota error. Scale down something else, raise the tier, or set an environment override to make room.

The [auto resource controller](#auto-mode-default) respects the ceiling too. When an app needs more memory or CPU than the quota has room for (for example an OOM-killed container that wants its memory doubled), the controller leaves the resources unchanged and raises a critical alert naming the quota instead. Committing the increase would only produce replacement pods that admission rejects.

A manual resource change from the console or CLI runs the same projection before it saves. If raising an app or service beyond what the namespace quota can admit, the request is rejected up front with the exhausted dimension named, rather than saving a value that would stall the rollout at admission.

### Existing namespaces

When a quota first arrives on a namespace that already runs more than its tier default (for example after upgrading a cluster that predates quotas), Kipper measures current usage and records a raised quota as an explicit override on the environment, with 25% headroom for rolling updates. Nothing gets evicted and the next deploy still goes through. The override is visible in the Project CR, so you can lower it deliberately once you know what the environment really needs.

### Container defaults

Alongside the quota, each tiered or overridden environment gets a LimitRange with small per-container defaults: requests of 25m CPU and 32 Mi memory, limits of 100m CPU and 128 Mi memory. Tierless environments get neither the quota nor the LimitRange. Pods that declare no resources (for example something applied manually with kubectl) are filled in at admission instead of being rejected. Kipper's own workloads always declare explicit resources, so these defaults exist as a safety net. A container that inherits the 128 Mi default limit and needs more will be OOM-killed, which is the signal to give it real resource values.

Project quotas cap CPU and memory only. Storage is not part of the quota; PVC sizes are set per volume and per service.

## GitOps

Kipper resources are defined as Custom Resource Definitions (CRDs) under `kipper.run/v1alpha1`. This means you can manage your entire cluster declaratively with tools like ArgoCD or Flux:

```yaml
apiVersion: kipper.run/v1alpha1
kind: App
metadata:
  name: api
  namespace: blog-test
spec:
  image: registry.git.example.com/api:v2.1.0
  port: 8080
  replicas: 2
  resources:
    profile: jvm
    memoryRequest: "4Gi"
    memoryLimit: "4Gi"
    cpuRequest: "100m"
    cpuLimit: "1500m"
  env:
    LOG_LEVEL: "info"
  route:
    host: api.example.com
```

Apply with `kubectl apply -f app.yaml` or commit to a Git repo and let your GitOps tool sync it. Kipper's reconcilers ensure the underlying Kubernetes resources (Deployment, Service, Ingress, Secrets) match the CR spec.

Available CRDs: `App`, `Service`, `Function`, `Project`, `Job`, `Volume`.

For a more user-friendly approach, use the `kipper.yaml` manifest format with `kip apply`. See the full [GitOps](/en/gitops) guide for details, including ArgoCD and Flux integration examples.
