---
title: 'Start small with a nano Kubernetes cluster'
description: 'Run a full Kipper cluster on a 4 GB server with local storage, then move to a larger cluster when you need more.'
---

# Start Small with Nano

Kubernetes has a reputation for needing big servers and a lot of setup. With minimal setup, nano gives you a full Kubernetes cluster with Kipper's console, builds, services and functions on a server with 4 GB of RAM and 20 GiB of free disk space. It suits anyone who wants to try Kipper, run a prototype, test an idea or give a QA team its own cluster, and it is a real cluster rather than a demo mode.

When you need more, you install a larger cluster and move your projects to it with [Cluster Migration](/en/migration).

## What makes a cluster nano

`kip install` picks the `nano` profile on servers with less than 6000 MiB of RAM, or when you pass `--profile nano`. By default a new nano cluster keeps its footprint small in three ways:

- **Local storage.** Volumes live on the server's own disk through the local-path provisioner that k3s ships with. Omitting Longhorn and its iSCSI packages saves about 350 MiB of memory and about 850 MB of images. The registry's volume also uses local storage, so builds and images share the server's disk.
- **No monitoring by default.** Prometheus, Loki and Grafana stay off. Turn them on later with `kip platform enable prometheus` and `kip platform enable loki` if the server has room.
- **Backups to external storage only.** A cluster on local storage omits Velero and MinIO unless you supply `--backup-storage-bucket`, because a backup on the same disk would be lost if the disk failed. Add [external backup storage](/en/backups#external-s3-compatible-storage) when you want backups.

```bash
kip install --host 203.0.113.10 --admin-email admin@example.com
```

The installer prints the profile and storage it chose. To start a small server on Longhorn instead, so that you can add nodes later, pass `--storage longhorn`. That cluster gets Longhorn's in-cluster backups as well, and needs a larger disk to meet the [Longhorn disk requirements](/en/installation#preflight-checks).

## What to know about local storage

Local storage keeps things simple, with a few limits specific to this storage mode:

- **One node.** Every volume lives on the server, so `kip node add` refuses to join a node. Multi-node clusters use Longhorn.
- **No shared volumes.** Shared (ReadWriteMany) volumes need Longhorn. Use a [MinIO service](/en/shared-storage#minio-recommended) for files that several apps read and write.
- **No size limit per volume.** A service's volume can use whatever space the disk has left. `kip status` shows each node's free disk and its largest volumes, and warns when the disk runs low.
- **Fixed for the cluster's lifetime.** A cluster keeps the storage mode it was installed with. `kip install` refuses to switch it on an existing cluster.

## How to move on

A nano cluster grows by moving to a bigger one. Install a cluster with Longhorn on a larger server, or several servers, and copy your projects across with [Cluster Migration](/en/migration). Migration copies apps, services, secrets and volume data once, so [freeze writes on the source](/en/migration#3-freeze-writes-on-the-source) before it starts and keep them frozen until you switch DNS.

Before you migrate, check two things:

- **Declared sizes.** Migration creates each volume on the target at the size the service declares, while a local volume can hold more than that. Compare each service's data with its declared size, and increase the declared size before migrating if needed:

  ```bash
  kip service update db --storage 20Gi
  ```

- **Migration limits.** Databases up to about 500 MB move automatically, and apps built from git are rebuilt from the head of their branch on the target. See [Limitations](/en/migration#limitations).
