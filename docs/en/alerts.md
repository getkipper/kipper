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

A container Kubernetes has given up restarting produces a warning every hour for
six hours. If it is still going at six hours, one critical alert says it is not
recovering on its own and names the command that recreates the pod. After that
it repeats once a day for as long as the loop lasts.

The ladder exists because the bell holds fifty alerts. An hourly warning that
never changes fills it in two days and evicts the alert that would have told you
when the trouble started.

An episode closes when the container has run clean for ten minutes. If it had
escalated, an all-clear says so, because the people who were told about it are
owed the news that it is over.

### Read-only volumes

When a container with a persistent volume dies, Kipper reads the log it wrote on
the way out. A line saying the filesystem is read-only raises a `VolumeReadOnly`
alert rather than another crash-loop warning, quoting the line it found so you
can see the evidence rather than take the diagnosis on trust.

A log line says a filesystem stopped accepting writes. It rarely says which, and
a container has several: its persistent volumes, anything mounted from a
ConfigMap or Secret, and its own image, which lives on the node's disk. Nor can
the path in the message settle it, because a pathname does not identify the
filesystem behind it. Postgres puts `/var/lib/postgresql/data/pg_wal` on a
separate volume often enough for that to matter.

So the alert reports what it knows and leaves the rest to you:

```
container "postgres" wrote this before it died: FATAL:  could not remove old
lock file "postmaster.pid": Read-only file system. The container mounts volume
data-db-0. Either that volume or the node's own disk stopped accepting writes,
and a container restart clears neither. Recover with: kip service restart db
```

Only containers that mount a writable persistent volume raise it, because those
are the ones a pod recreation can help. A volume the workload asked for
read-only is left out: that one did not remount, it was mounted that way, and a
new pod reproduces it.

It is critical from the first sighting rather than climbing the ladder above.
The filesystem does not come back on its own, and restarting the container
cannot clear a mount. See [Recovering a read-only
volume](#recovering-a-read-only-volume).

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

Dismissed alerts are not deleted. They remain visible in the alerts panel but no longer contribute to the unread count. New alerts that arrive after dismissal will increment the badge again.

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

Kipper writes `/etc/needrestart/conf.d/50-kipper-storage.conf` during
`kip install` and `kip node add` to stop that, leaving `iscsid`,
`systemd-networkd` and `systemd-resolved` for you to restart at a time you
choose. A node added by an older `kip` does not have the file:

```bash
kip node repair-host --host <address>
```

That trade has a cost, and `kip status` reports it rather than letting it become
drift nobody agreed to:

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
