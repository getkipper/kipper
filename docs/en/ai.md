# AI Bundle

Run your own private LLM and chat UI in your cluster with one command. No cloud bills, no API keys, no data leaving your server.

The bundle installs two things:

- **Ollama** serves the language model and exposes an OpenAI-compatible API.
- **LibreChat** is the web chat UI on `chat.<your-domain>`.

After install you can chat with the model in your browser and call the API from your apps just like you would with OpenAI, no code changes needed.

## What hardware do you need?

`kip ai install` inspects your cluster and picks a model that fits. The hard refusal is **less than 8 GiB of free memory on a single node**, the floor the smallest useful model needs at runtime. Below that, install bails with a pointer to use a hosted provider instead.

The model RAM requirement is just the inference floor. To actually use the bundle without hitting the ceiling on every backup or upgrade, you also need CPU, disk, and (for serious use) a GPU.

| Tier | Free RAM (best node) | GPU | Default model | Realistic use |
|---|---|---|---|---|
| 1 | 8 GiB | none | Qwen 2.5 3B Q4 | Demo / autocomplete only |
| 2 | 16 GiB | none | Qwen 2.5 7B Q4 | Slow but usable chat |
| 3 | 16 GiB | NVIDIA, any | Qwen 2.5 7B/14B | Fast chat, real-time use |
| 4 | 32 GiB | one NVIDIA GPU with 16+ GiB VRAM | Qwen 2.5 32B | Production-grade local AI |

Tier 4 needs a single GPU at or above the VRAM threshold; the bundle requests one GPU per pod and does not split a model across devices.

### CPU and disk also matter

Memory is what the preflight gates on. CPU and disk are what determine whether the bundle is pleasant to use:

- **CPU.** Token throughput on CPU is roughly proportional to single-threaded performance. A 3B model on a modern 4-vCPU x86 box does 5-10 tokens/sec, a 7B model does 1-3 tokens/sec, and a 14B+ model on CPU is unusable. That's "demo good", not "ChatGPT good". If you need fast responses, get a GPU node or use a hosted provider. Backup and restore are also CPU-bound: Velero's Kopia uploader is essentially single-threaded per volume, so on a 4-vCPU box backing up a 5 GB model cache takes 10-15 minutes and a full restore can take longer.
- **Disk.** The model cache PVC is sized by tier (10 / 30 / 60 GiB by default; override with `--pvc-size`). On top of that, MinIO (Velero's object store, where snapshots land) needs roughly 3x the model cache size: one snapshot at parity with the source, plus headroom for incremental layers and Kopia's working overhead. Kipper provisions MinIO with 30 GiB by default, which is enough for the tier 1 model cache. Tier 2 and 3+ installs need a larger MinIO volume; `kip ai install` runs a storage preflight that refuses install when MinIO is too small and points you at the resize procedure below.
- **Network.** First-run model download is multi-GB pulled directly from Ollama's registry. On a slow uplink, install can take 20+ minutes before the model is loaded. Subsequent restores from a Velero snapshot use the local MinIO bucket and are bandwidth-bound to the node's disk, not the internet.

::: warning Be generous across the board
A box that meets the 8 GiB minimum on every other axis (2 vCPU, 40 GB disk) technically passes preflight, but every operation on it will be painful. Backup, restore, model upgrade, and even the install itself stretch from minutes to hours, and a single pod scaling up can cause memory eviction. For anything beyond a demo, plan on **at least 16 GiB RAM, 4 vCPU, and 100 GB SSD** for the AI bundle on top of whatever the rest of your cluster needs.
:::

### Model cache and snapshot sizing

The AI bundle's PVCs are typically dominated by the Ollama model cache:

| Model | Cache size on disk |
|---|---|
| qwen2.5:3b-instruct-q4_K_M | ~2.4 GB |
| qwen2.5:7b-instruct-q4_K_M | ~5 GB |
| qwen2.5:14b-instruct-q4_K_M | ~10 GB |
| qwen2.5:32b-instruct-q4_K_M | ~22 GB |

`kip ai install` provisions a PersistentVolume sized via `--pvc-size` (defaults: 10 GiB tier 1, 30 GiB tier 2, 60 GiB tier 3+) on whatever the cluster's default storage class is. On a fresh Kipper install that's typically Longhorn, which keeps a replica copy on top, so the underlying node disk needs at least 2x the model cache size free.

If you intend to take backups of the AI bundle, the cluster's MinIO volume needs roughly 3x the model cache size to hold one snapshot plus headroom. Fresh Kipper installs ship MinIO with 30 GiB, which fits the default tier 1 install (10 GiB cache). For tier 2 (30 GiB cache) and tier 3+ (60 GiB cache), the MinIO volume needs to be expanded to 90 GiB and 180 GiB respectively, and `kip ai install` will refuse with a clear error until you do.

Pick the size that matches the tier you intend to install (or the `--pvc-size` you plan to pass):

| Install plan | Model cache PVC | MinIO needs |
|---|---|---|
| Tier 1 default | 10 GiB | 30 GiB (already the install default) |
| Tier 2 default | 30 GiB | 90 GiB |
| Tier 3+ default | 60 GiB | 180 GiB |
| Custom `--pvc-size N` | N | 3 × N |

For clusters installed before the MinIO default was bumped (the original 5 GiB sizing), or when moving up a tier, expand the volume in place before running `kip ai install`. The example below sizes for tier 2; substitute `90Gi` with the target from the table:

```bash
kubectl -n velero patch pvc minio-storage \
  --type merge \
  -p '{"spec":{"resources":{"requests":{"storage":"90Gi"}}}}'
```

Longhorn supports online expansion when the volume's storage class allows it (`longhorn-single` does). Wait for `kubectl -n velero get pvc minio-storage` to show the new capacity before re-running `kip ai install`. If you would rather skip the storage check entirely (evaluation installs that will never run `kip ai backup`), pass `--skip-storage-check`. Snapshotting against an undersized MinIO produces `PartiallyFailed` Backup CRs, Kopia errors about object-storage write failures, and BackupRepository CRs pointing at half-written repo metadata. Recovery is `kip ai backup delete --name <name>` to clear the failed CRs and freeing MinIO space before re-running.

## Install

```bash
kip ai install
```

The command picks a sensible default for everything based on your cluster. You can override the chat hostname or model:

```bash
kip ai install --host chat.acme.com
kip ai install --model qwen2.5:7b-instruct-q4_K_M
```

Expected output on a tier 1 box:

```
  Inspecting cluster capacity...
  ✔   Detected tier 1 (CPU, 8 GiB), 11.2 GiB free across 1 node(s)

  Installing AI bundle on demo-cluster

  ...  Creating namespace
  ✔   Creating namespace
  ...  Installing Ollama
  ✔   Installing Ollama
  ...  Installing LibreChat
  ✔   Installing LibreChat
  ...  Waiting for Ollama to be ready
  ✔   Waiting for Ollama to be ready
  ...  Verifying Ollama loaded the model
  ✔   Verifying Ollama loaded the model
  ...  Waiting for LibreChat to be ready
  ✔   Waiting for LibreChat to be ready

  ✔  AI bundle installed
  Chat UI:   https://chat--demo-cluster.kipper.run
  Cluster API: http://ollama.kipper-ai.svc.cluster.local:11434/v1

  Use this Ollama for kip's own AI features (log analysis, Dockerfile generation)? [Y/n]: y

  ✔   kip AI client pointed at in-cluster Ollama (model: qwen2.5:3b-instruct-q4_K_M)
```

Before you open the chat URL, create your admin account. Open registration is disabled by default so a stranger cannot grab the chat UI between install and your first visit.

```bash
kip ai admin create \
  --email you@example.com \
  --name 'Your Name' \
  --password 'pick-a-strong-password'
```

The username defaults to the local part of your email if you don't pass `--username`. Once that succeeds, open the chat URL and log in with those credentials.

## Use it from your apps

Inside the cluster, your apps reach Ollama at:

```
http://ollama.kipper-ai.svc.cluster.local:11434/v1
```

It's OpenAI-compatible, so any client library works with `apiKey: "ollama"` (the value doesn't matter, Ollama ignores it).

**Python:**

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://ollama.kipper-ai.svc.cluster.local:11434/v1",
    api_key="ollama",
)

response = client.chat.completions.create(
    model="qwen2.5:3b-instruct-q4_K_M",
    messages=[{"role": "user", "content": "Summarise this changelog in one sentence."}],
)
print(response.choices[0].message.content)
```

**Node:**

```js
import OpenAI from "openai"

const client = new OpenAI({
  baseURL: "http://ollama.kipper-ai.svc.cluster.local:11434/v1",
  apiKey: "ollama",
})

const response = await client.chat.completions.create({
  model: "qwen2.5:3b-instruct-q4_K_M",
  messages: [{ role: "user", content: "Summarise this changelog in one sentence." }],
})
console.log(response.choices[0].message.content)
```

## Status and uninstall

```bash
kip ai status
```

```
  AI: enabled
  Provider: ollama
  Model: qwen2.5:3b-instruct-q4_K_M
  Ollama URL: http://ollama.kipper-ai.svc.cluster.local:11434

  In-cluster bundle:
    ✔  ollama: 1/1 ready
    ✔  librechat: 1/1 ready
  Chat URL: https://chat--demo-cluster.kipper.run
```

Removing the bundle wipes its data: model cache, chat history, MongoDB content, LibreChat credentials, and the `kipper-ai` namespace are all deleted.

```bash
kip ai uninstall
```

Re-running `kip ai install` afterwards starts fresh: a new admin must be created with `kip ai admin create`. To preserve data across an uninstall, take a blocking snapshot first with `kip ai backup --name pre-uninstall --wait` (see below). The bare `kip ai backup` command exits while the snapshot is still uploading, so always pair an uninstall with `--wait` or check `kip ai backup show` for `Completed` first.

## Upgrades

Re-running `kip ai install` against an existing bundle is an in-place upgrade. Ollama is pinned to the `Recreate` rollout strategy so the old pod terminates before the new one starts. That means a few seconds of chat downtime per upgrade, but it's the right tradeoff for a single-replica workload that loads several gigabytes of model weights into memory. A rolling update would briefly run two pods and OOM tier 1 nodes, or fight over the GPU on tier 3 and 4.

## Backup and restore

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

### Repairing orphan backup state

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

### If you wiped MinIO

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

## Quality reality check

A 7B model on CPU is good for short, focused questions. It's slow for long generations and weak at synthesis tasks that need to weave multiple inputs together. The fix for demanding workloads is more hardware (GPU node, larger model) rather than training a custom model.

Fine-tuning on your own data is rarely the right answer. It's the right tool in three narrow cases: matching a very specific writing voice, teaching the model proprietary jargon it has never seen, or training on thousands of clean question-answer pairs. None of those are normal "chatbot for my product" use cases. For those, retrieval-augmented generation against your existing docs is what you actually want.
