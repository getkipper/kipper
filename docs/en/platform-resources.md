# Platform Resources

Kipper's cluster runs a small set of system components alongside your apps: Prometheus and Grafana for metrics, Loki for logs, Longhorn for storage, Traefik for ingress, Dex for identity, Zot for the local registry, and the console plus its API. The platform resource layer keeps those components sized appropriately for the box they're running on, and reacts when something runs short of memory.

This page explains how that works and what knobs you have.

## Sizing profiles

At install time, `kip install` looks at the node's total RAM and picks one of five profiles. Each profile maps to a set of memory limits for the system components.

| Profile  | Node RAM | Prometheus | Loki   | What it's for |
|----------|----------|------------|--------|---------------|
| `nano`   | < 4 GB   | off        | off    | Demos, dev boxes. Monitoring disabled to give apps room to breathe. |
| `small`  | 4–8 GB   | 512 Mi     | 384 Mi | Side projects and small workloads. Monitoring runs but with tight limits. |
| `medium` | 8–16 GB  | 1 Gi       | 512 Mi | Real production for a small team. Sensible defaults across the board. |
| `large`  | 16–32 GB | 1 Gi       | 512 Mi | Same limits as medium, more headroom for apps. |
| `xlarge` | > 32 GB  | 2 Gi       | 1 Gi   | Mature production with many services. |

The total system overhead across all profiles stays well under 8 GB, even at the top. Kipper deliberately ships a small platform layer so the box you pay for goes to your apps, not to operators and dashboards.

## Auto-bump on OOM

If Prometheus or Loki gets killed for running out of memory, Kipper does not wait for you to notice. A controller watching pod events sees the OOMKilled signal, doubles the component's memory limit, and writes the new value to the `PlatformConfig` CR. The reconciler picks the change up, patches the underlying HelmChart, and helm-controller redeploys the pod with the new limit.

A few invariants:

- Each component has a ceiling. Prometheus tops out at 4 Gi, Loki at 2 Gi. If a bump would exceed the ceiling, it stops there and flags the component as `at ceiling` so you know automated help has run out.
- A 10-minute cooldown sits between consecutive bumps on the same component, so a still-failing rollout doesn't burn through the ceiling in seconds.
- The same OOMKilled event never triggers two bumps. Kipper records which OOM event it handled (the container's `FinishedAt` timestamp) so a routine pod status update doesn't look like a fresh OOM and double the limit again.
- The bump never lowers a manual override. If you set Prometheus to 6 Gi yourself and it OOMs, Kipper leaves your value alone and reports the ceiling instead.
- The auto-bump is recorded on the CR's status (`LastBumpAt`, `LastBumpFrom`, `LastBumpTo`, `LastBumpReason`), visible in the Platform section of the console.

## Manual resizing

You can set a memory limit yourself, either through the Platform page in the console or with `kip platform resize`. The override is stored on the `PlatformConfig` CR and the reconciler applies it to the HelmChart on the next pass.

If your override lowers the limit below the profile's default memory request, Kipper clamps the request down to match. Kubernetes rejects pods where `request > limit`, so this guard means a fat-fingered resize cannot break the rollout. A user lowering the limit implicitly accepts a lower request too.

## Console

Admins get a Platform link in the sidebar. The page shows the active profile, a card per system component with its current limit and recent bump history, and inline controls to change the limit or disable a component.

When the dashboard's "N OOM-killed pods" warning lists a pod in the `monitoring` namespace, the row is a deep-link to this page so you can see what just happened and react.

## `kip platform`

Same actions, command-line edition:

```bash
kip platform status                          # active profile + per-component state
kip platform resize prometheus --memory 2Gi  # set a manual memory override
kip platform disable loki                    # turn a component off
kip platform enable loki                     # turn it back on
kip platform restart prometheus              # rolling restart
kip platform profile show                    # current profile
kip platform profile set large               # change profile
```

Restart works for the cluster components too (`console`, `console-api`, `dex`, `traefik`).

## Reinstall and upgrade behavior

`kip install` and `kip upgrade` both treat the `PlatformConfig` CR as the source of truth. Re-running install on an existing cluster does not bring back components you disabled, and an upgrade does not downsize Prometheus or Loki to the profile default after you bumped them manually.

What that looks like in practice:

- If you ran `kip platform disable loki` and then re-run `kip install`, the install step for Loki prints "(disabled in PlatformConfig; skipping)" and the HelmChart stays gone.
- If you bumped Prometheus to 3 Gi and then run `kip upgrade`, the upgrade renders the HelmChart with your 3 Gi override, not the medium profile's 1 Gi default.
- `kip install` writes the `PlatformConfig` CR, so a fresh cluster always has one. If `kip upgrade` finds it missing, it stops and asks you to run `kip install` first. That keeps the upgrade from guessing a profile and downsizing Prometheus or Loki by accident.

## Running a central observability stack

If you already have Prometheus, Loki, and Grafana running somewhere centrally and you don't want the per-cluster ones, disable them and claim back roughly 1.5 GB on a `medium`-or-larger profile:

```bash
kip platform disable prometheus
kip platform disable loki
```

The console's Platform page has the same toggle. The HelmCharts are deleted; helm-controller uninstalls the releases; the next `kip upgrade` won't try to reinstall them as long as the override is in place.

Forwarding metrics and logs from this cluster to your central stack (Prometheus remote-write, Loki client) is a separate feature on the roadmap. For now the supported pattern is "scrape from outside, run thin here."

## Footprint, in context

Kipper deliberately ships a small platform layer. The total system overhead is roughly:

| Profile  | System total | What's left for apps on the min node |
|----------|--------------|---------------------------------------|
| `nano`   | ~1.8 GB      | ~2 GB on a 4 GB node                  |
| `small`  | ~3.2 GB      | ~5 GB on an 8 GB node                 |
| `medium` | ~4.5 GB      | ~11 GB on a 16 GB node                |
| `large`  | ~4.5 GB      | ~27 GB on a 32 GB node                |
| `xlarge` | ~5.5 GB      | 58+ GB on a 64 GB node                |

For comparison, enterprise Kubernetes distributions typically require three or more nodes with 16 GB each (48 GB+ total) just for the control plane. Kipper runs the whole thing on one box at the low end and stays under an 8 GB platform budget even at the top. The bargain is "no HA, simpler operations, small footprint". Fine for the audience Kipper exists for. Less fine for a regulated bank that needs five nines.

## How it's wired

For the curious:

- `PlatformConfig` is a cluster-scoped CR. There's exactly one, named `platform`. It carries the active profile and per-component overrides.
- `PlatformConfigReconciler` (in `console-api`) watches the CR. On change it patches the relevant HelmCharts' valuesContent and, for enable/disable, creates or deletes the chart entirely.
- `PodOOMReconciler` watches pods in the `monitoring` namespace. On OOMKilled it writes a memory bump to the CR.
- `kip install` picks the profile from `/proc/meminfo` at install time, with a small margin so a marketed 4 GB box reporting 3900 MB still lands on the `small` profile.
