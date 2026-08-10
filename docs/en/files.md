# Browsing Files in Running Containers

The Files tab in the app detail panel lets you browse the filesystem inside a running pod directly from the web console. No terminal access or `kubectl` knowledge required.

## Opening the file browser

1. Open the web console and navigate to your project
2. Click on an app to open its detail panel
3. Click the **Files** tab

Kipper connects to the first running pod for that app and lists the root directory (`/`).

## Navigating directories

Click any folder to navigate into it. The breadcrumb bar at the top shows your current path:

```
/ > app > uploads > images
```

Click any segment in the breadcrumb to jump back to that directory.

## Viewing files

Click any file to open a read-only preview in a modal window. The preview uses a monospace font on a dark background, making it easy to read configuration files, logs, and source code.

Files larger than 1MB cannot be previewed. Use the download button instead.

## Downloading files

Every file row has a download button on the right side. Click it to download the file to your local machine. This works for files of any size.

## Editing files

Click a file to open a preview, then click **Edit** to modify it in the built-in editor. When you save, Kipper writes the updated content to all running pods for that app, not just the one you are browsing. This keeps configuration changes consistent across replicas.

::: warning Edits do not survive deployments
Changes made through the file editor are written directly to the running container filesystem. They will be lost when the pod restarts or a new deployment rolls out. For permanent changes, update your source code or configuration and redeploy.
:::

Files larger than 1MB cannot be edited in the browser. Download the file, modify it locally, and upload it back.

## Uploading files

Click the **Upload** button in the file browser toolbar to upload a file from your local machine into the current directory. The upload limit is 10MB. Uploaded files are written to the pod you are currently browsing.

## Multi-pod support

When your app has multiple replicas, the file browser shows which pod you are connected to and the total pod count in the header. Browsing and downloading always target a single pod. Saving edited files writes to all running pods automatically.

## Blocked paths

For safety, Kipper blocks access to certain system directories:

- `/proc`: kernel and process information
- `/sys`: kernel subsystem data
- `/dev`: device files

Attempting to browse, read, or write files under these paths returns a 403 Forbidden error. Subdirectories are also blocked. For example, `/proc/cpuinfo` and `/sys/class/net` are both restricted.

## How it works

Under the hood, the Files tab uses the Kubernetes exec API to run `ls` and `cat` commands inside the container, the same mechanism as `kubectl exec`.

The file browser connects to the first pod with a `Running` status that matches the app's label selector. If no running pods are available (for example, during a deployment or if the app is scaled to zero), the Files tab shows an error.

## Limitations

- **1MB preview/edit limit:** files larger than 1MB must be downloaded to view or edit
- **10MB upload limit:** files larger than 10MB cannot be uploaded through the browser
- **Container filesystem only:** shows the container's filesystem, including mounted volumes but not the host filesystem
- **Edits are ephemeral:** file changes do not persist across deployments
