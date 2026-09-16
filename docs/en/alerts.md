# Alerts

Kipper tracks important cluster events (resource changes, OOM kills, stuck pods) and surfaces them through an in-console alerting system with optional Slack integration.

## The bell icon

The bell icon in the sidebar shows a badge with the number of unread alerts. Click it to open the alerts panel, which lists the most recent 50 alerts sorted newest first.

Each alert shows:

- **Severity:** critical (red), warning (yellow), or info (green)
- **App and namespace:** which workload triggered the alert
- **Action:** what happened (memory doubled, CPU increased, stuck pod deleted)
- **Reason:** why the action was taken (OOM kill detected, usage at 92%)
- **Timestamp:** when the event occurred

## What triggers alerts

Alerts come from the resource controller. Failure alerts (crash loops, read-only
volumes, unready nodes, failed jobs, stalled rollouts) run whether the controller
is in auto or expert mode, because an operator needs to know about those either
way. The alerts about resource changes below only appear in auto mode, since
expert mode makes no such changes.

### Resource adjustments

When CPU or memory usage stays above 80% or below 20% for 3 consecutive checks (each check runs every 60 seconds), the controller adjusts resources and creates an alert. The alert records the old and new values so you can see exactly what changed.

### OOM kills

When a pod is terminated due to an out-of-memory condition, the controller immediately doubles the memory limit and creates a critical alert. OOM recovery does not require multiple consecutive checks. It acts on the first detection.

### Stuck pods

If a pod remains in `ContainerCreating` state for more than 5 minutes, the controller deletes it (allowing Kubernetes to recreate it) and creates a warning alert.

### Node resource pressure

When total memory usage across all pods exceeds 80% of the node's allocatable memory, the controller generates a warning alert listing the top consumers and any anomalies. At 90%+, the alert is marked critical. The alert includes which workloads are using the most memory and which ones have grown significantly in the last 10 minutes.

### Default profile application

When a new app has no resource requests configured, the controller applies profile defaults and creates an informational alert recording which profile was used.

### Crash loops

A crash-looping container generates hourly warnings for six hours, then a critical alert and daily reminders. This keeps recurring failures visible while preserving space in the 50-alert history.

After ten minutes of healthy running, the episode closes. An escalated episode also produces an all-clear alert.

### Read-only volumes

When a container with a writable persistent volume fails, Kipper checks its previous logs for read-only filesystem errors. A matching message produces a critical `ReadOnlyFilesystem` alert containing the log evidence and mounted volumes.

The message identifies a write failure, but may not identify which filesystem failed. Check the affected mount before recreating the pod. See [Recovering a read-only volume](#recovering-a-read-only-volume).

## Where alerts go

Alerts are delivered to Slack when a webhook is configured, and by email to the
cluster admins when one is not. With neither configured they are stored in the
bell and go nowhere else, which means only somebody who already has the console
open will see them.

Both the console's Settings page and `kip status` say which of the three is
happening:

```
$ kip status

  Alerts:
    ⚠  not leaving this cluster
       Alerts are stored in the console bell and delivered nowhere. Add a
       Slack webhook or SMTP server under Settings in the console.
```

Configure a channel under **Settings** in the console. See
[Configuration](/en/configuration#slack-notifications) for the Slack setup.

## Dismissing alerts

Click **Dismiss** in the alerts panel to mark all current alerts as read. The unread count on the bell icon resets to zero. Dismiss is per-user, so each team member has their own read/unread state.

Dismissed alerts remain in the panel’s history. New alerts increase the unread count again.

## Storage

Alerts are stored in a Kubernetes ConfigMap (`kipper-alerts` in the `kipper-system` namespace). The system retains the most recent 50 alerts, automatically discarding older ones. No external database is required.

## Recovering a read-only volume

A Longhorn volume holds an iSCSI session between the node and the pod serving the
volume's data. When that session drops, the block device underneath the mounted
filesystem fails, and ext4 answers by remounting the filesystem read-only.

The pod does not notice until it writes. A database then dies on its first write,
gets restarted by Kubernetes into the same read-only mount, and dies again. Every
signal reads as an ordinary crash loop: the volume is attached, the pod is
scheduled, and the container's own log is the only place that says why.

**Restarting the container does not fix it.** The mount belongs to the pod, so
kubelet has to detach and re-attach the volume, and that only happens when the
pod is recreated:

```bash
kip service restart <name>
```

The volume's data is untouched. The pod is recreated and its volume reattached.

To confirm what is happening first:

```bash
kip service list           # a crash-looping service is named, with its restart count
kip status                 # reads /proc/mounts on the cluster host
```

`kip status` reads mounts over one SSH connection, to the cluster host, so on a
multi-node cluster it names the nodes it could not check. The alert above has no
such limit: it runs in the cluster and sees pods on every node.

### What causes the session to drop

The common trigger is a security update. Debian's needrestart restarts daemons
holding a patched library, and restarting `iscsid` takes every Longhorn session
on the node with it.

Kipper’s protection covers restarts initiated by needrestart. An upgrade of the open-iscsi package can restart `iscsid` through its own package script, so plan storage-related package upgrades carefully.

Kipper writes `/etc/needrestart/conf.d/50-kipper-storage.conf` during
`kip install` and `kip node add` to stop that, leaving `iscsid`,
`systemd-networkd` and `systemd-resolved` for you to restart at a time you
choose. A node added by an older `kip` does not have the file:

```bash
kip node repair-host --host <address>
```

`kip status` reports services that still need a restart to load updated libraries:

```
  Pending restarts:
    ⚠  deferred by Kipper: iscsid.service
       Patched libraries stay unloaded until these restart. Restarting them
       drops every Longhorn volume on this node, so reboot the node during a
       window you choose rather than restarting the units directly.
```

Reboot the node to pick those up. A reboot restarts everything cleanly, where
restarting `iscsid` on its own reproduces the failure this configuration exists
to prevent.
