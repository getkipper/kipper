---
title: 'Share files between pods on k3s: S3 or ReadWriteMany'
description: 'Two ways several pods can use the same files: S3-compatible object storage, or a ReadWriteMany volume mounted into every replica.'
---

# Shared Storage

When multiple replicas of an app need to access the same files (uploads, media, shared data), you need shared storage. Kipper provides two approaches.

## MinIO (recommended)

MinIO provides S3-compatible object storage. Choose it when your application reads and writes files through an S3 API or plugin.

```bash
kip service add minio --name media --project blog --environment prod
```

```
  Connection details:
    Endpoint:   http://media.blog-prod.svc.cluster.local:9000
    Access Key: kipper
    Secret Key: a1b2c3d4...
```

### Why MinIO is recommended

- **Shared access:** multiple replicas use the same object store
- **Works with any S3-compatible SDK:** AWS SDK, MinIO SDK, boto3
- **Survives pod restarts:** data stored independently from pods
- **Web console included:** browse and manage files at port 9001
- **Standard approach:** most modern apps and frameworks support S3 natively

### Connecting your app

Bind the MinIO service to your app to inject credentials automatically:

```bash
kip service bind media my-app --project blog --environment prod
```

This injects `S3_ENDPOINT`, `S3_ACCESS_KEY`, and `S3_SECRET_KEY` into the app. `S3_ENDPOINT` is a full URL (`http://storage.blog-test.svc.cluster.local:9000`) you pass straight to your S3 client.

### Common integrations

- **WordPress:** WP Offload Media or Media Cloud plugin
- **Laravel:** `FILESYSTEM_DISK=s3` with MinIO endpoint
- **Node.js:** `@aws-sdk/client-s3` with MinIO endpoint
- **Django:** `django-storages` with S3 backend
- **Spring Boot:** `spring-cloud-aws` or MinIO Java SDK

## Shared Volumes

For apps that need a traditional shared filesystem (e.g. legacy apps that read/write files to disk), Kipper provides shared volumes backed by Longhorn with ReadWriteMany (RWX) access.

Shared volumes need a cluster with Longhorn storage. A cluster with [local storage](/en/nano) rejects them in `kip`, the console and the API. Use a MinIO service on these clusters instead.

Shared volumes provide a filesystem path through Longhorn's RWX storage. Choose this approach when your app expects filesystem access, and account for the shared storage service when planning capacity and availability.

On Linux 7.0 and later, the NFS client requests directory delegations, which Longhorn 1.7.3's shared volume server rejects. Apps then get "Remote I/O error" when accessing subdirectories, and backups skip the volume's files. `kip install` and `kip node add` turn off directory delegations on the node they set up. For clusters set up by older kip versions, run `kip node repair-host` once. It covers every recorded node.

### Create a shared volume

```bash
kip volume create uploads --size 10Gi --project blog --environment prod
```

A new shared volume keeps one copy of its data on the server, like a service created with the default copy count. Volumes created by earlier Kipper releases keep the copies they have. `kip volume create` refuses creation when the server's Longhorn disks have insufficient capacity for the volume and names the disk's node and its remaining room.

```
  ✔  Shared volume "uploads" created (10Gi)
  Access mode: ReadWriteMany (multiple pods can mount this)

  Mount into an app:
    kip volume mount uploads <app-name> --path /data/uploads
```

### Mount into an app

```bash
kip volume mount uploads frontend --path /app/public/uploads --project blog --environment prod
```

This mounts the shared volume at `/app/public/uploads` inside every pod of the `frontend` deployment. All replicas read and write to the same filesystem.

You can mount the same volume into multiple different apps:

```bash
kip volume mount uploads frontend --path /app/public/uploads
kip volume mount uploads image-processor --path /data/input
```

Mounts are recorded on the volume and applied to the app automatically, so they survive image updates and redeployments. Mounting an already mounted volume at a different path moves it.

### Unmount from an app

```bash
kip volume unmount uploads image-processor --project blog --environment prod
```

```
  ✔  Volume "uploads" unmounted from image-processor
  The pod restarts to detach the volume. The volume and its data still exist.
```

Unmounting only detaches the volume from that app. Other apps keep their mounts, and the data stays on the volume until you delete it.

### List shared volumes

```bash
kip volume list --project blog --environment prod
```

```
  NAME                      SIZE       STATUS     ACCESS
  uploads                   10Gi       bound      ReadWriteMany
```

If a volume's [backup holder](#backups) cannot run, a warning below the table explains why.

### Delete a shared volume

```bash
kip volume delete uploads --delete-data --project blog --environment prod
```

The `--delete-data` flag is required to prevent accidental data loss.

### Backups

Backups copy a volume's files through a running pod that mounts it. Kipper runs a small holder pod named `volume-holder-<name>` for each shared volume so backups include its files even when no app mounts it, for example while every app using it is stopped. The holder mounts the volume read-only and sleeps. It runs as a non-root user without a service account token or open ports, and it requests 1m CPU and 8Mi memory, with limits of 10m CPU and 16Mi memory that count towards the project's quota. Deleting the volume deletes its holder.

A restore brings the holder back along with the volume, and Velero writes the volume's files through it before the holder starts. Kipper adopts the restored holder once the volume itself is restored. Until then, the holder has no owner, so `kip node reboot` and `kip node remove` refuse to drain its node and identify the `volume-holder-<name>` pod in the error.

`kip volume list` adds a warning line for a volume whose holder cannot run: the project's quota or an admission policy rejects it, it cannot be scheduled, its container fails, or it has not started within ten minutes. Kipper records the reason on the volume's `BackupHolderReady` condition and retries a rejected holder every minute. Until the holder runs, the volume's files are backed up only while an app mounts it. A holder that is evicted is replaced. A holder on a node that goes down stays terminating until the node is back or removed with `kip node remove`, and `kip volume list` warns after ten minutes of termination.

If a restored holder stops before its restore finishes, or is still waiting for its restore 24 hours after the holder was created, Kipper replaces it so later backups include the volume again. The 24-hour limit prevents a failed volume restore from leaving the holder waiting indefinitely. Kipper records a warning event on the volume, and `kip volume list` warns until the new holder runs. When the restore finishes, `kip backup restore` reports the restore failure. Restore again if the volume's files are incomplete.

### Web console

Shared volumes can also be managed entirely from the web console via the **Volumes** page (HardDrive icon in the sidebar):

- **Project selector:** choose which project and environment to view
- **Create:** click "Create" to set a name and size for a new shared volume
- **Mount:** click "Mount" to attach a volume to an app. Select the volume and app from dropdowns, then enter the mount path
- **View mounts:** each volume card shows which apps have it mounted and at which path
- **Disconnect:** click "Disconnect" next to any mount to remove it from the app (the pod restarts automatically)
- **Delete:** remove the volume entirely

The Volumes page gives you a clear overview of which volumes exist, their status (Bound = ready, Pending = provisioning), and exactly which apps are using them.

## Which approach to use

| Criteria | MinIO (S3) | Shared Volume |
|---|---|---|
| Multi-pod access | Yes (via API) | Yes (via filesystem) |
| Access method | S3 requests | Shared filesystem operations |
| App compatibility | S3 SDK or plugin | App supports a shared filesystem |
| Survives pod restart | Yes | Yes |
| Use case | Modern apps, uploads, media | Legacy apps, WordPress without S3 plugin |
| Recommendation | **Preferred** | Use when S3 is not an option |
