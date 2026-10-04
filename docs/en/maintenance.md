---
title: Upgrades & Maintenance
description: Upgrade Kipper, understand the component scope, and recover from failed upgrades.
---

# Upgrades & Maintenance

Use `kip upgrade` for routine platform updates. Review its scope and disruption warning below before running it. For initial setup, see [Installation](/en/installation).

## kip upgrade {#kip-upgrade}

Upgrades the cluster to the versions pinned in this kip build. Before making changes, it checks the cluster's data for compatibility and asks before applying repairs; see [Checking before an upgrade](#checking-before-an-upgrade). Then three things happen, in order:

1. **Kipper CRDs** are updated (so newer console features that need new CRD fields work).
2. **Console and console-api** are restarted to pull the latest images. console-api also gets the pod security settings this kip build ships (non-root, no privilege escalation, dropped capabilities, a read-only root filesystem), so a cluster installed a while ago ends up with the same posture as a fresh install. Anything you added to the Deployment by hand, such as an extra volume, is kept. Each component is then waited for: kip only moves on once the new pods are actually running, and stops with the reason if one of them cannot start. That matters because a rollout keeps the previous pod serving while the new one fails, so without the wait a broken upgrade looks exactly like a working one.
3. **Cluster system components** (Traefik, Longhorn, KEDA, Loki, Prometheus, Grafana, Velero, Zot, security middleware) are reconciled. Each chart is re-applied at the version pinned in kip, and helm-controller upgrades it in place.

On a cluster that uses a `*.kipper.run` name, the upgrade also records that name and the address the cluster registers with on the ClusterIdentity, which is where the console reads them from when it renews the registration. It keeps the address already recorded rather than replacing it, and tells you when the cluster answers somewhere else: a server that has genuinely moved needs a fresh registration, because the gateway ties a name to one address. A cluster that has never registered is left alone and told so, since only you know whether it should hold a kipper.run name.

Your apps and services are not directly touched, but step 3 can briefly disrupt running workloads if a chart upgrade rolls pods. For that reason kip prompts before running step 3 and refuses to proceed in non-interactive contexts without `--yes`.

### Clusters installed before operator login existed {#clusters-installed-before-operator-login-existed}

A cluster installed before Kipper configured the API server has no authenticator, so it rejects every login token while still accepting the admin certificate. `kip cluster ca status` reports it as an authentication config that names no issuer. The arguments that fix it live on the server rather than in the cluster, so only an upgrade, which reaches the host over SSH, can add them.

Every upgrade checks, and repairs a cluster that needs it:

```
  ...  The API server is missing the arguments this kip installs. Adding them and restarting k3s once; workloads keep running through it.
  ✔  API server arguments, and k3s restarted on them
  ...  Operator login against dex.shop.kipper.run
  ✔  Operator login configured. Run 'kip auth login', then 'kip auth kubeconfig'
```

k3s restarts once, which interrupts the control plane for a few seconds. containerd and your pods run through it, so apps keep serving. kip then waits up to two minutes for the API server to answer.

One component can notice: `kube-state-metrics` rebuilds its view of every object when the API server returns, and on a cluster still carrying the old 64 Mi limit that burst can OOM-kill it for a few minutes before it settles. The current limit is applied by the system-component step of a full `kip upgrade`, which runs after this one, so a cluster being repaired for the first time may see that blip once. `--skip-system` skips the resize entirely, and the next full upgrade applies it.

If the API server does not come back from a restart that changed the configuration, kip restores the file it replaced and restarts k3s on that. A copy of the previous file stays at `/etc/rancher/k3s/config.yaml.kipper-bak` either way.

A restart that changed no configuration is different, and kip restores nothing there. Later upgrades restart k3s to load a new audit policy, and the backup on disk may be months old and predate edits you have made since, so putting it back to recover from a failed restart would undo your work rather than kip's. The error names the file and leaves the decision with you.

Read the recovery result before retrying. It distinguishes a successful rollback, a rollback whose restart also failed, and an unreachable host whose state needs inspection. The previous configuration remains at `/etc/rancher/k3s/config.yaml.kipper-bak`.

For custom or partially configured `kube-apiserver-arg` blocks, Kipper prints the required additions and stops for manual review. If Dex is unreachable, it reports that operator authentication could not be configured and preserves certificate access.

Audit logging arrives with the same block, writing to `/var/lib/rancher/k3s/server/logs/audit.log` under the policy fresh installs use: metadata only, never request or response bodies, capped at 100 MB per file with 10 kept for 30 days.

A cluster already carrying these arguments keeps them, and the arguments themselves are not rewritten. It can still restart: an upgrade that changes the audit policy loads it by restarting k3s, and says so before it does.

```bash
kip upgrade --check            # report what the upgrade would fix, change nothing
kip upgrade                    # default, prompts before system components
kip upgrade --skip-system      # only steps 1 and 2 (Kipper console layer)
kip upgrade --yes              # all three steps, no component prompt (for automation)
kip upgrade --yes --repair-autoscaling  # also apply autoscaling fixes without asking
```

### Flags {#flags-1}

| Flag | Default | Description |
|---|---|---|
| `--skip-system` | `false` | Skip the cluster components (Traefik, Longhorn, KEDA, Velero, Zot, monitoring). CRDs, console, and the cluster's own trust material still move. Use this in production to avoid touching component versions. |
| `--yes` | `false` | Skip the confirmation prompt before upgrading cluster components. Required in non-interactive contexts (CI, scripts). Autoscaling fixes are confirmed separately, so a run without a terminal also needs `--repair-autoscaling` when there are fixes to apply |
| `--check` | `false` | Run the upgrade's data checks and report what it would fix, without changing anything. Needs no SSH key. Exits non-zero when something needs a decision. See [Checking before an upgrade](#checking-before-an-upgrade) |
| `--repair-autoscaling` | `false` | Apply the autoscaling fixes the upgrade lists without asking. Required in non-interactive contexts when there are fixes to apply |
| `--skip-autoscaling-check` | `false` | Upgrade although some apps' autoscaling settings need a decision, leaving those apps as they are |
| `--image-tag` | none | Install the console images built from this commit (a full 40-character sha) instead of the released ones. It is meant for testing a branch on a test cluster and needs `--skip-system`, because the component step would put authz back on its released image; see [Testing a branch on a test cluster](/en/contributing#testing-a-branch-on-a-test-cluster). The next `kip upgrade` without it returns the cluster to the released images |
| `--seed-credential-grants` | `false` | Grant each shared git credential the projects whose apps already reference it, without asking. Only acts on a cluster that predates credential allow-lists |
| `--ssh-key` | inherited from `~/.kip/config.yaml` | SSH private key for connecting to the cluster host. If unset, falls back to `KIP_SSH_KEY` env, then the saved `cluster.ssh_key`, then your ssh-agent. Needed for upgrades, including `--skip-system`, to reconcile the cluster's trust material. `--check` needs no SSH key |

### Checking before an upgrade {#checking-before-an-upgrade}

Upgrade `kip` first, then run the check before every upgrade:

```bash
kip upgrade --check
```

The check lists findings and proposed fixes without changing the cluster. It reads apps and Deployments through the Kubernetes API, so it needs no SSH key. It exits non-zero if a check fails or an app needs a decision; otherwise it exits zero.

The check covers app [autoscaling settings](/en/deploying-apps#autoscaling). Older apps can have invalid policies or stored counts outside their bounds:

```
  Checking this cluster's data against the rules of kip v0.24.0...

  !   The new rules refuse these apps' autoscaling settings. Once the new console-api
      is running, kip upgrade changes them as shown, and nothing running changes:
      - shop-prod/api: autoscaling is on and it stores replicas 1, outside its bounds of 2 to 5, so the stored count becomes the running count when the fix is written.
          replicas: 1 → 3 (the running count when the fix is written)
  ✗   These apps need a decision, and kip upgrade does not change them:
      - shop-prod/worker: autoscaling is off, but its Deployment is set to 4 while it stores 3. Any reconcile restores 3.
          Set the count you want with 'kip app scale worker --project shop --environment prod --replicas N', with N from 2 to 5.
```

#### What the upgrade does with the findings

`kip upgrade` runs the same check before its first change and prints the same list. With nothing to fix it carries on without asking. Otherwise it asks:

```
  Apply these fixes? [y/N]
```

Answering no stops the upgrade with nothing changed. `--repair-autoscaling` answers yes without asking, for automation. A run without a terminal that has fixes to apply stops unless that flag is given. An app that needs a decision stops the upgrade too, before anything changes, unless you pass `--skip-autoscaling-check`, which upgrades and leaves those apps as they are.

The fixes are written late in the upgrade, once the new console-api is running and no old console-api pod remains. Versions predating stops would restart an app that the repair marks as stopped. If kip cannot confirm that the new console-api is serving alone, it writes no fix, says why, and finishes the rest of the upgrade; running `kip upgrade` again applies them. Each app is read again before its fix is written. If the proposed fix no longer matches what you approved, the app is left alone and reported for another `kip upgrade --check`. For a stored-count repair, consent covers the Deployment's latest desired count as long as it remains within bounds. A failed write names the app, and the next run picks up from there.

#### What the repair changes

Repairs apply to apps with valid bounds. Each fix is a single App update, shown with its values before and after. Here, the Deployment count means its desired replicas, not the number of ready pods:

| The app | The fix |
|---|---|
| Stopped, with a stored count outside its bounds | Autoscaling on: the count moves into the bounds (a start uses the minimum anyway). Autoscaling off: the bounds widen to include the count it starts at |
| Deployment count and stored count both 0, with autoscaling off | A stop is recorded with the reason "scaled to 0 before Kipper *version*" and the count is set to the minimum. The app stays at 0, its route serves the stopped page, and listings show it as Stopped. `kip app start` starts it at the minimum |
| Autoscaling on, Deployment count within bounds, stored count outside them | The stored count becomes the Deployment count at write time |
| Autoscaling off, Deployment count matching the stored count, both outside bounds | The bounds widen to include the count, because moving the count into them would scale the app |

These repairs preserve the Deployment's desired count and leave autoscalers in place. The new controller can separately start autoscaled apps at zero, as explained below.

#### Apps that need a decision

The check names the command to run for each of these, and the upgrade changes none of them:

- The app's Deployment cannot be read, or is missing while the app is not stopped.
- The autoscaling block is invalid: a minimum above the maximum, a minimum or maximum below 1, a minimum above 1 without a maximum, autoscaling on without a maximum or without a target, or a negative target while autoscaling is on.
- Autoscaling is on, but the Deployment's nonzero desired count lies outside the bounds. Check whether a stale autoscaler or another writer is changing it.
- Autoscaling is off, but the Deployment's desired count differs from the stored one.
- No fix could satisfy the rules, for example a stopped app with autoscaling off and a stored count of 0.

#### Autoscaled apps at 0 pods start during the upgrade

An autoscaled app whose Deployment was set to 0 without a stop starts during the upgrade. The new console-api starts it at its minimum as soon as it runs, because the autoscaler cannot scale up from zero. The check and the prompt list these apps separately. To keep one at 0, stop it before upgrading:

```bash
kip app stop web --project shop --environment test
```

Stops exist since Kipper v0.23.0. On an older cluster the upgrade starts the app, so run `kip app stop` on it once the upgrade finishes.

#### Between a release and your upgrade

console-api runs a `:latest` image that is pulled whenever its pod starts. A console-api pod that restarts for any other reason after a release, such as a node reboot, runs the new release before `kip upgrade` has repaired anything or updated the CRDs. In that window the new controller reports invalid settings and starts autoscaled apps at 0. Until the CRDs are updated, the old schema drops the OOM marker, so memory tracking can filter out a stored memory increase intended for OOM recovery. Upgrade `kip` as soon as a release is out and run `kip upgrade --check`, so the findings are in front of you before that can happen.

### Upgrade scope {#what-an-upgrade-moves-and-what-it-does-not}

Check this table before upgrading to see which components are included and which need a separate procedure.

| Component | `kip upgrade` | How it moves otherwise |
|---|---|---|
| Kipper CRD schemas | Yes, or the upgrade stops | — |
| Cluster CA and API server trust anchor | Yes, over SSH, even with `--skip-system` | — |
| Recorded gateway identity (`*.kipper.run`) | Yes | — |
| console-api image and pod hardening | Yes | — |
| console image | Yes | — |
| kipper-authz image | Yes | — |
| console-api RBAC | Yes | — |
| Project operator roles (viewer, deployer, owner) | Yes | — |
| Shared git credential allow-lists, where never set | Once, on consent (prompt, or `--seed-credential-grants`), from the apps that reference them | `kip credentials allow` |
| App autoscaling settings the new rules refuse | On consent (prompt, or `--repair-autoscaling`), after the console-api rollout | `kip app autoscale`, `kip app scale` |
| Traefik, Longhorn, KEDA, Velero, Zot | Yes | — |
| Loki, Prometheus, Grafana | Yes, when enabled | — |
| Security-header middleware, build isolation | Yes | — |
| cert-manager DNS resolvers | Yes | — |
| **cert-manager version** | **No** | Re-run `kip install` |
| **Dex** (version and configuration) | **No** | See the warning below |
| **Console deployment manifest** | **No** | Image moves on upgrade; the manifest does not |
| **Initial admin ClusterRoleBinding** | **No, deliberately** | Managed as you add and remove admins |
| **k3s** | **No** | Re-run `kip install` |
| API server arguments and operator login | Yes, over SSH, even with `--skip-system` | — |
| **Host firewall, kernel sysctls** | **No** | Fresh install only |
| Running apps | No rollout for these platform changes | New readiness and shutdown settings apply on the next pod rollout |

The following components need additional explanation.

**Apps** adopt new [readiness and shutdown settings](/en/deploying-apps#health-checks-and-rollouts)
on their next pod rollout. These settings alone do not restart apps during an upgrade.
Run `kip app restart <app>` to apply them immediately. If a check is selected, new pods must
pass it before receiving traffic. Pods created before the upgrade lack the shutdown delay;
replacement pods use it on supported clusters.

Downgrading to a controller that removes these settings can trigger app rollouts. If the cluster
rejects the server-side dry run used to identify rollout changes, Kipper preserves the existing
platform settings and records an `AdoptionUnavailable` warning event on the App.

**The CRD schemas** move unless this kip is older than the cluster. Two things
stop that, and both refuse before anything is written, so nothing is ever
partially applied:

- The cluster has objects stored under an API version this kip does not declare.
  Applying would strand them.
- The schemas were last written by a newer kip. Every upgrade records which
  version wrote them, so an older CLI can tell it is about to replace a newer
  schema with an older one and prune whatever the newer one added. A cluster
  last upgraded before this recording existed carries no such marker, and is
  allowed through on the first check alone.

Either way the remedy is the same: upgrade kip, then run it again. A `kip` built
from source reports a version that cannot be ordered against a release, so the
second check announces that it was skipped rather than passing quietly.

**The cluster CA and the trust anchor** the API server verifies logins against are
reconciled over SSH on every upgrade, including with `--skip-system`, because they
are this cluster's own identity rather than a component version. With
`--skip-system` it is the only part that reaches the host, which is why `--ssh-key`
matters even for an upgrade you expected to be API-only. It is announced before it
runs.

**Dex.** Its configuration lives in a ConfigMap that `kip install` renders from
install-time values, and the console writes users into that same ConfigMap. So
re-running `kip install` on a cluster removes every user account created through
the console since. There is no supported way to move Dex on an existing cluster
today, and re-running the installer is not a workaround for it.

**The initial admin binding** is left alone on purpose. Its subject list changes
as admins are added and removed, so re-applying the install-time copy would
reset the cluster to a single admin and revoke everyone else.

::: warning Re-running `kip install` is not a general upgrade path
It is idempotent for some things and destructive for others. It replaces the
Velero credentials Secret in place, which is why it is the documented way to
rotate backup keys, and it moves k3s and cert-manager onto the versions this kip
pins. It also re-renders the Dex ConfigMap, losing console-created users, and
re-runs the rest of the install. Use it for the specific tasks named in these
docs, not as a way to catch up a cluster.
:::

**k3s** deserves its own note: re-running `kip install` moves the server onto the
k3s release this kip version pins, which briefly restarts the control plane. That
path refuses downgrades and upgrades at most one Kubernetes minor per run,
because the Kubernetes skew policy forbids skipping minors. A server further
behind stays untouched with a warning and needs the intermediate minor upgrades
first.

### Recovering from a failed chart upgrade {#recovering-from-a-failed-chart-upgrade}

If a system component upgrade fails (helm release ends in `failed` state), subsequent `kip upgrade` runs may hang on `helm uninstall --wait` while helm-controller tries to reset the release. The fix is to drop the stale helm release secrets and let helm-controller do a fresh install:

```bash
ssh root@<cluster-host>
kubectl -n kube-system delete job helm-install-<chart>
kubectl -n kube-system delete pods -l helmcharts.helm.cattle.io/chart=<chart> --force --grace-period=0
kubectl -n <chart-target-ns> delete secret -l owner=helm,name=<chart>
kubectl -n kube-system annotate helmchart <chart> kip.kipper/reapply="$(date +%s)" --overwrite
```

## Host maintenance and recovery

- [DNS repair](/en/cli-reference#kip-cluster-dns-repair) restores the configured host resolvers.
- [Host hardening](/en/security#host-hardening) applies firewall and service defaults.
- [Node repair](/en/alerts#what-causes-the-session-to-drop) updates storage-related host settings.
- [Domain changes](/en/domains#custom-console-domain) move the cluster’s serving identity.
- [Cluster removal](/en/cli-reference#kip-cluster-uninstall) removes the installation and cluster data.
