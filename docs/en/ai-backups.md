---
title: AI Backup & Recovery
description: Back up and restore the AI bundle and repair orphaned backup state.
---

# AI Backup & Recovery

Use this guide to preserve an installed [AI Bundle](/en/ai) before an uninstall or change. Wait for a backup to complete before deleting the source data.

## Backup and restore {#backup-and-restore}

Snapshots are handled by Velero, which Kipper already runs as a system component. A backup grabs everything in the `kipper-ai` namespace (model cache PVC, MongoDB data, chat history, LibreChat credentials), the cluster-side `kipper-ai-config` Secret in `kipper-system` (so kip's AI client config comes back too), and the Ollama and LibreChat HelmChart CRs in `kube-system` (so helm-controller still recognises the bundle after restore).

The backup is a live filesystem snapshot. MongoDB and Meilisearch keep writing during the snapshot, so the very last in-flight chat messages may not survive a restore cleanly. For a clean checkpoint (e.g. before a risky upgrade), pause LibreChat traffic for a few seconds before running `kip ai backup`.

```bash
# Take a snapshot. Without --name a timestamped name is generated.
# The command exits after about 60 seconds, leaving the backup
# running in the background. Use 'kip ai backup show' to check on it.
kip ai backup
kip ai backup --name pre-upgrade

# Block until the backup finishes (useful from scripts).
kip ai backup --name pre-upgrade --wait

# Show detailed status of a single backup (phase, items, errors).
kip ai backup show --name pre-upgrade

# Show your AI snapshots (foreign Velero backups are filtered out).
kip ai backup list

# Drop a snapshot. The command issues a Velero DeleteBackupRequest
# and exits after about 60 seconds. Velero deletes the Backup CRs in
# the background, then reclaims the underlying Kopia repo data via
# scheduled maintenance jobs (visible as kopia-maintain-job pods in
# the velero namespace). Pass --wait to block until the Backup CRs
# disappear, or check 'kip ai backup list' afterwards.
kip ai backup delete --name pre-upgrade
kip ai backup delete --name pre-upgrade --wait
```

Each snapshot is two Velero backups under the hood: one for the `kipper-ai` namespace, one for the cross-namespace config Secret. `kip ai backup list` shows them as a single entry; `delete` removes both.

Backups of multi-gigabyte model caches can take several minutes to upload through Velero's filesystem backup. The default `kip ai backup` flow watches for the first 60 seconds (long enough to surface a malformed name, a Velero outage, or an RBAC issue), then exits. Use `kip ai backup show --name <name>` to track the in-flight snapshot or pass `--wait` if you need the command to block.

Restore replays a snapshot into the same cluster. It refuses to run while `kipper-ai` is still installed, so the safe sequence is uninstall first, then restore. Use `--wait` (or check `kip ai backup show` for `Completed`) before uninstalling. `kip ai backup` on its own exits after a 60-second warmup, and uninstalling during the still-uploading phase deletes the source PVCs before Velero is done.

```bash
kip ai backup --name pre-upgrade --wait
kip ai uninstall
kip ai restore --name pre-upgrade
```

After a restore, run `kip ai status` to confirm both Ollama and LibreChat are ready. Existing admin accounts come back with the snapshot, so `kip ai admin create` is only needed if the snapshot pre-dates that account.

### Repairing orphan backup state {#repairing-orphan-backup-state}

Backup state can drift out of sync with reality in three ways:

1. A Backup CR points at Kopia repo data that was wiped manually from MinIO (most often after a `mc rm` of the bucket).
2. MinIO holds backup directories with no matching Backup CR. This is what `kubectl delete backup` produces, since `kubectl delete` bypasses Velero's deletion pipeline so Kopia data is never freed.
3. A BackupRepository CR is in a non-Ready phase. Velero's view of the bucket has diverged from reality and the next backup attempt fails with `repository not initialized in the provided storage`.

`kip ai backup repair` detects all three states, prints a plan, asks for explicit confirmation, then executes the cleanup:

```bash
kip ai backup repair          # interactive, prints plan and asks y/N
kip ai backup repair --yes    # non-interactive (e.g. from a script)
```

The command compares MinIO's `velero/backups/` directory against every Velero Backup CR (not only AI bundle ones) so cluster-wide schedules like `daily-apps` and `weekly-system` are never falsely flagged. Cluster-side findings (broken BackupRepository CRs, orphan Kipper Backup CRs) are still surfaced when MinIO is unreachable, so a torn-down storage layer doesn't hide a fixable problem.

### If you wiped MinIO {#if-you-wiped-minio}

`mc rm --recursive` against the `velero` bucket frees disk space immediately, but it leaves Velero's BackupRepository CR pointing at metadata that no longer exists. The next `kip ai backup` attempt fails with `repository not initialized in the provided storage`. The repository is wedged until something forces Velero to re-initialise it.

Two ways out:

```bash
# Preferred: kip ai backup repair detects the wedged repository and
# guides cleanup. Once the BackupRepository CR is gone, Velero re-
# initialises Kopia on the next backup.
kip ai backup repair

# Manual fallback if you cannot install the latest kip yet. Replace
# the BackupRepository name with what 'kubectl -n velero get
# backuprepositories' shows; the controller will create a fresh CR
# the next time a backup runs.
kubectl -n velero delete backuprepository <name>
```

Wiping MinIO also leaves any existing Backup CRs orphaned: the metadata in MinIO is gone but the CRs still exist. `kip ai backup repair` surfaces those too. After repair, the first new backup is a full upload (Kopia has no historical data to deduplicate against), so expect it to take longer than incremental snapshots.
