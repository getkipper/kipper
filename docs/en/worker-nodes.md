---
title: 'Add worker nodes to a Kubernetes cluster'
description: 'Add worker servers to a Kipper cluster over a private network or public addresses. Learn the firewall requirements and limitations before you start.'
---

# Add Worker Nodes

A Kipper cluster starts with one server. When apps, functions, jobs and services need more CPU and memory than that server has, join more servers as workers with `kip node add`. kip prepares each host, joins it to the cluster and checks it before ordinary new workloads are scheduled there.

The first server stays the cluster's only control plane and ingress node, and Kipper schedules database, registry and in-cluster backup volumes on it. Workers add capacity, so the cluster still depends on the first server for availability.

## Before you start

- **The server's pod network runs WireGuard.** Clusters installed before multi-node support use VXLAN until a full `kip upgrade` switches them to WireGuard. If the upgrade warns that it kept VXLAN because the server cannot load WireGuard, fix that first. See [Upgrades & Maintenance](/en/maintenance).
- **Each new host runs a supported OS** with an IPv4 address and the `wireguard` kernel module, like the server. See [preflight checks](/en/installation#preflight-checks).
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

- **Private mode** when the hosts share a private IPv4 network, such as a Hetzner Cloud network or a Hetzner vSwitch. kip identifies it from the hosts' routing tables, then checks connectivity by connecting from the server to the new host's SSH port and from the new host to the server's API on port 6443 over that network. If the private network carries the default route, specify it with `--private-network 10.0.0.0/16`. A private network that overlaps the cluster's pod or Service network (10.42.0.0/16 and 10.43.0.0/16 by default) is refused.
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

Services that ran on a lost worker may stay unavailable until it returns. Losing the server makes the cluster unavailable. Recovering from the loss of its disk requires a surviving backup. `kip service info <name>` shows where a service runs and where its data is stored.

## Looking after workers

- **Reboots:** [`kip node reboot <worker>`](/en/cli-reference#kip-node-reboot) takes a backup, drains the worker, reboots it and waits for previously ready workloads to become ready again. The CLI reference lists when it cancels and what a drain removes.
- **Certificates:** a worker's agent certificates last a year and are renewed whenever its agent starts while they are still valid, so reboot each worker at least once a year. A worker whose certificates have expired or are missing shows `NotReady`. Run `kip node add <host>` again with the original options to rejoin it.
- **Listing nodes:** `kip node list` shows every node with its role, status and address.

- **k3s upgrades:** rerunning `kip install` upgrades k3s on the server, then brings each worker recorded as `ready` to the same version, one at a time and without a drain. See [k3s upgrades](/en/maintenance#k3s-upgrades).

`kip upgrade` acts on the server's host only. For what [`kip cluster uninstall`](/en/cli-reference#kip-cluster-uninstall) does with the workers, and for other worker maintenance, see [Maintenance](/en/cli-reference#kip-node-add) in the CLI reference.
