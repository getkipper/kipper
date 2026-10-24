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
3. **Cluster system components** (Traefik, Longhorn, KEDA, Loki, Prometheus, Grafana, Velero, Zot, security middleware) are reconciled. Each chart is re-applied at the version pinned in kip, and helm-controller upgrades it in place. The Zot step restarts k3s only when the server's registry CA certificate or `registries.yaml` changes, or when an earlier attempt leaves a pending marker. The marker ensures that a retry loads any changes left by an interrupted run. It is cleared after any required restart and node readiness check succeed.

On a cluster that uses a `*.kipper.run` name, the upgrade also records that name and the address the cluster registers with on the ClusterIdentity, which is where the console reads them from when it renews the registration. It keeps the address already recorded rather than replacing it, and tells you when the cluster answers somewhere else: a server that has genuinely moved needs a fresh registration, because the gateway ties a name to one address. A cluster that has never registered is left alone and told so, since only you know whether it should hold a kipper.run name.

On a single-node cluster whose pod network still runs VXLAN, a full upgrade first moves it to WireGuard, which `kip node add` needs. This is a one-time change. kip saves the k3s configuration, restarts k3s on the new backend, and waits up to five minutes each for the network and for the workloads that were ready before. If either check fails, it attempts to restore the previous configuration and restart k3s again. The restart leaves running containers alone; the Kubernetes API, kip commands, console actions and any app that calls the API are unavailable meanwhile (usually 10 to 30 seconds, longer on small or busy servers), and the whole change takes about a minute. If a check fails, kip waits up to five minutes per step and then attempts to restore the previous configuration. Review the migration prompt before using `--yes` to approve it in unattended upgrades. `--skip-system` skips migration but still recovers interrupted network changes. A server whose kernel cannot load the `wireguard` module keeps VXLAN with a warning, and the rest of the upgrade runs.

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
kip upgrade --k3s              # also upgrade k3s to the release pinned by this kip version
kip upgrade --yes              # all three steps, no component prompt (for automation)
kip upgrade --yes --repair-autoscaling  # also apply autoscaling fixes without asking
```

### Flags {#flags-1}

| Flag | Default | Description |
|---|---|---|
| `--skip-system` | `false` | Skip the cluster components (Traefik, Longhorn, KEDA, Velero, Zot, monitoring). CRDs, console, and the cluster's own trust material are still updated, and Traefik is restarted when its copy of the hop certificate changes. Use this in production to avoid touching component versions. |
| `--yes` | `false` | Confirm cluster-component upgrades and removal of app routes that collide with installed platform routes. Required for these steps in non-interactive contexts (CI, scripts). Autoscaling fixes are confirmed separately, so a run without a terminal also needs `--repair-autoscaling` when there are fixes to apply |
| `--k3s` | `false` | Also upgrade k3s to the release pinned by this kip version, before the CRDs and console: the server first, then each ready worker. Stops before upgrading k3s when the server is more than one Kubernetes minor behind the pin, a node runs a newer release, or a worker would cross the limit of three minor versions behind. With `--check`, reports the planned upgrade. See [k3s upgrades](#k3s-upgrades) |
| `--check` | `false` | Run the upgrade's data checks and report what it would fix, without changing anything. Needs no SSH key. Exits non-zero when something needs a decision. See [Checking before an upgrade](#checking-before-an-upgrade) |
| `--repair-autoscaling` | `false` | Apply the autoscaling fixes the upgrade lists without asking. Required in non-interactive contexts when there are fixes to apply |
| `--skip-autoscaling-check` | `false` | Upgrade although some apps' autoscaling settings need a decision, leaving those apps as they are |
| `--image-tag` | none | Test a commit's console, instance proxy and data mover images using its full 40-character SHA. Requires `--skip-system` to prevent the system component upgrade from restoring the released authz image. Apps using the instance proxy roll out when its image changes. Omit this flag on the next `kip upgrade` to restore released images and roll out those apps again. See [Testing a branch on a test cluster](/en/contributing#testing-a-branch-on-a-test-cluster) |
| `--seed-credential-grants` | `false` | Grant each shared git credential the projects whose apps already reference it, without asking. Only acts on a cluster that predates credential allow-lists |
| `--ssh-key` | inherited from `~/.kip/config.yaml` | SSH private key for connecting to the cluster host. If unset, falls back to `KIP_SSH_KEY` env, then the saved `cluster.ssh_key`, then your ssh-agent. Needed for upgrades, including `--skip-system`, to reconcile the cluster's trust material. `--check` needs no SSH key |

### Checking before an upgrade {#checking-before-an-upgrade}

Upgrade `kip` first, then run the check before every upgrade:

```bash
kip upgrade --check
```

The check lists findings and proposed fixes without changing the cluster. It reads apps, Deployments and Ingresses through the Kubernetes API, so it needs no SSH key. It exits non-zero if a check fails or an app needs a decision; otherwise it exits zero.

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

#### Route names

`kip upgrade --check` lists [route name conflicts](/en/deploying-apps#route-names) and explains which routes need attention:

- **A route conflicts with an installed Kipper component on the same port.** For example, app `system-console-api` in namespace `kipper` on port 8080 conflicts with the console API. The upgrade removes conflicting app routes managed by Kipper. Use a different app name to keep the app reachable. The check exits with a non-zero status, and `kip upgrade` requires confirmation. Use `--yes` to confirm without a prompt. Manually remove conflicting routes that Kipper does not manage.
- **A route uses a name reserved for a Kipper component that is absent or uses a different port.** The existing route stays available, but its request figures are unavailable. `kip ai` refuses to install a component while its route name is in use by another namespace, regardless of port. Rename or remove the conflicting route before installing it.
- **Apps or services have conflicting route names.** The upgrade keeps their existing routes. If their ports match, requests can reach the wrong app or service. Give each a distinct name. In the same namespace, a removed route cannot be recreated while the conflicting app or service remains.

The last two findings are advisory. After renaming conflicting apps or services, run `kip upgrade` to recheck the names so new request figures can appear. Figures recorded while names conflicted cannot be separated.

#### What the upgrade does with the findings

`kip upgrade` checks autoscaling settings before its first change and prints the proposed fixes. If there are autoscaling repairs to apply, it asks:

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

**k3s** deserves its own note: `kip upgrade --k3s` moves a cluster onto the k3s
release this kip version pins without re-running the install. See
[k3s upgrades](#k3s-upgrades). Re-running `kip install` also moves the server onto
that release, and it restarts k3s on every run so that it loads the configuration
kip wrote. For a server more than one Kubernetes minor behind, the install keeps
the installed version and warns that the intermediate minor upgrades are needed
first.

To check component versions after an upgrade, run [`kip platform versions`](/en/cli-reference#kip-platform-versions). It shows the kip version recorded on the App CRD, the console-api build recorded at startup, each node's Kubernetes version, and the pod template images of every Deployment, StatefulSet and DaemonSet in Kipper's system namespaces.

### k3s upgrades {#k3s-upgrades}

`kip upgrade --k3s` upgrades the cluster to the k3s release pinned by this kip
version, then continues with the rest of the upgrade. The k3s step preserves the
other settings created by `kip install`, including console users, the admin
password and the k3s configuration.

```bash
kip upgrade --check --k3s   # report the planned move
kip upgrade --k3s           # move the server, then each ready worker
```

After recovering any interrupted network change and before upgrading k3s, kip
checks the server's version and the reported versions of recorded workers. It
stops when the server is more than one Kubernetes minor behind the pin, because Kubernetes
upgrades the control plane one minor at a time, and when the server or a worker runs
a newer release, because kip prevents k3s downgrades. It also stops while a worker is still
joining. Workers follow the skew rules below.

The server moves first. kip runs the k3s install script for the pinned release and
restarts k3s, then waits up to five minutes for the server's node to report Ready on
that release. Restarting k3s pauses the Kubernetes API, usually for under a minute,
while running pods keep serving. A server whose node already reports the pinned
release keeps running, and one whose binary was replaced by an interrupted attempt is
restarted. The workers then follow as described below, and a failed step names the
command to run again.

Before applying the CRDs and console, kip waits up to two minutes for the API to
accept kip's credentials again. An API server that authenticates operators through
Dex can reject their tokens for a few seconds after a restart while it reconnects
to Dex and initializes authentication.

On a cluster with workers, `kip upgrade --k3s` and rerunning `kip install` check
every recorded worker's reported k3s version before upgrading k3s. Each command
stops if a worker runs a newer version than the server will run. Use a
kip release that installs at least that worker's version.

It also stops if the server upgrade would leave a worker more than three
Kubernetes minor versions behind when it was previously within that limit.
Three minor versions is the limit in the
[Kubernetes version skew policy](https://kubernetes.io/releases/version-skew-policy/#kubelet)
for kubelets at version 1.25 or later. Bring such a worker to the server's current
version first with `kip node add <host>`. Workers already beyond that limit are
handled by the worker upgrade that follows.

Once the server is on its new version, kip upgrades workers recorded as `ready` to
the server's k3s version, one at a time in name order. Before upgrading each
worker, it checks that the host is the recorded machine and that its installed
k3s matches either the version its Node reports or the target version from an
earlier attempt. It then reruns the k3s install script with the worker's saved
join settings. `k3s-agent` restarts while existing pods keep running, without a
drain. kip waits up to five minutes for each worker to report Ready on the target
version, including workers already on that version, and stops at the first
upgrade failure or readiness timeout. Running the same command again resumes the
upgrades and skips workers already reporting Ready on the server's version.
Workers recorded as joining, being removed or being wiped are left unchanged.
kip holds the same lock as `kip node add` and `kip node remove` while upgrading
workers, so it stops if another command holds that lock.

By default on Windows, and with `--via-server`, the install runs as a job on the
server. The machine running the CLI upgrades the workers after the job finishes.
`kip install --local` upgrades the server only.

### Downgrading console-api {#downgrading-console-api}

Traefik serves the hop certificate of a `*.kipper.run` cluster from its own copy, the Secret
`kipper-hop-cert` in the `traefik` namespace, and console-api keeps that copy up to date. An older
console-api, installed by an older kip with `--skip-system` or by using `--image-tag` with an older commit,
does not maintain the copy. If it rotates the hop key, Traefik keeps serving the retired key, and the
cluster stops routing through the kipper.run gateway once the old key's proof runs out. `kip doctor`
reports this as Traefik's copy holding a different key.

Once the downgrade has finished and only older console-api pods are running, remove the copy and restart
Traefik. Traefik then serves the hop certificate through its default TLSStore, which the older console-api
maintains. If you remove the copy earlier, a console-api that maintains it recreates it. Any `kip upgrade`
run by a kip that maintains the copy also recreates it, including one using `--image-tag` with an older commit.

A finished rollout can still leave a newer console-api pod terminating. The function below therefore waits
until every console-api pod belongs to the current revision, printing the pods it is waiting for, and then
removes the copy. It prints the current revision's image for reference and leaves the copy unchanged if
any read before deletion fails. At the end, it displays any recreated copy so you can check that deletion
persisted. Each rollout wait has a five-minute timeout. The wait for pods from earlier revisions to
exit has no timeout. Run it while no other console-api rollout is in progress, and rerun it if it stops partway:

```bash
remove_traefik_copy() (
  local rev hash pods
  kubectl -n kipper-system rollout status deployment console-api --timeout=300s || return 1
  if ! rev=$(kubectl -n kipper-system get deployment console-api \
      -o jsonpath='{.metadata.annotations.deployment\.kubernetes\.io/revision}') || [ -z "$rev" ]; then
    echo "STOP: could not read console-api's revision. The copy is unchanged."; return 1
  fi
  if ! hash=$(kubectl -n kipper-system get replicaset -l app=console-api \
      -o jsonpath="{range .items[?(@.metadata.annotations.deployment\.kubernetes\.io/revision==\"$rev\")]}{.metadata.labels.pod-template-hash}{' '}{end}"); then
    echo "STOP: could not read console-api's ReplicaSets. The copy is unchanged."; return 1
  fi
  hash=${hash% }
  if [ -z "$hash" ] || [ "$hash" != "${hash% *}" ]; then
    echo "STOP: could not find exactly one current console-api ReplicaSet. The copy is unchanged."; return 1
  fi
  if ! kubectl -n kipper-system get deployment console-api -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'; then
    echo "STOP: could not read console-api's image. The copy is unchanged."; return 1
  fi
  while :; do
    if ! pods=$(kubectl -n kipper-system get pods -l "app=console-api,pod-template-hash!=$hash" -o name); then
      echo "STOP: could not list console-api's pods. The copy is unchanged."; return 1
    fi
    [ -z "$pods" ] && break
    echo "waiting for: $pods"
    sleep 2
  done
  kubectl -n traefik delete secret kipper-hop-cert --ignore-not-found &&
  kubectl -n traefik rollout restart deployment traefik &&
  kubectl -n traefik rollout status deployment traefik --timeout=300s &&
  kubectl -n traefik get secret kipper-hop-cert --ignore-not-found
)
remove_traefik_copy
```

The last command prints nothing when the copy is gone. If it prints the Secret, a console-api or kip that
maintains the copy recreated it. Check the image above and any concurrent `kip upgrade`, then rerun the
function once the downgrade has finished.
Until a console-api or kip that maintains the copy creates it again, a rebooted Traefik serves the hop
certificate only once it has loaded the TLSStore, as it did before the copy existed.

Repeat these steps after every `kip upgrade` by a kip that maintains the copy while an older console-api stays
in place. The same steps recover a cluster that is already serving a retired key. An older `kip upgrade`
without `--skip-system` needs no extra step, because it reapplies a Traefik chart that does not read the
copy. Upgrading console-api again to a version that maintains the copy creates or repairs it.

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
- [Node reboot](/en/cli-reference#kip-node-reboot) backs up and reboots the server to apply kernel or service updates, then waits for healthy nodes and components and for previously ready apps to become ready again.
- [Worker rejoin](/en/cli-reference#kip-node-add): a worker that shows NotReady while `k3s-agent` logs `not authorized` can have missing or expired agent certificates. Run `kip node add <host>` again with the original join options; it rejoins the worker with a fresh token and keeps its name and data. If that fails, follow the cause it reports.
- [Domain changes](/en/domains#custom-console-domain) move the cluster’s serving identity.
- [Cluster removal](/en/cli-reference#kip-cluster-uninstall) removes the installation and cluster data.
