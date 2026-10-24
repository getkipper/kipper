---
title: 'Add worker nodes to a Kubernetes cluster'
description: 'Add worker servers to a Kipper cluster over a private network or public addresses. Learn the firewall requirements and limitations before you start.'
---

# Add Worker Nodes

A Kipper cluster starts with one server. When apps, functions, jobs and services need more CPU and memory than that server has, join more servers as workers with `kip node add`. kip prepares each host, joins it to the cluster and checks it before ordinary new workloads are scheduled there.

The first server stays the cluster's only control plane and ingress node, and Kipper schedules database, registry and in-cluster backup volumes on it. Workers add capacity, so the cluster still depends on the first server for availability.

## Before you start

- **The server's pod network runs WireGuard.** Clusters installed before multi-node support use VXLAN until a full `kip upgrade` switches them to WireGuard. If the upgrade warns that it kept VXLAN because the server cannot load WireGuard, fix that first. See [Upgrades & Maintenance](/en/maintenance).
- **Each new host runs a supported OS** with an IPv4 address and the `wireguard` kernel module, like the server. See [preflight checks](/en/installation#preflight-checks). Workers hold Longhorn volumes, so kip refuses Rocky Linux or AlmaLinux 9.7 or a later 9.x until a Kipper release adopts a fixed Longhorn (see [Longhorn upgrades](/en/maintenance#longhorn-upgrades)).
- **Your SSH key logs in to every node the way it logs in to the server:** the new host and every worker already in the cluster. Each join needs this access to update their firewall rules. Pass `--ssh-key` to choose the key.
- **Every existing worker is recorded.** kip refuses to add a new node while the cluster has an unrecorded worker. Run `kip node add <host>` for that worker first to adopt it.
- **The provider's firewall allows the traffic between the nodes.** See [Firewalls](#firewalls) below.

## Join a worker

```bash
kip node add 203.0.113.11
```

kip checks the host, then prepares its kernel modules, kubelet hardening, Longhorn storage packages, host hardening and firewall. It joins the host as a k3s worker on the server's k3s version and keeps ordinary new workloads off it with a taint while it checks DNS, connectivity to the server, Longhorn storage, the cluster registry and kubelet access. Longhorn's own pods, the validation test pod and any pod that tolerates the taint can run there during the checks.

Rejoining a node that is already in the cluster cordons it for the checks but leaves its running workloads in place. If a join is interrupted, run the same command again with the same options to resume it.

## Private network or public addresses

kip picks the network the nodes use to talk to each other:

- **Private mode** when the hosts share a private IPv4 network, such as a Hetzner Cloud network or a Hetzner vSwitch. kip identifies it from the hosts' routing tables, then checks connectivity by connecting from the server to the new host's SSH port and from the new host to the server's API on port 6443 over that network. If the private network carries the default route, as can happen for cloud VMs in the same VPC, specify it with `--private-network 10.0.0.0/16`. A worker already recorded in a private-mode cluster can rejoin or be added again without `--private-network`. If automatic selection finds no private path, kip checks the path between the server's and worker's recorded private addresses and reuses it if the routing, connectivity and network overlap checks pass. On the first join, if no other private route qualifies and you pass neither `--private-network` nor `--public`, kip checks this network for private connectivity and network overlap. If it passes, kip stops before the host joins and shows the `--private-network` value to use. On a cluster with an [operator access list](/en/cli-reference#kip-cluster-access), the server continues to allow connections from the host's addresses on port 6443 until the next `kip cluster access apply` or reboot. A private network that overlaps the cluster's pod or Service network (10.42.0.0/16 and 10.43.0.0/16 by default) is refused.
- **Public mode** when there is no shared private network, or when you pass `--public`. The nodes then reach each other over their public addresses, so hosts at different providers can join one cluster.

When kip finds a private route but one of its connection checks fails, the join stops and identifies the failed connection. Open the port, or pass `--public` to join over public addresses.

The first join sets the mode for the whole cluster, and every later node uses that mode.

Moving the server into the chosen mode restarts k3s on it: running containers keep running. The Kubernetes API, kip commands, console actions and apps that call the API are unavailable during the restart, usually for 10 to 30 seconds and longer on small or busy servers. kip asks for confirmation before restarting k3s. If its recovery checks fail, it attempts to restore the previous configuration. Pass `--yes` to approve it without a prompt. On Rocky Linux, AlmaLinux and CentOS Stream, applying kip's firewalld rules also restarts k3s, so a later join can interrupt the API briefly too.

## What is encrypted

Traffic between pods on different nodes travels inside WireGuard, in both modes. Traffic between the hosts themselves does not, but the kubelet API on port 10250 uses TLS, and Prometheus reads node metrics on port 9100 over TLS with a token.

## Firewalls

When kip manages the server's firewall, it sets up a host firewall on each new node too, using UFW or firewalld depending on the node's OS, and opens these ports only to the other nodes' addresses (their private addresses in private mode, their public ones in public mode):

| Port | From |
|---|---|
| 51820/udp (WireGuard) | every other node |
| 10250/tcp (kubelet) | every other node |
| 9100/tcp (node metrics) | the server |

On a worker, SSH is the only port kip opens to every source. `--firewall` and `--firewall=false` override the default for the new node, and kip still updates its own peer rules on the nodes whose firewall it already manages. A UFW firewall that kip does not manage on the server or an existing worker stops the join, even with the ports open. On a node whose firewall you run yourself, allow the ports above from those sources only, keep 8888/tcp closed to outside traffic, and limit access to any NodePort services.

A cloud provider's firewall or security group must allow the same traffic, plus SSH from the server to the new host in private mode. Open 51820/udp and 10250/tcp in both directions between the nodes, 9100/tcp from the server to each worker, and 6443/tcp from each worker to the server. Open both directions even when a join passes without them: a stateful security group can let a join through while an inbound rule is missing, and the tunnel can then fail later. See the [CLI reference](/en/cli-reference#kip-node-add) for details.

## Data and availability

Kipper configures each database, registry and in-cluster backup volume to keep one copy on the server, and `kip status` and `kip node add` list any copies still on a worker. A volume whose only copy is on a worker can be lost with that worker, so move such copies before relying on the worker. [`kip node reboot <worker>`](/en/cli-reference#kip-node-reboot) moves a pinned copy to the server when the server has room for it, and otherwise refuses the reboot.

A copy reported as not pinned can move to another worker during a reboot instead: run `kip upgrade` to pin it first.

Single-copy volumes are stored on the server, so adding workers leaves database storage capacity unchanged. Longhorn excludes part of the server's disk from scheduling. By default, 30% of the default disk is reserved, and more than 25% must remain available to schedule a new replica, so scheduling capacity runs out before the disk is full. `kip service add`, `kip apply` and `kip service update --consistent-backups` refuse new volumes when the server's disks have a definite capacity shortage. `kip status` warns when the roomiest eligible disk has less than 5Gi available for scheduling. See [Stateful Services](/en/services#options).

Services that ran on a lost worker stay unavailable until the worker returns or you fence it, as [When a node dies](#when-a-node-dies) describes. Losing the server makes the cluster unavailable. Recovering from the loss of its disk requires a surviving backup. `kip service info <name>` shows where a service runs and where its data is stored.

## When a node dies

When a worker stops reporting, Kubernetes keeps its pods and volumes where they were. It cannot tell a dead host from one that lost its network and may still be writing to its volumes, so Kipper moves nothing on its own. `kip status` lists the services stranded on the worker with the nodes that hold a current copy of their data, and the console's [NotReady alert](/en/alerts) names them along with the command to run.

Once the worker has been NotReady for at least a minute and you know it is down, run [`kip node fence <worker>`](/en/cli-reference#kip-node-fence):

1. kip records the fence before it touches anything, so an interrupted fence resumes when you run the command again.
2. If the worker still answers over SSH, kip installs a systemd drop-in that keeps its k3s agent from starting, even after a reboot. It then stops k3s and every container on the host and checks that no container, Longhorn process or Longhorn mount is left. If the worker does not answer, kip asks you to confirm that the host is powered off and stays off until you rejoin it, and it refuses while the worker's kubelet still answers. The server checks the kubelet at the worker's recorded address, and kip refuses when that address does not resolve to an IPv4 address or the check does not run. `--powered-off` gives that confirmation without a prompt.
3. kip checks each service with a single-node volume on the worker. It lets the service start on another node only when another node holds a current copy of every one of its volumes, which means a copy that has been healthy since it last failed. Otherwise kip stops the service. A service whose other copies may be older stays stopped until you accept the older data: kip shows every copy with its timestamps and asks, or you pass `--accept-stale-data`. A service whose only copy is on the worker stays stopped until the worker rejoins. If the worker is lost, delete the service with `kip service delete <name> --delete-data` and then restore it with `kip backup restore <backup> --namespace <namespace>`, because a restore skips objects that still exist. Use a backup taken before the fence, while the service ran. A later backup holds none of its files and restores the service stopped. `--namespace-mapping` restores it beside the old one instead. A service paused by a migration whose data transfer runs on the worker is checked the same way. kip then marks the transfer failed and suspends Jobs whose pods hold single-node volumes on the worker to prevent those pods from restarting. A migration keeps a service stopped while it carries the fence stop annotation. These checks apply only to Kipper workloads. kip warns about other pods with single-node volumes on the worker, such as workloads installed with kubectl or Helm. The taint lets Kubernetes start them using whichever copy Longhorn has available.
4. kip taints the node `node.kubernetes.io/out-of-service`, and Kubernetes deletes its pods and detaches their volumes. After a minute, kip deletes remaining Kipper pods with Longhorn volumes, but only while the node is still NotReady and tainted and their volume attachments have been removed.
5. kip reports where each service runs again, and which ones stay stopped and why.

The worker then stays fenced until you decide what happens to it:

- **Bring it back:** run `kip node add <host>`. kip reboots the host unless it has booted since the fence, lifts the fence step by step, runs the usual checks and starts the services the fence stopped. In private mode, if a worker responds at an address different from the one its node publishes, kip records the new address. This can happen, for example, when a cloud provider assigns a new public IP. After the rejoin, kip runs the add process again so the node publishes the new address. In public mode, the other nodes reach the worker at its old address, so kip refuses to rejoin it at the new address. A service whose copies are not known to be current starts only after you accept the older data.
- **Give it up:** run `kip node remove <worker> --force`. When the host still answers over SSH, kip removes it only while the fence still holds it stopped.
- **Start a stopped service on its own:** [`kip service start <name>`](/en/cli-reference#kip-service-start) starts a service the fence left stopped, with the same check and consent.

While a worker is fenced, `kip node reboot` and `kip node remove` without `--force` refuse it, k3s upgrades leave it alone, and `kip status` shows it as `OutOfService`. A fenced node never counts as Ready, even when its host is running. A powered-off worker cannot be reached either, so commands that update every node, such as adding another worker, stop at it until you rejoin or remove it.

No kip command and no part of the console force-deletes a pod without a recorded fence. Two things outside Kipper act without one:

- Longhorn moves the share manager of a shared volume to another node when its node goes down, so apps and functions that use a [shared volume](/en/shared-storage) recover without a fence. Apps and functions mount only shared volumes. Only stateful services hold single-node volumes.
- Kubernetes can force-detach a volume after a six-minute pod deletion timeout if the node is unhealthy, regardless of what requested deletion. Deleting a dead node's pod with `kubectl delete pod --force` can therefore release its volume for another node without any of the fence's checks, while the old host may still be writing to it. Use `kip node fence`, never `kubectl delete pod --force`.

Some platforms, such as Harvester, taint a node out of service on a timer. Kipper waits for you instead, because a timer cannot tell a dead host from one that is only cut off.

## Two copies of a service's data

A service can keep two copies of its data, each on a different node, so the data survives the loss of either node's disk. Two copies are opt-in, chosen when the service is created or later on a running service:

```bash
kip cluster storage --max-copies 2           # an admin offers two copies, once
kip service add postgres --name db --copies 2
kip service update cache --copies 2 --wait   # convert a running service
```

The server is always eligible to hold copies. A worker becomes eligible after its disk passes the check that `kip node add` runs at the end of each join unless `--no-copies` is set. [`kip node storage <worker> --copies`](/en/cli-reference#kip-node-storage) repeats the check. The check measures how long the disk takes to sync small writes, because a replica on a slow disk stalls every write to the volume. Local SSD and NVMe disks pass. Network block storage mounted at `/var/lib/longhorn` is usually refused. `--ignore-slow-disk` can accept it and records the override, except when a sync stall reaches half of Longhorn's replica timeout. `kip node list` shows which nodes can hold copies.

`kip service add --copies 2` requires two ready nodes that can hold copies. If a service reaches the controller before two eligible nodes are ready, creation of its data volume waits, and `kip service info` explains why. A restore from a backup goes ahead with the nodes that are back, and Longhorn adds the second copy when a node can take it.

- **Shared volumes** keep one copy in this release.
- **Existing services and volumes** have their copies unset, and Kipper leaves their storage exactly as it is. `kip status` lists them with the copies Longhorn keeps, and `kip service info` shows the command that sets them. Shared volumes and project shared storage created by an earlier Kipper release keep Longhorn's three copies until copies are set. Setting them converts the volume to that number.
- **Who decides:** an admin sets the offer, a deployer chooses one or two copies when creating a service, and changing copies on an existing object needs the project owner role.
- **Backups** stay necessary: two copies protect against a lost node, and only backups outside the cluster protect against a lost cluster.

### Changing the copies of a running service

`kip service update <name> --copies 1|2` and `kip volume update <name> --copies 1` change the copies of a volume that is in use, with no restart. The console-api controller converts the volume one step per pass and reports each step in `kip service info`, `kip volume list` and `kip status`. `--wait` follows the conversion until every copy is healthy.

- **One to two:** Longhorn copies the volume to a second node that can hold copies. The controller first checks that two such nodes have room for the volume. Kipper starts one build at a time across the cluster, and the others wait their turn until that build finishes or exceeds its time budget. Longhorn may continue an earlier build after its budget expires.
- **Two to one:** the copy on the server stays. The controller turns off data locality and auto-balance, lowers the copy count, then removes the other copy. It never deletes a copy on the server, and it removes a worker's copy only after checking again that the server's copy is healthy. It refuses to remove anything while the server's copy is unhealthy or not in the running engine, while a copy is being evicted, or while the volume is detached. A volume with no copy on the server gets one first, so a reduction never leaves a volume without a server copy.
- **Three to two** (a volume from an earlier release): a server copy is built first when there is none, and the extra copies are removed one at a time.
- **While Longhorn builds a copy**, the copies cannot change again. The build has a time budget of 30 minutes plus one minute per GiB. Past it, the conversion reports `Blocked` and Longhorn keeps trying. Setting the copies back stops it.
- **Detached volumes:** Longhorn 1.9 and later build the new copy of a detached volume by attaching it for the build. On Longhorn 1.7 and 1.8 a detached volume waits until an app attaches it. Removing a copy always waits for the volume to be attached.
- **When conversion is blocked**, the service shows `Blocked` with the reason, for example too little room on the server, and keeps every copy. A failed step, such as a write the API server refuses, reports `Blocked` with the error. Conflicts, timeouts and throttled requests are retried. A copy that Longhorn recovered after a failure without rebuilding it also blocks the change, because Kipper cannot tell whether that copy is current. The command exits non-zero. The conversion resumes on its own once the reason clears, and setting the copies back stops it.
- **Longhorn upgrades:** `kip upgrade --longhorn` refuses an upgrade step while a volume has more replicas without a failure marker than its requested copy count. An unfinished reduction can leave these extra replicas behind.

Kipper departs from Longhorn's defaults here:

- One copy by default, at most two.
- Data locality is off, so a copy stays on its node when the pod moves.
- Replica auto-balance is off, so Longhorn leaves healthy copies in place instead of moving them to balance storage across nodes.
- A volume can be created with one copy missing, so a restore never waits for a node.

## Looking after workers

- **Reboots:** [`kip node reboot <worker>`](/en/cli-reference#kip-node-reboot) takes a backup, drains the worker, reboots it and waits for previously ready workloads to become ready again. The CLI reference lists when it cancels and what a drain removes.
- **Certificates:** a worker's agent certificates last a year and are renewed whenever its agent starts while they are still valid, so reboot each worker at least once a year. A worker whose certificates have expired or are missing shows `NotReady`. Run `kip node add <host>` again with the original options to rejoin it.
- **Removing a worker:** [`kip node remove <worker>`](/en/cli-reference#kip-node-remove) moves volume copies off the worker, then drains and wipes it and removes it from the cluster. It also removes the worker's address from the other nodes' kip-managed firewalls. On a node with an operator-managed firewall, kip lists the worker's address and ports so that you can remove any rules that still allow it access. `--force` deregisters a worker whose host is powered off or gone for good. For a worker that stopped reporting, fence it first, as [When a node dies](#when-a-node-dies) describes.
- **Listing nodes:** `kip node list` shows every node with its role, status and address. When a worker is recorded, it also shows each node's network mode, whether it can hold copies of volumes, and its SSH address.
- **Health:** `kip status` runs its host checks on the server and every recorded worker, and names a worker it could not reach. `kip app info` and `kip function info` name the nodes a workload's pods run on, and `kip service info` shows the node a service runs on and where its data is.

- **k3s upgrades:** rerunning `kip install` upgrades k3s on the server, then brings each worker recorded as `ready` to the same version, one at a time and without a drain. See [k3s upgrades](/en/maintenance#k3s-upgrades).

`kip upgrade` acts on the server's host only. For what [`kip cluster uninstall`](/en/cli-reference#kip-cluster-uninstall) does with the workers, and for other worker maintenance, see [Maintenance](/en/cli-reference#kip-node-add) in the CLI reference.
