---
title: 'Start small with a nano Kubernetes cluster'
description: 'Run a full Kipper cluster on a 4 GB server with local storage, then grow it onto Longhorn in place when you need more.'
---

# Start Small with Nano

Kubernetes has a reputation for needing big servers and a lot of setup. With minimal setup, nano gives you a full Kubernetes cluster with Kipper's console, builds, services and functions on a server with 4 GB of RAM and 20 GiB of free disk space. It suits anyone who wants to try Kipper, run a prototype, test an idea or give a QA team its own cluster, and it is a real cluster rather than a demo mode.

When you need more, you resize the server and switch the same cluster to Longhorn in place, then add nodes. You can also install a larger cluster and move your projects to it with [Cluster Migration](/en/migration).

## What makes a cluster nano

`kip install` picks the `nano` profile on servers with less than 6000 MiB of RAM or too few CPUs for a larger profile, or when you pass `--profile nano`. By default a new nano cluster keeps its footprint small in three ways:

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
- **CPU.** Longhorn's storage engine requests 12% of every node's allocatable CPU, which is 240m on a 2-CPU server. That much CPU must be available after accounting for requests from pods on the node and pending pods that could run there. Otherwise, a service could fail to restart after its move, so `kip cluster storage longhorn` refuses a node without that CPU available. When possible, the refusal suggests service CPU reductions on that node that would make room, for example "Lower db in shop-staging from 500m to 405m with 'kip service update db --cpu 405m --project shop --environment staging', or give the node more CPU". These are suggestions for you to apply yourself. Burstable cloud instances, which guarantee only a fraction of their CPU, are a poor fit for Longhorn, because its engine needs that CPU whenever a volume is written.
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

The command shows how much data it will copy and how long the service will be stopped, and asks before it starts. It then stops the service, keeps every other pod off the volume, copies the data into a new single-copy Longhorn volume, verifies the copy, and starts the service on the new volume. Apps that use the service see it as unavailable the whole time.

- **Downtime.** The estimate assumes 10 MiB/s for copying and verifying together, plus 3 ms for each file and directory and three minutes for stopping, detaching and starting. On a small cloud server, a 6 GiB volume took about eight minutes to copy and verify, and 150,000 small files kept a service down for 12 minutes. The estimate allows extra time compared with these measurements. The prompt shows how many files and directories the volume holds, because a volume with many small files, such as uploads or a cache, takes longer than its size suggests.
- **Verification.** The copy pod copies the data and flushes it to the volume, then kip deletes the pod and waits for Longhorn to detach the volume. A separate verify pod then mounts the copy read-only on a fresh mount and compares every file, its size, owner, mode and checksum, with the original. Only that comparison completes the copy, so what it reads is what Longhorn stored, not data still in memory.
- **Resources.** Before the service stops, kip checks that the server keeps room for the larger of the copy pod, the verify pod and the service itself once the service's own pods are gone, counting CPU and memory separately and counting pending pods that could be placed there. This is a point-in-time check of the requests on the node, not a reservation, so it can refuse a move that would have fit or let through one that later waits for room. kip also checks that the namespace's ResourceQuota admits the copy pod, which may use up to 1 GiB of memory for data waiting to be written, and names the quota if it refuses the move. A quota with scopes counts the pods its scopes cover, the way the API server evaluates them, and kip refuses the move if it cannot evaluate a scope, naming the affected quota.
- **Running out of room.** The new volume holds the data's size plus 20%, rounded up to whole GiB with a minimum of 1 GiB. A larger declared size is preserved. If the copy still runs out of room, kip grows the volume once, to a quarter more than planned or 30% more than the data, whichever is larger. It records the new size first, checks Longhorn's room and the quota again, counting the dump volume that a service with consistent backups gets on Longhorn at the new size, waits for the volume to show the new size and copies again. If the copy runs out of room on the grown volume too, the service stays stopped and `--rollback` returns it to its local volume.

- **Backups.** On a cluster with backups, a completed backup of the service's project is required. Run `kip backup create` first if there is none. On a cluster without backups, pass `--no-backup` to move anyway.
- **Interruptions.** Run the same command again and it continues from the step it reached. It first checks that the service is still stopped and that every other pod is kept off the volume. A completed copy is kept unless another pod mounted the new volume or its protection was lifted in the meantime, in which case kip copies the data again. A move that resumes after its copy was verified, but before it committed, verifies the copy again against the original, because a pod that wrote to the volume while kip was away may be gone by then. If the copy no longer matches, kip copies the data again. Until the move commits, just before the service starts on Longhorn, `--rollback` undoes it and starts the service on its local volume again. `--rollback` also undoes a move whose record was lost, using the remaining volumes.
- **A failed comparison.** The service stays stopped with its local data untouched. kip prints what the copy reported, or how its pod ended when it reported nothing. Run the command again to copy again, or roll back.
- **Starting and deleting the service.** While a move is unfinished, the service stays stopped. `kip service start` refuses to start it. `kip service delete`, `kip project delete` and `kip project remove-env` block deletion of the affected service, project or environment until the move finishes or is rolled back. The console also blocks deletion of the affected project or removal of the affected environment. `kip service recreate-dump` refuses to replace the service's dump volume until the move ends. `kip status` lists it under "Moves to Longhorn not finished".
- **A namespace under move cannot be deleted.** An admission policy refuses to delete a namespace while a move runs in it, whoever asks, and kip checks that the policy is enforced before it stops the service. A project deleted some other way waits with the condition `WaitingForStorageMove` and finishes once the move is done or rolled back. A new move stops before stopping the service if its namespace or project is already being deleted. A move that has stopped the service goes on, and the deletion waits for it. While only the project is being deleted, its namespace still has to refuse deletion before the move goes on.
- **Volumes created by a cluster admin.** The move deletes the empty claim the StatefulSet recreates while the old claim is gone. A cluster admin who creates a PersistentVolume bound in advance to that claim's name at that moment could have it deleted with the claim. Don't create volumes for a service while its move runs.
- **Backups during the move.** A backup that runs while the service is moving holds no data for it, because the service is stopped. The copy pod and its read-only view of the old directory are left out of every backup, so restoring that backup never starts a copy again.

A database dump volume is recreated empty on Longhorn during the move. The next scheduled dump fills it.

### 4. Prune the local copies

Each moved service's old local directory stays on the server, so a problem found right after the move can still be recovered by hand. `kip status` lists each kept copy, how much space the copies use, and why each one is still kept. Delete the copies that are no longer needed with:

```bash
kip cluster storage prune
```

The command lists the copies, asks once, and deletes a copy only when all of these hold:

- Its service still has its data on the Longhorn volume the move verified, and the move finished.
- The service's pod uses that volume and has been Ready for at least an hour. The hour starts again when the pod becomes Ready after a crash.
- A backup made after the move holds the service's data from Longhorn, or a day has passed since the move. A backup counts only if it has a future expiry and is not being deleted. A Postgres service with consistent backups always waits the day, because Velero records hook results for the whole backup rather than for each pod, so a backup does not show that this service's own dump succeeded.
- Any change to that volume's copy count has finished. This requirement also applies with `--now` or `--delete-unproven`.

A copy whose service and data claim are both gone is deleted without these checks, and the prompt lists it separately. A copy is kept when its service lost or replaced that volume, because it may hold the only original data. Each copy is checked again right before it is deleted.

`--now` deletes the copies of healthy services without waiting for a backup or a day, so the local copy stops being the fallback early. The health check still applies. A service that has not been Ready on Longhorn for an hour, such as one you stopped, keeps its copy. To delete that copy sooner, name the service and pass `--delete-unproven`:

```bash
kip cluster storage prune db --project shop --delete-unproven
```

`--delete-unproven` waives only the health check for the named copy. The wait for a backup or a day still applies unless you also pass `--now`.

## Move to a new cluster instead

To change servers rather than grow the one you have, install a cluster with Longhorn on a larger server, or several servers, and copy your projects across with [Cluster Migration](/en/migration). Migration copies apps, services, secrets and volume data once, so [freeze writes on the source](/en/migration#3-freeze-writes-on-the-source) before it starts and keep them frozen until you switch DNS.

Before you migrate, check two things:

- **Declared sizes.** Migration creates each volume on the target at the size the service declares, while a local volume can hold more than that. Compare each service's data with its declared size, and increase the declared size before migrating if needed:

  ```bash
  kip service update db --storage 20Gi
  ```

- **Migration limits.** Databases up to about 500 MB move automatically, and apps built from git are rebuilt from the head of their branch on the target. See [Limitations](/en/migration#limitations).
