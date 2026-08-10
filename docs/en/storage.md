# Storage (MinIO File Explorer)

The Storage page in the web console provides a file explorer for MinIO S3 buckets. Browse files, upload and download objects, delete files, make objects public, and generate shareable download links, all from the browser.

## Prerequisites

You need at least one MinIO service running in your cluster:

```bash
kip service add minio --name storage --project blog
```

The Storage page automatically discovers all MinIO services in the cluster. If you have multiple MinIO instances across different projects, use the service selector dropdown to switch between them.

## Browsing files

Open the **Storage** page from the sidebar. Select a MinIO service and a bucket to browse its contents.

- Click a folder to navigate into it
- Use the breadcrumb navigation at the top to jump back to any parent folder
- Click the bucket name in the breadcrumbs to return to the root
- Click a file name to open it in a new browser tab

The file list shows each object's name, size, and last modified date. Folders appear first, followed by files sorted alphabetically. Public objects display a globe icon next to their name.

## Creating buckets

Click **New bucket** next to the bucket selector. Enter a name (lowercase letters, numbers, and hyphens only) and click **Create**.

Bucket names must be unique within the MinIO instance and follow S3 naming rules:
- 3 to 63 characters
- Lowercase letters, numbers, and hyphens
- Must start and end with a letter or number

## Uploading files

Click the **Upload** button in the top-right corner. Select one or more files from your machine. Files upload to the current folder path. If you are viewing `images/`, a file uploads as `images/yourfile.jpg`.

You can also drag and drop files directly onto the file list to upload them.

Upload supports files up to 100 MB through the web console. For larger files, use the MinIO Client (`mc`). See [Using mc from your app](#using-mc-from-your-app) below.

## Downloading files

Click any file name to open it in a new tab, or hover over a file and click the download icon to download it directly.

## Deleting files

Hover over a file to reveal the delete button (trash icon). Click it once, then click **Confirm** to permanently delete the file.

::: danger
Deletion is permanent. There is no recycle bin or undo. Make sure you have backups of important files before deleting.
:::

## Bulk file operations

Select multiple files by clicking the checkbox next to each file name. A floating action bar appears at the bottom of the screen showing the number of selected files and available actions:

- **Delete:** permanently delete all selected files
- **Make public:** make all selected files publicly accessible
- **Make private:** revoke public access for all selected files

A progress bar shows the status of bulk operations.

## Share links (presigned URLs)

Hover over a file and click the share icon to generate a presigned URL. The link allows anyone to download the file without authentication.

- Links expire after 24 hours by default
- Choose a custom expiry from the duration selector (1 hour to 7 days)
- Copy the link with the **Copy** button and share it with anyone who needs access

Presigned URLs are useful for:
- Sharing files with external collaborators
- Embedding download links in emails or chat messages
- Temporary access to private files without granting bucket-level permissions

Share links survive console-api restarts because the signing key is stored in a Kubernetes Secret.

## Public objects (permanent URLs)

For files that should always be accessible without authentication, make them public instead of using share links.

Hover over a file and click **Make public**. The file gets a permanent URL that does not expire:

```
https://console--203-0-113-10.kipper.run/api/v1/storage/storage/public/uploads/logo.png?namespace=blog
```

The `namespace` parameter names the project's storage service the link points at, so the same service name in another project resolves to its own files. The console fills it in when you click **Make public**.

Public objects are useful for:
- Static assets (logos, icons, stylesheets)
- Images embedded in emails or documentation
- Files that need a stable, permanent URL

To revoke public access, hover over the file and click **Make private**. The permanent URL stops working immediately.

::: tip
Share links are better for temporary access. Public objects are better for permanent, stable URLs.
:::

## Multiple MinIO services

If you run multiple MinIO instances (for example, one per environment), use the **Service** dropdown to switch between them. Each service has its own set of buckets and objects.

```bash
kip service add minio --name storage --project blog --environment staging
kip service add minio --name storage --project blog --environment production
```

Both services appear in the dropdown. Select the one you want to browse.

## Using mc from your app

When your app is bound to a MinIO service, Kipper injects S3 credentials as environment variables. You can use the MinIO Client (`mc`) to upload and download files directly from inside the pod. This is useful for data imports, batch operations, or files larger than the 100 MB web console limit.

### Bind MinIO to your app

```bash
kip service bind storage my-app --project blog --environment test
```

This injects the following environment variables into your app:

| Variable | Example value |
|---|---|
| `S3_ENDPOINT` | `http://storage.blog-test.svc.cluster.local:9000` |
| `S3_ACCESS_KEY` | `kipper` |
| `S3_SECRET_KEY` | `a1b2c3d4e5f6...` |

### Connect with mc

Exec into your app's pod and use the injected credentials:

```bash
kubectl exec -it -n blog-test deploy/my-app -- sh

# Download mc (single static binary, ~25 MB)
wget https://dl.min.io/client/mc/release/linux-amd64/mc -O /tmp/mc
chmod +x /tmp/mc

# Configure mc using the injected env vars
/tmp/mc alias set store ${S3_ENDPOINT} ${S3_ACCESS_KEY} ${S3_SECRET_KEY}
```

### Common operations

```bash
# List buckets
/tmp/mc ls store

# List files in a bucket
/tmp/mc ls store/uploads/images/

# Download a file
/tmp/mc cp store/uploads/images/photo.jpg /tmp/photo.jpg

# Upload a file
/tmp/mc cp /tmp/report.pdf store/uploads/reports/report.pdf

# Upload an entire directory
/tmp/mc cp --recursive /tmp/export/ store/uploads/export/

# Delete a file
/tmp/mc rm store/uploads/old-file.txt

# Mirror a local directory to a bucket (sync)
/tmp/mc mirror /tmp/assets/ store/uploads/assets/
```

### Using S3 SDKs

The same injected credentials work with any S3-compatible SDK. `S3_ENDPOINT` is already a full URL, so pass it straight through:

**Node.js (AWS SDK v3):**
```javascript
import { S3Client } from '@aws-sdk/client-s3';

const s3 = new S3Client({
  endpoint: process.env.S3_ENDPOINT,
  region: 'us-east-1',
  credentials: {
    accessKeyId: process.env.S3_ACCESS_KEY,
    secretAccessKey: process.env.S3_SECRET_KEY,
  },
  forcePathStyle: true,
});
```

**Python (boto3):**
```python
import boto3, os

s3 = boto3.client('s3',
    endpoint_url=os.environ['S3_ENDPOINT'],
    aws_access_key_id=os.environ['S3_ACCESS_KEY'],
    aws_secret_access_key=os.environ['S3_SECRET_KEY'])

s3.download_file('uploads', 'images/photo.jpg', '/tmp/photo.jpg')
```

**Java (MinIO SDK):**
```java
MinioClient client = MinioClient.builder()
    .endpoint(System.getenv("S3_ENDPOINT"))
    .credentials(System.getenv("S3_ACCESS_KEY"), System.getenv("S3_SECRET_KEY"))
    .build();
```
