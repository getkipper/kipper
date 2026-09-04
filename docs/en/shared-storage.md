---
title: 'Share files between pods on k3s: S3 or ReadWriteMany'
description: 'Two ways several pods can use the same files: S3-compatible object storage, or a ReadWriteMany volume mounted into every replica.'
---

# Shared Storage

When multiple replicas of an app need to access the same files (uploads, media, shared data), you need shared storage. Kipper provides two approaches.

## MinIO (recommended)

MinIO provides S3-compatible object storage. Applications use the S3 API to read and write files. This is the modern approach and the one used by AWS, GCP, and most cloud platforms.

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

- **Scales with any number of replicas:** no filesystem contention
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

::: warning Use MinIO when possible
Shared volumes work but have limitations: NFS overhead affects performance, and they create a dependency between pods and the volume. MinIO is faster, more resilient, and the industry standard for multi-pod file sharing.
:::

### Create a shared volume

```bash
kip volume create uploads --size 10Gi --project blog --environment prod
```

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

### Delete a shared volume

```bash
kip volume delete uploads --delete-data --project blog --environment prod
```

The `--delete-data` flag is required to prevent accidental data loss.

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
| Performance | High (direct API) | Moderate (NFS overhead) |
| Scalability | Excellent | Limited by NFS |
| App compatibility | Needs S3 SDK/plugin | Works with any app |
| Survives pod restart | Yes | Yes |
| Use case | Modern apps, uploads, media | Legacy apps, WordPress without S3 plugin |
| Recommendation | **Preferred** | Use when S3 is not an option |
