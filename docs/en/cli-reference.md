---
title: CLI Reference
description: Commands for managing Kipper clusters, workloads, users, and configuration.
---

# CLI Reference

Use this reference to look up commands and flags. For workflows, start with the guides in the sidebar. Run `kip <command> --help` for the options available in your installed version.

- [Installation reference](/en/installation) covers `kip install`, server requirements, and initial setup.
- [Upgrades & Maintenance](/en/maintenance) covers `kip upgrade`, its flags, `kip upgrade --check` and upgrade recovery.

## kip cluster {#kip-cluster}

Manage cluster configurations on your local machine.

```bash
kip cluster export > file.kip        # export connection details for sharing
kip cluster add file.kip              # import a cluster
kip cluster add file.kip --set-current # import and switch to it
kip cluster list                      # list all clusters
kip cluster use <name>                # switch active cluster
kip cluster remove <name>             # remove local config
kip cluster uninstall <name>          # wipe the cluster off the remote host
kip cluster domain <domain>           # move to a custom domain (no-lockout cutover)
kip cluster domain <domain> --ack-sso-callbacks # confirm SSO callback URLs are updated
kip cluster domain --sync             # finish an interrupted domain change
kip cluster domain --rollback         # return to the previous domain
kip cluster domain --repair           # rewrite local config from the cluster
kip cluster hosts                     # show the hostnames kip uses for a cluster
kip cluster hosts --dex dex.example.com # correct one without contacting the cluster
kip cluster dns repair                # restore the curated DNS resolvers and restart CoreDNS
kip platform restart <component>      # restart a platform or cluster component
```

### kip cluster uninstall {#kip-cluster-uninstall}

Wipes Kipper from a remote Linux server. Runs k3s's own uninstall script over SSH, sweeps the data directories Kipper writes outside k3s (Longhorn volumes, Zot blobs, AI bundle data), and removes the cluster from your local kip config. Recorded workers are wiped over SSH first. Each wipe checks that the host's machine ID matches its recorded ID, protecting machines at reused addresses. Workers are selected from the cluster's node records, or the local copy if those records cannot be read. Workers found only in the cluster's records require separate confirmation and are skipped with `--yes`. Workers whose cleanup is unconfirmed are listed with their recorded machine IDs. Check that `/etc/machine-id` on each host matches before running `/usr/local/bin/k3s-agent-uninstall.sh` there.

```bash
kip cluster uninstall storefront                  # interactive (prompts for cluster name)
kip cluster uninstall storefront --yes            # skip the confirmation prompt
kip cluster uninstall storefront --keep-local-config  # wipe host, keep local entry
kip cluster uninstall storefront --ssh-key ~/.ssh/kipper_ed25519
```

The command prompts you to type the cluster name to confirm, so you cannot tear down a cluster by reflex. Pass `--yes` only for automation.

A cluster on a free `*.kipper.run` subdomain also hands that name back to the gateway. The name is released after the host is wiped, using a credential read from the cluster beforehand: the gateway will only release a name to whoever holds its token, and that token lives on the cluster the wipe destroys. The name is free again straight away, so a rebuild of the same server can claim it back immediately. That also means links you published under it can be claimed by someone else, so move them before you uninstall if they still matter.

If the cluster cannot be reached, kip falls back to a copy of that credential it keeps in `~/.kip/config.yaml`, recorded on an earlier command. When neither is available the command says so and asks whether to wipe anyway. Answering no is the safe choice: wiping leaves the name registered with nothing able to release it, and installing on that host again cannot serve on it. The gateway stops routing such a name about a week after the cluster's last heartbeat, the registration lapses after 30 days without contact, and the name comes free 90 days after that, so waiting it out means waiting four months. `--yes` skips this question as well as the typed-name one, so scripted teardown consents to stranding a name it could not release.

If the server itself cannot be reached, kip offers to hand the name back without it, provided a credential for that name is recorded locally. Say yes only when the server is really gone: a cluster that is merely unreachable is still serving, and releasing its name takes it off the air. `--yes` never takes this offer, because a script cannot tell those two apart.

When the host is wiped but the gateway will not take the name back, the cluster stays in `~/.kip/config.yaml` and the command tells you to run it again. That entry is deliberate, because it holds the only credential that can still release the name, and it is why `kip cluster list` can show a server you have already wiped. The re-run goes straight to the gateway: it does not touch the host, so it works just as well on a server you have since destroyed.

```bash
kip cluster uninstall storefront
#   storefront was already wiped. Releasing its gateway name.
#
#   ✔  Gateway name released
#   ✔  Local config entry for storefront removed
```

This is destructive. All cluster state and persistent volume data on the host is removed. The command does **not** revert host firewall rules or OS hardening (rpcbind disabled, etc.), because those are general OS security improvements unrelated to k3s.

Use `--keep-local-config` when you plan to reinstall immediately, so the existing cluster name and kubeconfig path stay in `~/.kip/config.yaml` for the new install to refresh.

### kip platform restart {#kip-platform-restart}

Triggers a rolling restart of a platform or cluster component, for when it has stale configuration or needs to pick up changes.

```bash
kip platform restart dex           # restart identity provider
kip platform restart console       # restart web console
kip platform restart console-api   # restart console API
kip platform restart traefik       # restart ingress controller
```

### kip cluster env {#kip-cluster-env}

Sets environment variables on a cluster component and restarts it to pick up the changes.

```bash
kip cluster env console-api LOG_LEVEL=debug
kip cluster env console-api LOG_LEVEL=debug FEATURE_X=enabled
```

See [Team Access](/en/team-access) for full documentation.

## kip tunnel {#kip-tunnel}

Opens a secure tunnel from your machine to a service running in the cluster. Use this to connect desktop database clients (DBeaver, TablePlus, pgAdmin) to databases that are not exposed to the internet.

```bash
kip tunnel mydb                        # PostgreSQL on localhost:5432
kip tunnel cache                       # Redis on localhost:6379
kip tunnel mydb --local-port 15432     # custom local port
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--local-port` | No | Same as service port | Local port to listen on |
| `--port` | No | Auto-detected from the pod | Remote container port |
| `--project` | No | Saved project, else every project | Project name |
| `--environment` | No | Saved environment, but only when using the saved project | Target environment |
| `--kind` | No | Any kind | Restrict to `app`, `function`, or `service` |

The tunnel forwards to a pod that is Ready. When a workload is running but no
replica is ready, `kip tunnel` says so instead of forwarding, because a tunnel
to a pod that cannot serve produces connection errors that look like a broken
application.

See [Naming one workload](/en/cli-reference#naming-one-workload) below, and
[Team Access](/en/team-access) for full documentation.

## kip exec {#kip-exec}

Opens an interactive shell or runs a command inside a running container.

```bash
kip exec myapp                         # interactive shell
kip exec myapp -- cat /app/config.yaml # run a single command
kip exec mydb -- psql -U kipper app   # SQL session in a database pod
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--project` | No | Saved project, else every project | Project name |
| `--environment` | No | Saved environment, but only when using the saved project | Target environment |
| `--kind` | No | Any kind | Restrict to `app`, `function`, or `service` |

Unlike `kip tunnel`, `kip exec` will enter a pod that is running but not ready,
since a container failing its readiness probe is usually the one you want a
shell in.

See [Naming one workload](/en/cli-reference#naming-one-workload) below, and
[Team Access](/en/team-access) for full documentation.

## Naming one workload {#naming-one-workload}

`kip exec` and `kip tunnel` both take a workload name, and that name has to
identify exactly one workload. Where it matches more than one, they list the
matches and stop rather than pick one for you.

A project is authoritative when you give one, either with `--project` and
`--environment` or through `kip project use`. The search is confined to it, and
a project that holds no workload of that name is an error rather than a reason
to look in someone else's project. Without a project, the search covers the
whole cluster.

Two things commonly match more than once. The same app name in several
environments is the ordinary case, since each environment is its own namespace:

```bash
$ kip exec backend
Error: "backend" matches more than one workload:
  app/blog-prod
  app/blog-test
Name the one you mean with --project, plus --environment if the project has environments.
```

An app and a service can also share a name inside one namespace, which naming
the project does not resolve. Use `--kind` for that:

```bash
$ kip exec backend --project blog --environment prod
Error: "backend" matches more than one workload in blog-prod:
  app/blog-prod
  service/blog-prod
Name the one you mean with --kind app or --kind service.

$ kip exec backend --project blog --environment prod --kind service
  Connecting to blog-prod/backend-0...
```

## kip status {#kip-status}

Shows cluster health, node status, and component availability.

```bash
kip status
```

A component with no ready replicas (or no available replicas for a Deployment) is marked `…` and listed as starting when its pods appear to be starting up. Assigned nodes must be Ready. Pending pods can qualify before scheduling or container status is reported. Finished and deleting pods are ignored. If startup persists, check pod events, init containers, and readiness probes. `✗` means the health check found neither a healthy component nor evidence of startup; possible causes include missing pods, crash loops, image pull failures, scheduling failures, nodes that are not Ready, and lookup errors.

It also audits the DNS resolvers your cluster forwards external queries to. Those live in a file on the server, so `kip status` makes a best-effort SSH connection as the cluster's SSH user (reusing the key from install, then your SSH agent or `~/.ssh/id_ed25519`) to read it. Three things are checked: the file is still a safe set (IPv4 entries only, at most three), the entries still match the resolvers the cluster was configured with (`dns_resolvers` in `~/.kip/config.yaml`, or the default public set), and each resolver actually accepts connections from the server. Each problem gets its own warning, and drift or an unsafe hand-edit is fixed with `kip cluster dns repair`. If the server can't be reached over SSH, the section reports that the check could not run, and the rest of the status still prints.

## kip doctor {#kip-doctor}

```bash
kip doctor
```

Diagnoses a cluster that installed but does not work and provides a way to report failed checks. It first reads the cluster over SSH as the cluster's SSH user, reusing the key from install. If SSH is unavailable, it reads through the Kubernetes API. Using SSH lets it diagnose problems even when the API rejects your login. This can happen after a restart when Traefik serves its built-in fallback certificate: the API server can no longer load Dex's discovery document, so commands using your API login fail, including `kip status`.

The checks are grouped by where a problem could occur:

- **Host:** the server answers over SSH, k3s is running, and how long ago the server booted.
- **Kubernetes:** the API server answers, it accepts your login, the nodes are Ready, and Traefik, CoreDNS, cert-manager, Dex and the Console API are running.
- **Gateway hop** (kipper.run clusters only): the hop certificate is in place, the default TLSStore points Traefik at it, port 443 actually serves it, the gateway heartbeat renews the name, and the cluster registers the address kip has on record.
- **kipper.run gateway** (kipper.run clusters only): the gateway is up and routes the cluster's name to it.

Checks that cannot run are marked as not checked. For example, reading the hop certificate requires SSH access. When a check fails, `kip doctor` prints a link to a prefilled GitHub issue form and a report to paste into it, as a failed `kip install` does. The link contains only check names and statuses, the kip version, your machine's OS and architecture, the server's distribution and version, the sizing profile and the names of the flags you set. The report replaces recognised addresses, hostnames and credentials with placeholders. The command exits with status 1 when a check fails.

| Flag | Meaning |
|---|---|
| `--ssh-key` | Private key for the server, if it is not the one recorded at install. |

## kip cluster dns repair {#kip-cluster-dns-repair}

Restores the curated DNS resolver file on the server and restarts CoreDNS to pick it up. This is the scoped fix for the resolver drift `kip status` warns about; nothing else about the installation is touched.

```bash
kip cluster dns repair
```

```
  ✔  Restored resolvers on 203.0.113.10: 1.1.1.1, 8.8.8.8, 9.9.9.9
  ✔  CoreDNS restarted to pick them up
```

The resolvers come from `dns_resolvers` in `~/.kip/config.yaml` (set via `--dns-resolver` at install time), or the default public set when none are configured.

## kip node add {#kip-node-add}

A cluster starts with one server. `kip node add` joins workers that add CPU and memory for apps, functions, jobs and services. The server remains the only control plane and ingress node, so the cluster still depends on it for availability.

```bash
kip node add <host> [--ssh-key <path>] [--private-network <cidr> | --public] [--firewall | --firewall=false] [--yes]
```

The command prepares the host (kernel modules, kubelet hardening, Longhorn's storage packages, host hardening and the firewall) and joins it as a k3s worker on the server's k3s version.

- **Networking:** kip uses a shared private network when available, or public addresses otherwise. The first join sets the cluster network mode. Applying new network settings restarts k3s on the server; `--yes` confirms network changes and recovery without prompting. Use `--private-network` to select a private network that carries the default route, or `--public` to use public addresses. When the hosts share a private route but the server cannot reach the new host's SSH port on it, or the new host cannot reach the API on port 6443, kip stops and names the failed connection; open the port, or pass `--public` to join over public addresses instead. The server must already use WireGuard for its pod network; run `kip upgrade` first if needed.
- **Firewall:** By default, kip sets up a host firewall on a new node only when it manages the server's firewall. Use `--firewall` or `--firewall=false` to override this default. A kip-managed firewall allows 51820/udp and 10250/tcp from the other nodes and 9100/tcp from the server. On workers, kip opens only SSH to all sources. When configuring UFW for a worker, kip removes saved public rules for TCP ports 80, 443 and 6443 that exactly match its server rules, preserving every SSH port reported by `sshd -T`. Identical rules added by an operator are also removed. On a node without a kip-managed firewall, your rules must allow that traffic, restrict those ports to the listed sources, keep 8888/tcp closed to outside traffic, and limit access to any NodePort services. Existing kip-managed firewalls get updated peer rules on every join, even with `--firewall=false`, and keep their SSH rate limiting setting. kip checks operator-managed firewalld rules on existing nodes and, when firewall setup is enabled, on the joining node. A UFW firewall that kip does not manage on the server or another existing node stops the join, even with the ports open. Switch it off first. On the server, run `kip cluster harden` if kip should manage it. On a new node, such a firewall stops the join unless you pass `--firewall=false`. A new Ubuntu or Debian worker inherits the server's kip-managed UFW setting, with SSH rate limiting as the default. firewalld hosts allow SSH without rate limiting. Applying kip-managed firewalld rules restarts the active k3s service (`k3s-agent` on a worker). On the server, this temporarily interrupts API access.
- **Provider firewalls:** A cloud provider's firewall or security group must allow 51820/udp and 10250/tcp in both directions between the nodes, 9100/tcp from the server to each worker, and 6443/tcp from each worker to the server. Allow either node to initiate traffic on 51820/udp and 10250/tcp, even if the join succeeds without these rules. A stateful security group allows replies to WireGuard traffic initiated by the server, so a join can pass while the server's inbound 51820/udp rule is missing. Once the connection tracking entry expires, for example after a long idle period or a reboot, the provider can block tunnel traffic initiated by the worker.
- **Join checks:** before allowing new workloads, kip checks pod DNS, connectivity to CoreDNS on the server, Longhorn volume reads and writes, host authentication to the cluster registry, containerd's registry mirror configuration and direct registry access, and kubelet access from the server. The worker stays cordoned until the node and its Longhorn storage pods are ready. kip then replaces the cordon with a `kipper.run/checking` taint so Longhorn can attach the test volume. During validation, the taint blocks scheduling of pods that do not tolerate it. To resume an interrupted join, rerun the command with the same options.
- **Records:** kip records nodes in the `kipper-nodes` ConfigMap in `kipper-system`. `kip upgrade`, `kip cluster harden` and `kip install` require records for every cluster node. Use `kip node add` to adopt an unrecorded worker. Adopt all unrecorded nodes before joining a new node.
- **Join credentials:** each join attempt uses a fresh k3s bootstrap token valid for one hour. The command attempts to delete it after registration or a join failure. If cleanup fails, the token expires on its own. Once joined, a worker authenticates with its own agent certificates. k3s issues them for one year and renews them at each agent start while they are still valid, so a worker that restarts its agent at least once a year (`kip node reboot <worker>` does this) keeps them current. A worker whose certificates have expired or gone missing cannot reconnect: it shows NotReady and `k3s-agent` logs `failed to retrieve configuration from server: not authorized`. Run `kip node add <host>` again for that worker with the original join options. It mints a fresh join token, repeats the join and keeps the node's name and the data on its disks; workloads are kept off the node while the join is checked. If the rerun fails, follow the cause it reports.
- **Maintenance:** `kip upgrade` performs host operations on the server only. [`kip cluster uninstall`](#kip-cluster-uninstall) wipes the recorded workers before the server. `kip cluster harden` updates the server's firewall rules for peer access. `kip node reboot <worker>` drains and reboots a worker, and `kip node remove <worker>` removes one. Other worker maintenance is manual. When the first Rocky Linux, AlmaLinux or CentOS Stream node joins, `kip node add` turns on Velero's privileged file system backups, which SELinux hosts need, and waits for the backup agents to restart before it checks the worker.
- **Availability:** ingress stays on the control-plane node. Kipper configures each database, registry and in-cluster backup volume to keep one copy on the server. Workers add CPU and memory; high availability requires additional data copies and control-plane redundancy.
- **Data placement:** `kip upgrade` and `kip node add` set server-only scheduling for these volumes and report copies still on workers. `kip status` also lists these copies. For a pinned volume, `kip node reboot <worker>` moves the copy to the server when the server has room for it; otherwise the reboot refuses: the drain blocks until `--drain-timeout`, then fails, and kip cancels the reboot. For volumes reported as not pinned, run `kip upgrade` to retry pinning; a reboot may otherwise move the copy to another worker. Permanent loss of a worker holding the only copy can lose that data. `kip service info <name>` shows where the service runs and where its data is stored.

## kip node remove {#kip-node-remove}

Removes a worker from the cluster. The server cannot be removed. `kip cluster uninstall` removes the whole cluster.

```bash
kip node remove worker-1
kip node remove 203.0.113.11 --yes
```

kip records the worker as being removed, stops Longhorn from placing volume copies on it and asks Longhorn to move the copies already there. It waits until no volume has its only healthy copy on the worker, then cordons and drains it. It checks that the host still has the machine ID recorded for the worker, uninstalls k3s and wipes Kipper's data from it, and checks that k3s is gone before it deletes the node. It then removes the worker's address from the firewall on every remaining node whose firewall kip manages, names each node whose firewall it does not manage so you can remove the address there yourself, and deletes the worker's record.

If a step fails, the worker stays recorded as being removed. Fix the cause and run the command again to resume, even if the node itself has already been deleted. Without `--force`, the worker's host has to be reachable over SSH for the first run and for every rerun, because kip only deletes the node after it has checked the host's machine ID and wiped it.

To keep a worker whose removal stopped, run `kip node add` for it again, which also lifts Longhorn's eviction request. You can keep the worker until kip records it as `wiping`, just before it prints "Uninstalling k3s" or, with `--force`, starts deleting the node. After that, finish the removal and add the host again. To keep a worker while its removal is still running, stop the removal first.

Without `--force`, kip starts a removal only while the worker's host answers with its recorded machine ID. A host that is gone for good needs `--force`. While a removal is unfinished, kip blocks adding other nodes, rebooting the worker, hardening or reinstalling the cluster and starting another removal. These operations could change peer rules or schedule work on the worker. Only one `kip node add` or `kip node remove` runs on a cluster at a time. A second command is refused with a message naming the running command. A stopped command waits up to five minutes for its outstanding steps to finish. Each drain round has a timeout of at most two minutes. While that cleanup is running, a new command is refused with a message explaining the wait.

When Longhorn cannot place a copy elsewhere, for example because the other nodes lack disk space, the command stops before draining and names the volumes. Evicted pods lose their emptyDir data.

For a worker whose host is powered off or gone for good, `--force` deregisters it without changing anything on the host. It first checks whether the host still answers, without a password prompt and with short connection and command timeouts. If the connection or machine ID check fails, or the address now belongs to another machine, kip deletes the worker's node, requests deletion of its Longhorn node, removes its address from the kip-managed firewalls of the remaining nodes and drops its record. It refuses when the host still answers with the worker's machine ID, because forced removal could leave k3s running on that host. Remove it without `--force` instead. When a volume has a replica on the worker and no healthy copy elsewhere, its only data may be on the worker, so `--force` also needs `--accept-data-loss`. The host keeps its k3s credentials for the cluster, so wipe it with `/usr/local/bin/k3s-agent-uninstall.sh` or reinstall it before it comes back. Otherwise, its agent or a kip command already in transit to the host when an earlier run was interrupted can register the node again. To resume an interrupted forced removal, use `--force` again and include `--accept-data-loss` if it was needed. A rerun without `--force` takes the normal path and needs the host.

| Flag | Required | Description |
|---|---|---|
| `--yes` | No | Remove without asking for confirmation |
| `--force` | No | Remove a worker whose host is powered off or gone for good, without changing anything on the host |
| `--accept-data-loss` | No | With `--force`, remove the worker even when a volume's only data is on it |
| `--timeout` | No | Maximum wait for Longhorn to move copies and, separately, for the drain (default `30m`) |
| `--ssh-key` | No | SSH private key. Overrides `KIP_SSH_KEY` and `cluster.ssh_key` |

## kip node list {#kip-node-list}

Lists all nodes in the cluster with role, status, version, and IP.

```bash
kip node list
```

## kip node reboot {#kip-node-reboot}

Reboots a server or worker and waits for the cluster and previously ready apps or workloads to be ready again. Use it to apply a new kernel or service updates listed under Pending restarts in `kip status`.

```bash
kip node reboot
kip node reboot --yes --skip-backup
kip node reboot worker-1
```

| Flag | Required | Description |
|---|---|---|
| `--yes` | No | Reboot without asking for confirmation |
| `--skip-backup` | No | Reboot without taking a backup first |
| `--ssh-key` | No | SSH private key for the node; overrides `KIP_SSH_KEY` and `cluster.ssh_key` |
| `--timeout` | No | How long to wait for node, component, and app readiness after reboot (default `20m`) |
| `--drain-timeout` | No | Maximum wait for a worker to drain, including volume copies to other nodes (default `30m`, minimum `1s`) |
| `--ignore-degraded-volumes` | No | Reboot even when an attached volume is not healthy or its health cannot be read |

Apps on a single-server cluster are unavailable during the reboot. For a server, the command:

1. Reads the server's boot ID over SSH. Stops if the server cannot be reached.
2. Asks for confirmation, unless you pass `--yes`.
3. Unless `--skip-backup` is set, takes a backup using the defaults from `kip backup create` and waits up to 30 minutes for completion. A failed or timed-out backup cancels the reboot.
4. Records apps with at least one ready replica, then reboots the server. If listing apps fails, it warns and skips the app readiness checks. If console-api uses `:latest`, it may start with a newer image. Before rebooting, run `kip upgrade --check` to check for required data updates.
5. Waits until the server reports a new boot ID. If it still reports the old ID after three minutes, the command stops with instructions for investigating the cause.
6. Waits for Kubernetes to report the new boot ID and for every node and component from `kip status` to pass two consecutive readiness checks.
7. Waits for each app recorded in step 4 to have a non-terminating pod that became ready after the reboot was scheduled. Ready, non-terminating pods on workers also count, even if they became ready before the server reboot. If time runs out, it lists apps still waiting for readiness or reports an error if readiness could not be checked.
8. Runs the host checks from `kip status` over SSH, including Pending restarts. These checks also run if cluster or app readiness times out, even when the API server is unavailable.

On a cluster with workers, the server reboot checks volume health before confirmation and again before rebooting, as described below. A single-server cluster skips this check.

When kip cannot read the node list, use `kip node reboot --skip-backup` to reboot the server without a backup. Add `--yes` for unattended use. kip retries the node list before rebooting. If workers are found, it applies the volume health checks described below, including `--ignore-degraded-volumes` and the exception for excess requested replicas. If the list is still unavailable, kip warns and proceeds. Volume replicas on the server are unavailable during the reboot.

To reboot a worker, name it by its node name or address. Without an argument, or with the server's name or address, the command reboots the server as described above. For a worker, the command:

1. Checks the worker's machine ID against its recorded ID and requires Longhorn's drain policy to copy last healthy replicas to another node (`kip upgrade` configures this policy).
2. Lists volumes with healthy replicas on the worker and shows whether healthy copies exist elsewhere. Requires attached Longhorn volumes to report healthy before proceeding, because rebooting while a volume is degraded can reduce its availability further. Longhorn rebuilds degraded volumes on its own when healthy replicas and node capacity are available. Allow time for rebuilding, then rerun the command. A volume requesting more replicas than the cluster has nodes only triggers a warning when every node holds a healthy copy of it; a volume that has lost one of those copies still blocks. Use `--ignore-degraded-volumes` to proceed with unhealthy volumes or when volume health cannot be checked.
3. Asks for confirmation and takes a backup, as for the server.
4. Records fully ready Deployments, StatefulSets and DaemonSets with at least one desired pod and checks volume health again, since it can change during the confirmation and backup. Then it cordons and drains the worker. Controllers can recreate evicted pods on other nodes; DaemonSet and mirror pods stay. Evicted pods lose their `emptyDir` data. Longhorn copies volumes with their last healthy replica on the worker to another node before the drain finishes.
5. Checks that every volume with a healthy replica still on the worker also has a healthy copy elsewhere. If draining or this check fails, the command reports the error, attempts to lift any cordon it set, and cancels the reboot. Unmanaged pods block the drain; `--drain-timeout` limits the wait.
6. Reboots the worker and waits for its new boot ID.
7. Lifts any cordon it set, then waits for every node and component to be ready and for the recorded workloads to be fully ready again. A worker that was already cordoned stays cordoned.
8. Lists attached volumes still reporting unhealthy, without waiting for recovery. A volume requesting more replicas than the cluster has nodes allows the next reboot with a warning if every node holds a healthy copy.
9. Runs the Pending restarts check on the worker.

The `--timeout` period starts after the reboot is scheduled; backup and drain time are separate. Boot times vary by host. If the wait times out, run `kip status` to check progress. If SSH remains unreachable, check the host through your provider’s console.

## kip auth verify {#kip-auth-verify}

Proves your OIDC identity authenticates and authorizes against the cluster, the same check the installer runs inline. Run it after a headless install, or any time you want to confirm the login path works end to end.

```bash
kip auth verify
```

```
  ✔  Authenticated and authorized as oidc:admin@shop.kipper.run
```

It exits non-zero when the API server rejects your token or an admin identity is denied access, so it doubles as the CI signal that the authenticator is live. It never changes your kubeconfig.

## kip auth kubeconfig {#kip-auth-kubeconfig}

Converts a legacy or admin kubeconfig to authenticate through your `kip auth login` session. Fresh installations normally use this per-user configuration already.

```bash
kip auth kubeconfig
```

```
  ...  Checking that your login reaches this cluster (up to a minute)

  ✔  /Users/anna/.kip/clusters/shop.kipper.run.yaml now authenticates as your OIDC identity
     kubectl runs `kip auth kubectl-token` for short-lived tokens.
```

The check comes first because the file being replaced is often the only credential that reaches the cluster from this machine. It is the same proof [`kip auth verify`](/en/cli-reference#kip-auth-verify) makes: your session token is sent to the API server, which has to accept it as you and grant you access. A cluster that answers anything else keeps the credential it has.

```
  ✗  This cluster did not accept your login: the API server rejected the token
     /Users/anna/.kip/clusters/shop.kipper.run.yaml is unchanged, so it still reaches the cluster.
     Run 'kip cluster ca status' to see what the API server has loaded,
     and 'kip auth verify' to re-check the login once it is fixed.

this cluster does not accept your login, kubeconfig unchanged
```

A rejected token usually means the API server has no authenticator for your issuer. `kip cluster ca status` says so directly, reporting that the authentication config names no issuer.

The command also preserves the existing kubeconfig when it cannot reach the API server:

```
  ⚠  Could not reach the API server to check your login: Get "https://203.0.113.10:6443/apis/authentication.k8s.io/v1/selfsubjectreviews": dial tcp 203.0.113.10:6443: i/o timeout
     /Users/anna/.kip/clusters/shop.kipper.run.yaml is unchanged and still works.

login could not be checked, kubeconfig unchanged
```

Sign in with `kip auth login` before running it:

```
  ✗  Your login could not be checked, so /Users/anna/.kip/clusters/shop.kipper.run.yaml is unchanged.

not authenticated. Run: kip auth login
```

After this, the kubeconfig carries no credential at all: kubectl obtains a token valid for a few minutes each time it needs one, every action in the Kubernetes audit log names your email, and removing a person's access means removing their account rather than rotating certificates. The admin certificate stays on the server as the break-glass credential (see [Architecture](/en/architecture)).

The file also names the cluster it signs in against, so run this once per cluster on each machine that holds one. `kip cluster domain` keeps that name current when a cluster's domain changes.

## kip auth kubectl-token {#kip-auth-kubectl-token}

The credential plugin the rewritten kubeconfig calls. kubectl runs it automatically whenever it needs a token; there is normally no reason to run it by hand.

```bash
kip auth kubectl-token --cluster-domain shop.kipper.run
```

```json
{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"eyJhbGciOi…","expirationTimestamp":"2026-07-21T09:15:00Z"}}
```

`--cluster-domain` names the cluster whose session to serve, and kip writes it into every kubeconfig it renders, so kubectl passes it for you. It has to come from the file because kubectl tells a credential plugin nothing about which kubeconfig invoked it: each kubeconfig therefore signs in against its own cluster whatever `kip cluster use` is set to, which is what lets you work across two clusters in two terminals.

`kip cluster use` still decides which cluster the rest of kip talks to. It has no say over kubectl.

When the session has expired entirely it prints `session expired. Run: kip auth login` on stderr, which kubectl surfaces verbatim. A kubeconfig written before this flag existed prints `this kubeconfig does not say which cluster it authenticates to. Regenerate it with: kip auth kubeconfig`, and no token is issued until you do.

## kip auth reset-password {#kip-auth-reset-password}

Generates a new admin password, writes it to Dex, displays the new credentials, and restarts Dex.

```bash
kip auth reset-password
```

Uses your local session or the authentication configured in the cluster's kubeconfig. If the kubeconfig requires a session and none is available, kip retrieves the server's admin credentials over SSH and keeps them in memory.

Pass `--ssh-key <key>` to use SSH explicitly. Resetting the password requires permission to read and update Dex's ConfigMap and patch its deployment, or SSH access to the server as root or as the cluster's SSH user with passwordless sudo.

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--ssh-key` | No | `KIP_SSH_KEY`, then the key saved for the cluster | Private key for resetting the password over SSH |

## kip discover {#kip-discover}

Find Kipper-labeled workloads on the cluster that have no owning Kipper CR. Read-only.

```bash
kip discover
```

Kipper considers a Service or App or Volume or Function to "exist" when its CR exists. A Deployment, StatefulSet, or PVC carrying `app.kubernetes.io/managed-by=kipper` without a matching CR is drift. It will not show up in `kip service list` or in the console, even though it occupies cluster resources.

`kip discover` lists each orphan and prints a suggested kip command that recreates it as a proper CR with the workload's current settings. Run the suggested command to bring the orphan under management. The controller adopts the existing workload to match the new CR, no deletion needed. Edit the suggested command first if you want to change anything.

For Deployments, the suggestion uses `kip app deploy` with `--image`, `--port`, `--memory`, `--cpu`, `--env`, `--secret`, `--replicas`. For StatefulSets, `kip service add` plus the service type (postgres, redis, and so on), with `--storage`, `--memory`, `--cpu`. For PVCs, `kip volume create` with `--size`. Functions print a comment instead, because the source code is not on the workload and has to come from `kip function create`.

Workloads in the `kipper-system` namespace are skipped. The console, console-api, and zot legitimately have no owning CR.

## kip cert list {#kip-cert-list}

Lists every certificate on the current cluster with its host, current state,
the age of the last state change, and cert-manager's reason when it is not
ready. The list includes platform hosts and app routes.

```bash
kip cert list
```

## kip cert email {#kip-cert-email}

Shows or updates the Let's Encrypt email used for TLS certificates. This is the email cert-manager uses when registering with Let's Encrypt for automatic certificate issuance.

```bash
kip cert email                    # show current email
kip cert email admin@example.com  # update email and renew stuck certs
```

When updating, the command re-registers with Let's Encrypt using the new email and triggers renewal for any certificates that are stuck or failed. Certificates usually come through within 1-2 minutes.

See [Domains & SSL: Troubleshooting certificates](/en/domains#troubleshooting-certificates) for common certificate issues.

## kip ai {#kip-ai}

`kip ai` covers two related things: choosing which AI provider Kipper itself uses (for log analysis, Dockerfile generation, diagnostics), and installing a private LLM stack inside the cluster that your apps can call.

### kip ai configure / kip ai status {#kip-ai-configure-kip-ai-status}

Configure which AI provider Kipper uses. Supports Claude (Anthropic), OpenAI, and Ollama (self-hosted).

```bash
kip ai configure                                     # interactive setup
kip ai configure --provider claude --key sk-ant-...  # non-interactive
kip ai configure --provider ollama                   # self-hosted (no key needed)
kip ai status                                        # show current config and bundle health
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--provider` | No | — | AI provider: `claude`, `openai`, `ollama` |
| `--key` | No | — | API key (not needed for Ollama) |
| `--model` | No | — | Model override |
| `--ollama-url` | No | `http://localhost:11434` | Ollama server URL |

### kip ai admin create {#kip-ai-admin-create}

Seed the first LibreChat admin account after `kip ai install`. The bundle ships with open registration off, so an admin must be created once before anyone can log in. Runs `npm run create-user` inside the running librechat pod via the Kubernetes API. No kubectl required.

```bash
kip ai admin create --email you@example.com --name 'Your Name' --password 'a-strong-password'
kip ai admin create --email you@example.com --name 'Your Name' --username alice --password '...'
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--email` | Yes | — | Admin email address |
| `--password` | Yes | — | At least 8 characters |
| `--name` | Yes | — | Display name shown in the chat UI |
| `--username` | No | local part of `--email` | LibreChat username |

### kip ai install / kip ai uninstall {#kip-ai-install-kip-ai-uninstall}

Install or remove the in-cluster AI bundle (Ollama and LibreChat). The full walkthrough is on the [AI Bundle](./ai) page.

```bash
kip ai install                            # detect tier, pick a model, install
kip ai install --host chat.example.com    # override the chat hostname
kip ai install --model qwen2.5:7b-instruct-q4_K_M
kip ai uninstall                          # remove the bundle and wipe its data
```

`kip ai uninstall` is destructive: it deletes the workload, the PVCs (model cache, chat history, MongoDB), credentials, and the `kipper-ai` namespace. Take a blocking snapshot first with `kip ai backup --name pre-uninstall --wait` if you want to preserve any of it. Uninstall refuses by default while a Kipper AI backup is still in flight; pass `--force` to override (you'll get an unrestorable snapshot if you do).

| Flag | Command | Default | Description |
|---|---|---|---|
| `--host` | install | `chat.<cluster-domain>` | External chat UI hostname |
| `--model` | install | tier-appropriate Qwen 2.5 | Ollama model tag to preload |
| `--pvc-size` | install | 10/30/60 GiB by tier | Model cache PVC size |
| `-y, --yes` | install | `false` | Skip the auto-configure prompt |

### AI bundle health on the Platform page {#ai-bundle-health-on-the-platform-page}

The Platform page surfaces a drift check for each installed AI bundle. The check reads the `kipper-ai-bundle-state` and `kipper-rag-bundle-state` ConfigMaps in `kipper-ai` and confirms every expected workload (Ollama, LibreChat, AnythingLLM, Qdrant, plus their Ingresses) still exists.

If a bundle was installed but a workload has disappeared, the panel renders the missing resources and points at the `kip ai install` command that reconciles. This is the same diagnosis path that used to require `ssh + kubectl get`.

### kip ai backup / kip ai restore {#kip-ai-backup-kip-ai-restore}

Velero-backed snapshot of the AI bundle. Velero is a Kipper system component, so no extra setup is needed.

```bash
kip ai backup                            # auto-name, exits after 60s warmup
kip ai backup --name pre-upgrade         # named, exits after 60s warmup
kip ai backup --name pre-upgrade --wait  # block until completion (CI scripts)
kip ai backup show --name pre-upgrade    # detailed status (phase, items, errors)
kip ai backup list
kip ai backup delete --name pre-upgrade           # exits after 60s warmup
kip ai backup delete --name pre-upgrade --wait    # block until Backup CRs are gone
kip ai restore --name pre-upgrade        # requires kipper-ai uninstalled first
```

| Flag | Command | Default | Description |
|---|---|---|---|
| `--name` | backup | timestamped | Snapshot name |
| `--wait` | backup | `false` | Block until completion instead of exiting after the 60s warmup |
| `--wait` | backup delete | `false` | Block until both Backup CRs are gone instead of exiting after the 60s warmup |
| `--name` | backup show | — | Snapshot to inspect (required) |
| `--name` | backup delete | — | Snapshot to delete (required) |
| `--name` | restore | — | Snapshot to restore (required) |

## kip app info {#kip-app-info}

Shows an app's status, scaling, and CPU and memory settings.

```bash
kip app info backend --project shop --environment prod
```

```
  App: backend

  Status:  running, 2/2 ready
  Image:   ghcr.io/acme/api:v1

  Autoscaling: on (CPU 70%)
  Desired: 2 (set by autoscaling)   Min: 2   Max: 5
  Ready: 2
  cpu: target 70%, current 12%
  Last scaled: 2026-10-10 20:56 UTC (2h ago)

  Resources (profile jvm, from the pod template)
  CPU:     request 300m, limit 1
           the autoscaler tracks CPU, so Kipper leaves the CPU request alone
  Memory:  request 2Gi, limit 2Gi
           sized by Kipper; latest recommendation: request 1536Mi, limit 2Gi

  Conditions
  ✔  ServiceBindingsReady: BindingsInjected
           every declared service binding was injected
  ✘  BindingDatabasesReady: DatabaseCreateFailed
           binding "db": creating "shop" in service "db": no running pod found for service "db"
```

The Conditions section shows the app's controller-reported conditions: ✔ for `True`, ✘ for `False`, and ⚠ for an unknown status. Read each reason and message for context: `RolloutComplete: False` can mean pods are still being replaced, while `RouteReady: True` can include a warning about a shared route name.

| Flag | Required | Description |
|---|---|---|
| `--project` | No | Project name |
| `--environment` | No | Target environment |

The resource lines show the app's pod-template values. During a rollout, some pods may still use earlier values. Each resource includes a sizing explanation:

- **automatic:** Kipper manages sizing and shows its latest recommendation when it differs from the template. Recommendations take effect when sizing rules allow. Paused tuning keeps the current size; autoscaler targets restrict changes to the tracked resource.
- **bounded:** you set a request below a limit, and Kipper moves the request between them.
- **fixed:** Kipper uses the size you set.
- **held:** ownership is unknown, so Kipper keeps the values until you set them.

For automatic and bounded resources, the sizing explanation accounts for autoscaling: Kipper keeps tracked CPU requests unchanged and increases tracked memory only after an out-of-memory kill. If the policy is invalid and you lack permission to read the autoscaler, sizing is shown as unknown.

See [Your own CPU and memory values](/en/resource-management#your-own-values).

## kip app update {#kip-app-update}

Updates a deployed app's image, resource settings, or health check. Changes to the pod configuration trigger a rolling update.

```bash
kip app update backend --image ghcr.io/acme/api:v2.1.0
kip app update backend --profile jvm
kip app update backend --memory-request 512Mi --memory-limit 2Gi
kip app update backend --tuning auto
kip app update backend --health-path /actuator/health/readiness --health-startup-timeout 600
```

| Flag | Required | Description |
|---|---|---|
| `--image` | No* | New container image |
| `--profile` | No* | Resource profile: `lightweight`, `standard`, `compute-heavy`, `memory-heavy`, or `jvm` |
| `--memory` / `--cpu` | No* | Set request and limit to the same value |
| `--memory-request` / `--cpu-request` | No* | Minimum request for automatic tuning |
| `--memory-limit` / `--cpu-limit` | No* | Container limit and maximum request for automatic tuning |
| `--tuning auto` | No* | Clear the CPU and memory values so Kipper sizes the app on its own |
| `--health` | No* | Health check type: `http`, `tcp`, `none` (disable the app readiness check), or `auto` to remove a declared check |
| `--health-path` | No* | Path for an `http` check, e.g. `/ready`. Selects `http` when `--health` is not given |
| `--health-port` | No* | Port to check, when it is not the app port |
| `--health-startup-timeout` | No* | Seconds from pod creation to readiness before the pod is reported as stuck, including image pulls (default 300) |
| `--health-timeout` | No* | Timeout for each check in seconds (default 2) |
| `--closed` | No* | Publish only the route's public paths; use `--closed=false` to reopen. See [closing a route](/en/routing#closing-a-route-except-for-one-path) |
| `--project` | No | Project name |
| `--environment` | No | Target environment |

\* Supply at least one update flag: `--image`, `--profile`, a CPU or memory flag, `--tuning auto`, `--redirect-from`, `--internal-path`, `--public-path`, `--closed`, `--health` or a `--health-*` flag.

A request below its limit defines a range for automatic tuning. A request or limit supplied alone sets a fixed size; a request above its limit is rejected. Use either `--memory` or the separate memory request/limit flags. The same rule applies to CPU. `--tuning auto` cannot be combined with CPU or memory values. Changing the profile preserves explicit resource values. See [Your own CPU and memory values](/en/resource-management#your-own-values).

The health flags also work with `kip app deploy`. Updates merge into the existing check; switching types clears unsupported settings. Changing the readiness probe triggers a rollout, while changing only the startup timeout leaves pods running. See [Health checks and rollouts](/en/deploying-apps#health-checks-and-rollouts).

## kip app route {#kip-app-route}

Shows, sets or removes an app's public route.

```bash
kip app route show backend --project shop
kip app route set backend --host shop.example.com --path /api
kip app route remove backend --yes
```

`show` prints the host, path, URL and configured route settings: authentication, internal and public paths, redirects, the CSP allowlist and rate limit. It also displays the controller's `RouteReady` condition when available. Routes without an explicit host use a hostname derived from the app name, environment and cluster domain.

`set` updates the host and path you provide while preserving other route settings. Updating an existing route's host or path keeps pods running. A new route uses the supplied host or the default hostname, with `/` as the default path. Adding a route may restart pods to add Kipper's instance proxy.

For `kipper.run`, use the cluster's domain (such as `lab.kipper.run`) or a name such as `shop--lab.kipper.run`. The gateway routes these names to your cluster, and its wildcard certificate covers one label under `kipper.run`. For a custom host, point its DNS record at the server so its certificate can be issued. `set` waits briefly for the controller and reports whether publication is confirmed.

`remove` deletes the route and its settings, then waits briefly for the route's Ingresses to be removed. The app remains accessible inside the cluster and through links. Pods may restart to remove the instance proxy. Adding a route later uses defaults rather than restoring the removed settings. The command lists the settings being removed and asks for confirmation. Pass `--yes` to skip the prompt; this flag is required without an interactive terminal.

| Flag | Command | Description |
|---|---|---|
| `--host` | `set` | Hostname to serve the app on |
| `--path` | `set` | Path the app is served under (default `/` for a new route) |
| `--yes`, `-y` | `remove` | Remove without asking |
| `--project` | all | Project name |
| `--environment` | all | Target environment |

## kip app scale {#kip-app-scale}

Sets the replica count for a deployed application. Give the count as the second argument or with `--replicas`. A count is required to prevent accidental scaling to zero.

```bash
kip app scale backend 3
```

| Flag | Required | Description |
|---|---|---|
| `--replicas` | If the second argument is omitted | Number of replicas. Equivalent to the second argument. |
| `--project`, `--environment` | No | The app's project and environment |

For a stopped app, the new replica count takes effect on start. Use [`kip app stop`](#kip-app-stop-kip-app-start) to take an app out of service while preserving its count and recording a reason.

For an app with bounds, counts outside them (including 0) are refused with a message naming the bounds. While autoscaling is on, every count is refused, because the autoscaler sets it. `kip app deploy --replicas` follows the same bounds on an existing app; on an autoscaled app it stores the count for when autoscaling is off and prints a note saying so.

## kip app autoscale {#kip-app-autoscale}

Configures automatic scaling for an app. The minimum and maximum are bounds on the app's replica count whether the autoscaler or you set it. See [Autoscaling](/en/deploying-apps#autoscaling).

```bash
kip app autoscale backend --min 2 --max 10 --cpu 70
kip app autoscale backend --max 15
kip app autoscale backend --status
kip app autoscale backend --off
kip app autoscale backend --remove
```

| Flag | Required | Description |
|---|---|---|
| `--min` | No | Minimum number of replicas. 1 on a first enable |
| `--max` | No | Maximum number of replicas. 5 on a first enable |
| `--cpu` | No | Target CPU use as a percentage of the request. 0 removes the target |
| `--memory` | No | Target memory use as a percentage of the request. 0 removes the target |
| `--status` | No | Show desired replicas, bounds, ready pods, current use and autoscaling problems |
| `--off` | No | Switch autoscaling off, retaining the desired count within the bounds |
| `--remove` | No | Remove the bounds from an app whose autoscaling is off |
| `--project`, `--environment` | No | The app's project and environment |

Any of `--min`, `--max`, `--cpu` or `--memory` switches autoscaling on. Flags you leave out keep their stored values. Missing bounds default to 1 and 5. When neither target is set, CPU defaults to 70%. A change to the bounds moves the stored count into them and prints the move.

`--off` keeps the count the autoscaler last set, moved into the bounds if it lies outside them. A stopped app keeps its stored count. A failed Deployment read also falls back to the stored count and prints a warning. `--remove` refuses while autoscaling is on; pass `--off --remove` to do both. `--status` takes precedence over the other flags. When the App's `AutoscalingReady` condition is not True, it adds a line starting `⚠  Autoscaling is not ready (` with the reason and message the cluster reports.

`--off` and `--remove` ignore `--min`, `--max`, `--cpu` and `--memory` and print a notice naming the flags they ignored. To change a setting and switch off, run the change first and `--off` after it.

The cluster refuses a change that leaves the autoscaling block invalid, and switching off counts as a change. On an app whose stored minimum is above its maximum, `--off` (and so `--off --remove`) fails with `minReplicas must not exceed maxReplicas`. Set valid bounds with `--min` and `--max` first, then switch off. An app stored with autoscaling on, a minimum above 1 and no maximum fails `--off` with `set maxReplicas with minReplicas, or leave both out to remove the bounds`, so first set a maximum no lower than the stored minimum, for example `kip app autoscale backend --max 5` for a minimum of up to 5. In both cases kip names the command that fixes the block. See [When settings do not add up](/en/deploying-apps#when-settings-do-not-add-up).

## kip app stop / kip app start {#kip-app-stop-kip-app-start}

`kip app stop` shuts down an app’s pods. `kip app start` resumes the app using its saved configuration.

```bash
kip app stop backend --reason "not needed until the next campaign"
kip app start backend
```

| Flag | Command | Description |
|---|---|---|
| `--reason` | `stop` | Optional stop reason, up to 500 bytes. On an already stopped app, only an explicit `--reason` changes the reason |
| `--for-migration` | `stop` | Marks a new stop as a migration write freeze, which stays on the source. Existing stop records keep their original type |
| `--project`, `--environment` | both | The app's project and environment |

Stopping preserves the app’s configuration, replica count, autoscaling settings and data. Its route returns a "this app is stopped" page. For an app scaled to zero without a stop record and without autoscaling, use `kip app scale` to raise the count. See [Stopping and starting an app](/en/deploying-apps#stopping-and-starting-an-app).

## kip app env / kip app secret {#kip-app-env-kip-app-secret}

Manage environment variables and secrets for an application.

```bash
kip app env set backend LOG_LEVEL=debug       # set env var
kip app env list backend                      # list with values visible
kip app env delete backend LOG_LEVEL          # remove

kip app secret set backend DATABASE_URL       # interactive hidden prompt
kip app secret list backend                   # keys only, values masked
kip app secret reveal backend DATABASE_URL    # show a single value
kip app secret rollback backend DATABASE_URL  # restore previous value
kip app secret delete backend DATABASE_URL    # remove
```

Every command that changes configuration saves it without restarting the
workload; add `--restart` to apply it immediately. See [Secrets and
configuration](/en/secrets).

Secrets can also be set at deploy time with `--secret` on `kip app deploy` (repeatable; a bare `KEY` prompts with hidden input), so the app never starts without them.

See [Secrets & Environment](/en/secrets) for full documentation.

## kip app link / kip app unlink {#kip-app-link-kip-app-unlink}

Connect apps so one can reach another via a URL.

```bash
kip app link domain-service api-gateway             # internal URL (backend-to-backend)
kip app link domain-service webapp --public          # public URL (for frontend apps)
kip app unlink domain-service api-gateway            # removes DOMAIN_SERVICE_URL
```

Use `--public` when linking to a frontend app that runs in the browser. It injects the target's public HTTPS URL instead of the internal Kubernetes DNS.

See [Deploying Apps: Linking apps](/en/routing#linking-apps) for details.

## kip service {#kip-service}

Manage stateful services (databases, caches) with persistent storage.

```bash
kip service add postgres --name mydb         # deploy PostgreSQL
kip service add redis --name cache           # deploy Redis
kip service list                             # list all services
kip service info mydb                        # show connection details
kip service share mailhog --expires 72h      # shareable link to a service web UI
kip service import mydb --file dump.sql      # load a database dump
kip service export mydb --file nightly.dump  # dump a database to a local file
kip service restore-dump mydb --to mydb-new  # restore mydb's consistent dump from a backup into an empty service
kip service credentials                      # check every service owns its credentials
kip service delete mydb --delete-data        # delete the service and its volume
```

`kip service share` accepts `--project` and `--environment` to target a service outside the active project, `--expires` (up to 720h) for the link lifetime, and `--label` for a note in the listing. Use `--list` to see a service's links, `--revoke <id>` to kill one, and `--revoke-all` plus `--rotate-key` to contain a leak.

`kip service restore-dump` takes `--to` (required, an existing Postgres service without tables), `--backup` (default: the latest completed backup that covers the namespace), `--project` and `--environment`. See [Restore a Postgres dump into a new service](/en/backups#restore-a-postgres-dump-into-a-new-service).

See [Stateful Services](/en/services) for full documentation.

## kip project {#kip-project}

Manage projects and environments.

```bash
kip project create blog --environments test,acc,prod
kip project create blog --display-name "example.com Domain Platform" --environments test,acc,prod
kip project list
kip project add-env blog prod
kip project remove-env blog staging
kip project delete blog
```

`kip project remove-env` deletes the matching namespace and everything in it. You'll be asked to type the environment name to confirm. See [Projects & Environments](/en/environments#adding-and-removing-environments) for details.

### kip project members {#kip-project-members}

Manage who can access a project and what they can do.

```bash
kip project members list blog
kip project members add blog jordan@acme.com deployer
kip project members remove blog jordan@acme.com
```

Roles are `owner`, `deployer`, or `viewer`. See [Project Members](/en/project-members) for what each role can do.

### kip project use {#kip-project-use}

Set a persistent project context for the current cluster, so the rest of the kip commands do not need `--project` and `--environment` flags every time.

```bash
kip project use blog           # active project: blog, default environment
kip project use blog/test      # active project: blog, environment: test
kip project use blog test      # same, with a space instead of a slash
kip project use --clear              # forget the active project on this cluster
```

The active project is stored per cluster in `~/.kip/config.yaml`. After setting it, `kip service list`, `kip volume list`, `kip function list` and every `kip app` command, including `kip app deploy`, `kip app logs`, `kip app rebuild` and `kip app webhook`, resolve to the active project's namespace automatically. `kip app promote` is the exception: it moves an app between environments, so it always takes `--project`. `kip cluster list` shows the active project on each cluster line.

Explicit `--project` flags still win. Passing `--project other-name` switches that single command to the other project; the persisted environment is not carried over so a different project never inherits a stale environment.

See [Projects & Environments](/en/environments) for full documentation.

## kip app promote {#kip-app-promote}

Promote an app from one environment to the next (copies the image tag only).

```bash
kip app promote backend --from test --to acc --project blog
kip app promote --all --from acc --to prod --project blog
```

See [Projects & Environments](/en/environments) for full documentation.

## kip function {#kip-function}

Manage serverless functions. Functions scale to zero when idle and spin up on demand. Alias: `kip fn`.

```bash
kip function create process-image --image myregistry/processor:v1 --port 8080 --project blog
kip function create db-sync --trigger postgres --source mydb --query "SELECT * FROM events WHERE processed = false" --project blog
kip function create cache-worker --trigger redis --source cache --list jobs --project blog
kip function list --project blog
kip function logs process-image --project blog
kip function logs process-image --project blog -f
kip function delete process-image --project blog
```

### kip function create flags {#kip-function-create-flags}

| Flag | Required | Default | Description |
|---|---|---|---|
| `--image` | Yes | — | Container image for the function |
| `--trigger` | No | `http` | Trigger type: `http`, `postgres`, `mysql`, `redis`, `minio` |
| `--port` | No | `8080` | Port the function listens on |
| `--source` | No | — | Service name for event triggers |
| `--query` | No | — | SQL query for postgres/mysql triggers |
| `--mark-done` | No | — | SQL to mark rows as processed |
| `--list` | No | — | Redis list name for redis triggers |
| `--bucket` | No | — | MinIO bucket name for minio triggers |
| `--project` | No | `default` | Project name |
| `--environment` | No | — | Target environment |

See [Serverless Functions](/en/functions) for full documentation.

## kip job {#kip-job}

Run one-off tasks and scheduled jobs.

```bash
kip job run --name migrate --image myapp:latest --command "npm run migrate" --project blog --environment test
kip job schedule --name cleanup --image myapp:latest --command "python cleanup.py" --cron "0 3 * * *"
kip job list --project blog
kip job history cleanup
kip job delete cleanup
```

See [Jobs & Scheduled Tasks](/en/jobs) for full documentation.

## kip volume {#kip-volume}

Create shared persistent volumes that can be mounted by multiple apps. Backed by Longhorn with ReadWriteMany access.

```bash
kip volume create uploads --size 5Gi --project blog --environment test
kip volume mount uploads webapp --path /data --project blog --environment test
kip volume unmount uploads webapp --project blog --environment test
kip volume list --project blog
kip volume delete uploads --delete-data --project blog --environment test
```

### kip volume create flags {#kip-volume-create-flags}

| Flag | Required | Default | Description |
|---|---|---|---|
| `--size` | No | `5Gi` | Volume size |
| `--project` | No | `default` | Project name |
| `--environment` | No | — | Target environment |

### kip volume mount flags {#kip-volume-mount-flags}

| Flag | Required | Default | Description |
|---|---|---|---|
| `--path` | No | `/data` | Mount path inside the container |
| `--project` | No | `default` | Project name |
| `--environment` | No | — | Target environment |

Mounts are recorded on the volume and applied to the app automatically, so they survive image updates and redeployments. `kip volume unmount` removes the mount again; the volume and its data stay available for other apps. Deleting a volume needs `--delete-data` because it permanently destroys the data.

See [Storage](/en/storage) for full documentation.

## kip user {#kip-user}

Manage cluster users and roles. Kipper has three cluster roles:

- `admin`: manage the cluster, settings, users and platform components.
- `deployer`: sign in and use the console's AI assistant.
- `viewer`: sign in to the console.

In the console, admins can work in every project. Other users get access to project apps, services and data through [project membership](/en/project-members).

```bash
kip user list
kip user add dev@example.com --role deployer
kip user add pm@example.com --role viewer --password secret123
kip user invite --email dev@example.com --role deployer      # invite a developer
kip user invite --email ops@example.com --role admin --expires 24h
kip user role dev@example.com admin                # change role
kip user monitoring grant dev@example.com          # let them open Grafana
kip user monitoring revoke dev@example.com
kip user remove dev@example.com
kip user import dex-snapshot.yaml                  # bulk-import Dex users from a snapshot
kip user import dex-snapshot.yaml --restart-dex    # also roll Dex so the new config takes effect
```

### kip user monitoring {#kip-user-monitoring}

Grant or revoke Grafana access for an existing Kipper user:

```bash
kip user monitoring grant <email>
kip user monitoring revoke <email>
```

This grants access to **every project's metrics and logs**, plus Explore and dashboard editing. Cluster admins have access automatically; revoking an explicit grant does not remove their access.

Use `kip user list` to check the **MONITORING** column. See [Accessing Grafana](/en/observability#accessing-grafana) for sign-in and permission details.

### kip user import {#kip-user-import}

Merges the `staticPasswords` and `connectors` blocks from a captured `dex-config` snapshot into the live `dex/dex-config` ConfigMap. The snapshot can be either a full ConfigMap manifest (`kubectl get cm dex-config -n dex -o yaml`) or the raw Dex config YAML directly.

Existing entries on the live side always win on conflicts, so the install admin cannot get overwritten with stale snapshot data. Use this after a Velero restore that brought a pre-rename `dex-config` across. Without it, production users live in the snapshot but the new install only knows about the bootstrap admin.

Pass `--restart-dex` to roll the Dex Deployment automatically. Without it, run `kubectl -n dex rollout restart deploy/dex` once the import finishes.

### kip user add flags {#kip-user-add-flags}

| Flag | Required | Default | Description |
|---|---|---|---|
| `--role` | No | `deployer` | Role: `admin`, `deployer`, or `viewer` |
| `--password` | No | prompted | Password (prompted interactively if not provided) |

### kip user invite flags {#kip-user-invite-flags}

| Flag | Required | Default | Description |
|---|---|---|---|
| `--email` | Yes | — | Address of the person being invited. The account is created under it, and only that address can accept the invite |
| `--role` | No | `deployer` | Role: `admin`, `deployer`, or `viewer` |
| `--expires` | No | `48h` | Expiry: `24h`, `48h`, `7d` |

Invites are tied to the supplied email address. The CLI prints a link you can share even when email delivery is unconfigured.

Create the account with an appropriate cluster role, then assign access to individual projects. For a project deployer:

```bash
kip user invite --email jordan@example.com --role viewer
# After Jordan accepts the invitation:
kip project members add acme-shop jordan@example.com deployer
```

Project access depends on membership; cluster admins have access to every project.

See [Team Access](/en/team-access) for full documentation.

## kip 2fa {#kip-2fa}

Manage two-factor authentication for console users. Destructive operations, meaning starting a cluster migration and applying its cutover, require a TOTP code on top of the admin login, and enrolling a factor requires a one-time code that only these host-level commands can issue.

```bash
kip 2fa bootstrap admin@example.com    # issue a one-time enrollment code
kip 2fa remove admin@example.com       # remove a factor (lost phone, no recovery codes)
```

### kip 2fa bootstrap {#kip-2fa-bootstrap}

Issues a single-use enrollment code, valid for 15 minutes:

```bash
kip 2fa bootstrap admin@example.com

  ✔  Enrollment code for admin@example.com

     K7QT-M3XP-9WLC-R2VD

  Valid for 15 minutes, single-use.
  Enter it in Console → Settings → Two-factor authentication to enroll.
```

The user enters the code in **Console → Settings → Two-factor authentication**, scans the QR code with an authenticator app, and confirms. Issuing a new code for the same email replaces any unused one. Enrollment is gated on this code because it can only be issued with kubeconfig access; a stolen console login alone can never enroll a device.

### kip 2fa remove {#kip-2fa-remove}

Deletes a user's enrolled factor, leaving the account unenrolled. This is the recovery path when the phone is gone and no recovery codes are left. Re-enrollment needs a fresh bootstrap code, and the new factor waits the full eligibility period (7 days by default) before it can authorize a migration.

See [Cluster Migration](/en/migration#two-factor-authentication) for the full 2FA and migration security model.

## kip registry {#kip-registry}

Manage container registry credentials. A credential is stored once in `kipper-system` and carries an allow-list of projects. When a workload in an allowed project runs an image from that registry, Kipper stages a pull secret in the workload's namespace, scoped to that single registry. Build containers run user code, so they carry no registry credentials and pull base images anonymously.

```bash
kip registry add --server ghcr.io --username myuser --password ghp_token123 --allow-project acme
kip registry add --server registry.git.example.com --username deploy --password secret --allow-project acme
kip registry allow ghcr-io --project shop      # adds shop, keeps acme
kip registry revoke ghcr-io --project shop     # removes shop, keeps the rest
kip registry list
kip registry remove ghcr-io
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--server` | Yes | — | Registry server (e.g. `ghcr.io`, `registry.git.example.com`) |
| `--username` | For a new registry | — | Registry username or token name |
| `--password` | For a new registry | — | Registry password or access token |
| `--name` | No | auto-generated | Credential name (derived from the server's host if omitted) |
| `--allow-project` | No | none | Project allowed to pull with this credential (repeatable; replaces the allow-list) |

A credential is used only by projects on its allow-list, so grant at least one. Re-running `kip registry add` for an existing name updates just the flags you pass, so granting a project keeps the stored password.

`--allow-project` **replaces** the allow-list. Naming one project takes every other away, which is what it has always done; it now prints what it removed so you can see it happen. To add a project without disturbing the others, use `kip registry allow`, and to take one away use `kip registry revoke`:

```bash
kip registry allow ghcr-io --project shop --project blog
kip registry revoke ghcr-io --project blog
```

`allow` checks that the project exists and refuses a name the cluster does not have, since a project name is matched exactly at pull time and a typo would be stored as a grant that can never work. `revoke` takes any name, because it is also how you remove one that should never have been there.

Pointing an existing credential at a different registry needs `--password`. The credential is addressed by its name, so changing the server would otherwise hand the new registry the password stored for the old one.

The console's registry settings edit the password and the server. Who may pull with a credential is changed with the commands above, and the settings API refuses a request that would change the allow-list on a credential that already exists.

When a workload in a granted project runs an image from that registry, the credential is staged into the workload's namespace as a pull secret, where the project's members can read it. Grant a credential only to projects that may share that registry login. When projects must stay isolated, create a separate registry account per project and add each as its own credential; a project's workloads then use the credential granted to them.

## kip credentials {#kip-credentials}

Read back the git and container-registry credentials stored in the cluster. Useful if you no longer have a copy of a token and need to reuse it somewhere else, such as another cluster or a CI pipeline.

```bash
kip credentials list                          # masked overview, both types, with allowed projects
kip credentials list --type git               # masked overview, git only
kip credentials get git-acme-tools            # plaintext token to stdout
kip credentials get ghcr-io --type registry   # disambiguate if names collide
kip credentials get --app blog --project acme --environment prod   # an app's own git token
kip credentials allow git-acme-tools --project acme    # let a project build with it
kip credentials revoke git-acme-tools --project acme   # stop it building with it
```

### Granting a project {#granting-a-project}

A shared git credential is usable only by the projects on its allow-list, so a new one builds nothing until you allow a project. Granting never asks for the token: it changes who may use the credential, not what it is.

```bash
kip credentials allow git-acme-tools --project acme
kip credentials allow git-acme-tools --project acme --project blog
kip credentials revoke git-acme-tools --project blog
```

A build refused for want of a grant says so and names the command:

```
git credential "git-acme-tools" is not allowed for project "acme". Allow it with 'kip credentials allow git-acme-tools --project acme'
```

Revoking leaves running apps alone. They keep the image they have, and the next build for that project is refused.

Container registry credentials have their own allow-list, granted with `kip registry add --allow-project` as described above. That flag replaces the list rather than adding to it, so name every project that should keep access.

The console's credential settings edit the token and the server. Who may build with an existing credential is changed with the commands above; `allow` checks that the project exists, and `revoke` takes any name, since it is also how you remove one that should never have been there. The settings API accepts a credential's allow-list when creating it, and refuses a request that would change the list on one that is already there.

`kip credentials list` shows each credential's allowed projects, so you can check a grant landed where you meant it.

On clusters that predate credential allow-lists, `kip upgrade` previews the projects whose apps reference each undecided shared credential. Accept the prompt to grant those pairs, or decline and grant access later with `kip credentials allow`. The migration runs once per cluster.

For automation, `--seed-credential-grants` accepts the previewed grants. A run with no terminal grants nothing unless that flag is set.

Rotate or replace shared credentials before or after an upgrade. If a credential changes during the upgrade, Kipper reports it and leaves its grants for you to review. After a rollback to a version older than 0.14, check `kip credentials list` and restore missing grants explicitly.

A token configured per app (under the app's Git settings) lives in a secret in the app's namespace, separate from the named credentials above. It does not show up in `kip credentials list` or the global credentials screen. To read it back, pass `--app` with the project and environment instead of a credential name. Kipper finds the secret the app references for its git source and prints that token.

The console masks credential values by default. To reveal one in the browser, click the eye icon next to a credential in Settings and re-enter your password. On the CLI, `kip credentials get` prints plaintext to stdout without prompting, so make sure nothing is watching your terminal.

CLI credential reads require Kubernetes permission to read the relevant Secret. Named credentials live in `kipper-system`; an app’s own token lives in its project namespace.

| Flag | Required | Default | Description |
|---|---|---|---|
| `--type` | No | — | Restrict or disambiguate: `git` or `registry` |

## kip backup {#kip-backup}

Create, list, and restore cluster backups using Velero.

```bash
kip backup create                                                   # backup user namespaces (system namespaces excluded by default)
kip backup create weekly --project blog                       # backup one project
kip backup create --project blog --environment prod --ttl 720h  # 30-day retention
kip backup create everything --include-system                       # also back up system namespaces
kip backup list
kip backup restore weekly-20260413
kip backup restore weekly-20260413 --namespace blog-prod      # restore one namespace
kip backup schedules                                                # list scheduled backups
kip backup storage resize --size 30Gi                               # grow the in-cluster MinIO volume
kip backup repository-key export --to ~/example-backups.key         # save the key that encrypts the backups
kip backup drill blog-prod                                          # restore blog-prod's services into an isolated namespace and count their rows
kip backup sse-key export --to ~/example-backups-sse.key            # save the SSE-C key, on clusters installed with --backup-storage-sse-c
kip backup mirror add uploads --source s3://... --target s3://... ...  # copy an S3 bucket daily into a locked bucket
kip backup mirror status uploads                                    # last run, preflight result and copy totals
kip backup mirror remove uploads                                    # stop the mirror. Both buckets stay as they are
```

`kip backup repository-key export` saves the volume backup encryption key to a new file with mode 0600. The command asks for confirmation, which you can skip with `--yes`. On Windows, the file inherits its folder's permissions. It never overwrites an existing file. Keep the key outside the cluster because a recovery install needs it to read the volume backups. See [The backup repository key](/en/installation#backup-repository-key).

`kip backup sse-key export` does the same for the SSE-C key of a cluster installed with `--backup-storage-sse-c`, writing it as base64 text that `--backup-storage-sse-c-key-file` reads back. See [SSE-C](/en/installation#backup-sse-c).

`kip backup storage resize` grows the in-cluster MinIO backup volume and waits up to five minutes for it to report the requested capacity. Run it again with the same size to resume waiting. The size must be at least the volume's current storage request. Manage external backup storage through its provider.

A default `kip backup create` excludes system namespaces (`kube-system`, `kube-public`, `kube-node-lease`, `traefik`, `longhorn-system`, `keda`, `monitoring`, `velero`). The daily schedule uses the same exclusion. Without it, Velero would try to back up its own MinIO PVC and the backup would hang. Pass `--include-system` if you really want a backup of those namespaces too.

Every backup also skips cert-manager's transient issuance objects (CertificateRequests, Orders, Challenges), including when `--include-system` is set. cert-manager recreates them on demand, and restoring them stops certificates renewing.

### kip backup mirror {#kip-backup-mirror}

`kip backup mirror add <name>` copies an S3 bucket once per schedule into a bucket at another endpoint that is versioned and carries an Object Lock default retention in compliance mode. It first runs the mirror's checks against both buckets from your machine and creates cluster resources only after all checks pass: the endpoints differ, source versioning is enabled, the target's default retention is `COMPLIANCE` and at least `--retention-days`, a canary written to the target is locked and cannot be deleted by version, and the source key is denied writing and deleting the canary key in the source. It then creates the Secret `backup-mirror-<name>` holding both keys and a CronJob of the same name in the `velero` namespace, then attempts to add the Prometheus alert `KipperBackupMirrorStale`. Each run repeats the checks, then runs `rclone sync` and `rclone check`.

```bash
kip backup mirror add uploads \
  --source s3://eu-1.objects.example.com/app-uploads --source-region eu-1 \
  --target s3://eu-2.objects.example.com/app-uploads-backup --target-region eu-2 \
  --source-credentials ~/.aws/mirror --target-credentials ~/.aws/mirror \
  --retention-days 30
kip backup mirror status [name]
kip backup mirror remove uploads
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--source`, `--target` | Yes | — | Buckets as `s3://endpoint/bucket` |
| `--source-region`, `--target-region` | Yes | — | Region as the provider names it |
| `--source-credentials`, `--target-credentials` | Yes | — | AWS-format credentials files. The same file may serve both sides |
| `--source-profile`, `--target-profile` | No | `default` | Profile within each credentials file |
| `--retention-days` | Yes | — | Minimum default retention of the target bucket, in days |
| `--schedule` | No | `0 3 * * *` | Cron schedule, in the cluster's time zone |
| `--max-delete` | No | `-1` (off) | Maximum objects one run may delete from the target. Exceeding it fails the run without a retry |
| `--image-tag` | No | — | Run the mirror image built from this commit (full 40-character SHA) |

`status` shows the schedule, the last run and its outcome, the time since the last success, the preflight result and the last sync's totals, for one mirror or all of them. `remove` deletes the CronJob, the Secret and the alert and leaves both buckets untouched. To change a mirror, remove it and add it again. See [Mirror an S3 bucket into a locked bucket](/en/backups#backup-mirror) for the bucket setup and the restore procedure.

### kip backup create flags {#kip-backup-create-flags}

| Flag | Required | Default | Description |
|---|---|---|---|
| `--project` | No | — | Backup a specific project only |
| `--environment` | No | — | Backup a specific environment only |
| `--ttl` | No | `168h` (7 days) | Backup retention period |
| `--include-system` | No | `false` | Include system namespaces (kube-*, traefik, longhorn-system, monitoring, keda, velero). Off by default because Velero recurses into its own MinIO PVC otherwise. |

### kip backup restore flags {#kip-backup-restore-flags}

| Flag | Required | Default | Description |
|---|---|---|---|
| `--namespace` | No | — | Restore only a specific namespace |
| `--namespace-mapping` | No | — | Restore only the source namespace into the target namespace (format: `source:target`). If set, `--namespace` must match the source. |
| `--resources` | No | — | Restore only specific resource types (comma-separated) |
| `--allow-partial` | No | `false` | Restore a `PartiallyFailed` backup despite potentially missing data or resources |
| `--repair-credentials` | No | `false` | Re-attach restored service credentials Secrets without prompting |
| `--cluster-resources` | No | `false` | Also restore projects and their resource adjustments. Existing ones remain unchanged. |
| `--include-ingresses` | No | `false` | With `--namespace-mapping`, also restore the copy's Ingresses, which claim the same hostnames as the source |
| `--cancel` | No | — | Cancel the named restore while it is `InProgress` instead of starting one. Takes no backup name. See [Cancel a stuck restore](/en/backups#cancel-a-stuck-restore). |
| `--force` | No | `false` | With `--cancel`, allow a Velero restart that also fails `InProgress` backups and other restores |

Restore a `Completed` backup, or use `--allow-partial` for a `PartiallyFailed` backup. For partial backups, kip lists volumes with failed or canceled backups when details are available. Backups in other phases cannot be restored; wait for pending or running backups to finish.

`kip backup restore` waits up to four hours for the restore to finish. After a `Completed` or `PartiallyFailed` restore, it checks the restored service credentials Secrets (see [Restore progress and credentials](/en/backups#restore-progress-and-credentials)). Press Ctrl-C to stop waiting; the restore continues in the cluster. The command returns an error for `Failed`, `FailedValidation`, or `PartiallyFailed` restores, even if credential repair succeeds.

## kip platform {#kip-platform}

Manage system component sizing, including the observability stack (Prometheus, Grafana, Loki). See [Platform Resources](/en/platform-resources) for the full reference.

```bash
kip platform status                          # active profile + per-component state
kip platform resize prometheus --memory 2Gi  # set a manual memory override
kip platform disable loki                    # turn a component off
kip platform enable loki                     # turn it back on
kip platform restart prometheus              # rolling restart
kip platform profile show                    # current profile
kip platform profile set large               # change profile
kip platform tuning show                     # active resource tuning mode
kip platform tuning expert                   # stop automatic resource changes
kip platform tuning auto                     # resume automatic tuning
```

## kip blueprint {#kip-blueprint}

Browse and install application blueprints. Blueprints are pre-built templates for common application stacks.

```bash
kip blueprint list
kip blueprint info wordpress
kip blueprint install wordpress --project myblog --set domain=blog.example.com
```

### kip blueprint install flags {#kip-blueprint-install-flags}

| Flag | Required | Default | Description |
|---|---|---|---|
| `--project` | No | — | Project name (overrides template) |
| `--environment` | No | — | Target environment |
| `--set` | No | — | Parameter values (`key=value`, repeatable) |

## kip apply {#kip-apply}

Apply a `kipper.yaml` manifest to the cluster. This is the declarative way to manage Kipper resources: the manifest is the desired spec, and on update a field left out of it is cleared.

```bash
kip apply                                         # apply ./kipper.yaml
kip apply -f myapp.yaml                           # apply a specific file
kip apply -f kipper/                              # apply all manifests in a directory
kip apply --dry-run                               # preview changes without applying
kip apply --project blog --environment prod  # override project/environment
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `-f`, `--file` | No | `kipper.yaml` | Path to manifest file or directory |
| `--dry-run` | No | `false` | Print what would be applied without making changes |
| `--project` | No | from manifest | Override the project name |
| `--environment` | No | from manifest | Override the environment |

## kip diff {#kip-diff}

Show differences between a `kipper.yaml` manifest and the live cluster state. Useful for reviewing changes before applying.

```bash
kip diff                               # diff ./kipper.yaml against live state
kip diff -f myapp.yaml                 # diff a specific file
kip diff --project blog          # override project
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `-f`, `--file` | No | `kipper.yaml` | Path to manifest file or directory |
| `--project` | No | from manifest | Override the project name |
| `--environment` | No | from manifest | Override the environment |

## kip export {#kip-export}

Export the current cluster state as a `kipper.yaml` manifest. Useful for backing up configuration or migrating between clusters.

```bash
kip export --project blog                                # export to stdout
kip export --project blog -o kipper.yaml                 # export to file
kip export --project blog --split -o blog-exports  # export every env, one file per env, into a directory
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--project` | Yes | — | Project name |
| `--environment` | No | — | Export a specific environment only. Mutually exclusive with `--split`. |
| `-o`, `--output` | No | stdout | Output file. With `--split`, this is the output directory. |
| `--split` | No | `false` | Read the project's `spec.environments` and write one manifest per env into `--output`. The directory is created if it does not exist. |

## kip init {#kip-init}

Generate a `kipper.yaml` manifest from a blueprint template.

```bash
kip init --blueprint wordpress --set domain=blog.example.com
kip init --blueprint nodejs -o kipper.yaml
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--blueprint` | Yes | — | Blueprint name to use as template |
| `--set` | No | — | Parameter values (`key=value`, repeatable) |
| `-o`, `--output` | No | `kipper.yaml` | Output file |
