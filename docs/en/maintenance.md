---
title: Upgrades & Maintenance
description: Upgrade Kipper, understand the component scope, and recover from failed upgrades.
---

# Upgrades & Maintenance

Use `kip upgrade` for routine platform updates. Review its scope and disruption warning below before running it. For initial setup, see [Installation](/en/installation).

## kip upgrade {#kip-upgrade}

Upgrades the cluster to the versions pinned in this kip build. Three things happen, in order:

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
kip upgrade                    # default, prompts before system components
kip upgrade --skip-system      # only steps 1 and 2 (Kipper console layer)
kip upgrade --yes              # all three steps, no prompt (for automation)
```

### Flags {#flags-1}

| Flag | Default | Description |
|---|---|---|
| `--skip-system` | `false` | Skip the cluster components (Traefik, Longhorn, KEDA, Velero, Zot, monitoring). CRDs, console, and the cluster's own trust material still move. Use this in production to avoid touching component versions. |
| `--yes` | `false` | Skip the confirmation prompt before upgrading cluster components. Required in non-interactive contexts (CI, scripts) |
| `--ssh-key` | inherited from `~/.kip/config.yaml` | SSH private key for connecting to the cluster host. If unset, falls back to `KIP_SSH_KEY` env, then the saved `cluster.ssh_key`, then your ssh-agent. Needed by every upgrade, including `--skip-system`, because the cluster's trust material is reconciled over SSH |

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

Four of those need more than a row.

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
