---
title: 'Start small with a nano Kubernetes cluster'
description: 'Run a full Kipper cluster on a 4 GB server with local storage, then grow it onto Longhorn in place when you need more.'
---

# Start Small with Nano

Kubernetes has a reputation for needing big servers and a lot of setup. With minimal setup, nano gives you a full Kubernetes cluster with Kipper's console, builds, services and functions on a server with 4 GB of RAM and 20 GiB of free disk space. It suits anyone who wants to try Kipper, run a prototype, test an idea or give a QA team its own cluster, and it is a real cluster rather than a demo mode.

When you need more, you resize the server and switch the same cluster to Longhorn in place, then add nodes. You can also install a larger cluster and move your projects to it with [Cluster Migration](/en/migration).

## What makes a cluster nano

`kip install` picks the `nano` profile on servers with less than 6000 MiB of RAM, or when you pass `--profile nano`. By default a new nano cluster keeps its footprint small in three ways:

- **Local storage.** Volumes live on the server's own disk through the local-path provisioner that k3s ships with. Omitting Longhorn and its iSCSI packages saves about 350 MiB of memory and about 850 MB of images. The registry's volume also uses local storage, so builds and images share the server's disk.
- **No monitoring by default.** Prometheus, Loki and Grafana stay off. Turn them on later with `kip platform enable prometheus` and `kip platform enable loki` if the server has room.
- **Backups to external storage only.** A cluster on local storage omits Velero and MinIO unless you supply `--backup-storage-bucket`, because a backup on the same disk would be lost if the disk failed. Add [external backup storage](/en/backups#external-s3-compatible-storage) when you want backups.

For now, nano blocks `kip backup drill` because running a second copy of a namespace's services exceeds a small server's memory capacity. To check one Postgres service's backup, restore its database dump with `kip service restore-dump`.

```bash
kip install --host 203.0.113.10 --admin-email admin@example.com
```

The installer prints the profile and storage it chose. To start on Longhorn instead, pass `--storage longhorn`. Longhorn needs at least 6000 MiB of RAM, so this works only on a server that size or larger. That cluster gets Longhorn's in-cluster backups as well, and needs a larger disk to meet the [Longhorn disk requirements](/en/installation#preflight-checks).

## What to know about local storage

Local storage keeps things simple, with a few limits specific to this storage mode:

- **One node.** Every volume lives on the server, so `kip node add` refuses to join a node until the cluster [grows onto Longhorn](#grow-in-place).
- **No shared volumes.** Shared (ReadWriteMany) volumes need Longhorn. Use a [MinIO service](/en/shared-storage#minio-recommended) for files that several apps read and write.
- **No size limit per volume.** A service's volume can use whatever space the disk has left. `kip status` shows each node's free disk and its largest volumes, and warns when the disk runs low.
- **Changes in one direction only.** A local cluster can switch to Longhorn with `kip cluster storage longhorn`. A Longhorn cluster stays on Longhorn, and `kip install` never switches the mode of an existing cluster.

## Grow in place

A nano cluster grows without moving to a new one. Resize the server to at least 6000 MiB of RAM, switch the cluster to Longhorn, and move each service's data onto Longhorn when it suits you. Apps, domains, secrets and settings stay where they are.

### 1. Check the requirements

- **RAM.** Longhorn needs at least 6000 MiB, so resize the server first. The cluster keeps its sizing profile, which [`kip platform profile set`](/en/platform-resources#sizing-profiles) changes.
- **An up-to-date platform.** Run `kip upgrade` with the same kip version first. `kip cluster storage longhorn` refuses on an older platform, because the move relies on admission policies that the upgrade installs.
- **Disk.** Longhorn keeps more than 25% of the filesystem holding `/var/lib/longhorn` free. Every moved service needs room for a second copy of its data, because its local copy stays until you prune it.
- **OS.** The [Longhorn OS rules](/en/installation#preflight-checks) apply, so Rocky Linux and AlmaLinux 9.7 or later 9.x releases are refused.

### 2. Switch the cluster to Longhorn

```bash
kip cluster storage longhorn
```

The command prepares the host (kernel modules, iSCSI and NFS packages), installs Longhorn and records Longhorn as the cluster's storage mode. Nothing stops while it runs. Afterwards new volumes get one Longhorn copy, `kip node add` accepts workers, and shared (ReadWriteMany) volumes work. It ends with a list of the service volumes still on local storage and the command that moves each one. If it is interrupted, run it again and it continues.

The platform's own volumes (the registry, MinIO and Loki) stay on local storage on the server. They keep working, and the server's disk keeps holding images, backups and logs.

### 3. Move each service's data

```bash
kip service storage longhorn db
```

The command shows how much data it will copy and asks before it starts. It then stops the service, keeps every other pod off the volume, copies the data into a new single-copy Longhorn volume, compares every file, and starts the service on the new volume. The service is down for the whole copy, roughly as long as reading and writing its data once. The copy pod may use up to 1 GiB of memory, mostly for data waiting to be written to the new volume. Apps that use it see the service as unavailable during that time.

- **Backups.** On a cluster with backups, a completed backup of the service's project is required. Run `kip backup create` first if there is none. On a cluster without backups, pass `--no-backup` to move anyway.
- **Interruptions.** Run the same command again and it continues from the step it reached. It first checks that the service is still stopped and that every other pod is kept off the volume. A completed, verified copy is kept unless another pod mounted the new volume or its protection was lifted in the meantime. In either case, kip copies the data again. Until the move commits, just before the service starts on Longhorn, `--rollback` undoes it and starts the service on its local volume again. `--rollback` also undoes a move whose record was lost, using the remaining volumes.
- **A failed comparison.** The service stays stopped with its local data untouched. kip prints what the copy reported, or how its pod ended when it reported nothing. Run the command again to copy again, or roll back.
- **Starting and deleting the service.** While a move is unfinished, the service stays stopped. `kip service start` refuses to start it. `kip service delete` and project deletion are blocked until the move finishes or is rolled back. `kip status` lists it under "Moves to Longhorn not finished".
- **Backups during the move.** A backup that runs while the service is moving holds no data for it, because the service is stopped. The copy pod and its read-only view of the old directory are left out of every backup, so restoring that backup never starts a copy again.

A database dump volume is recreated empty on Longhorn during the move. The next scheduled dump fills it.

### 4. Prune the local copies

Each moved service's old local directory stays on the server, so a problem found right after the move can still be recovered by hand. `kip status` shows how many local copies are kept and how much space they use. Once the services run well on Longhorn, delete the local copies:

```bash
kip cluster storage prune
```

The command lists the copies, asks once, and deletes only copies whose service still has its data on the Longhorn volume the move verified, or whose service was deleted. A copy is kept when its service lost or replaced that volume, because it may hold the only original data.

## Move to a new cluster instead

To change servers rather than grow the one you have, install a cluster with Longhorn on a larger server, or several servers, and copy your projects across with [Cluster Migration](/en/migration). Migration copies apps, services, secrets and volume data once, so [freeze writes on the source](/en/migration#3-freeze-writes-on-the-source) before it starts and keep them frozen until you switch DNS.

Before you migrate, check two things:

- **Declared sizes.** Migration creates each volume on the target at the size the service declares, while a local volume can hold more than that. Compare each service's data with its declared size, and increase the declared size before migrating if needed:

  ```bash
  kip service update db --storage 20Gi
  ```

- **Migration limits.** Databases up to about 500 MB move automatically, and apps built from git are rebuilt from the head of their branch on the target. See [Limitations](/en/migration#limitations).
