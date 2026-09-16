# AI Bundle

Run Ollama and a web chat interface in your cluster with one command. The bundled model runs on your own hardware.

The bundle installs two things:

- **Ollama** serves the language model and exposes an OpenAI-compatible API.
- **LibreChat** is the web chat UI on `chat.<your-domain>`.

After installation, chat in your browser or configure your apps to use Ollama’s API endpoint.

## What hardware do you need?

`kip ai install` selects a default model using the available memory and GPU capacity on the best eligible node. A fresh installation requires at least **8 GiB of free memory on one node**.

| Tier | Free RAM on selected node | Available GPU | Default model |
|---|---|---|---|
| 1 | 8 GiB | CPU only | Qwen 2.5 3B Q4 |
| 2 | 16 GiB | CPU only | Qwen 2.5 7B Q4 |
| 3 | 16 GiB | NVIDIA GPU | Qwen 2.5 7B Q4 |
| 4 | 16 GiB | NVIDIA GPU with at least 16 GiB VRAM | Qwen 2.5 14B Q4 |

The bundle requests one GPU per pod. Tier 4 requires the VRAM threshold on a single device.

Leave capacity for your apps, system components, and backups as well as the model. Response time depends on the model, hardware, and workload; measure it with your expected requests. The first installation also downloads model weights from Ollama’s registry.

### Model cache and snapshot sizing

The AI bundle's PVCs are typically dominated by the Ollama model cache:

| Model | Cache size on disk |
|---|---|
| qwen2.5:3b-instruct-q4_K_M | ~2.4 GB |
| qwen2.5:7b-instruct-q4_K_M | ~5 GB |
| qwen2.5:14b-instruct-q4_K_M | ~10 GB |
| qwen2.5:32b-instruct-q4_K_M | ~22 GB |

`kip ai install` requests model-cache storage through the cluster’s default storage class. `--pvc-size` defaults to 10 GiB for tier 1, 30 GiB for tier 2, and 60 GiB for tiers 3 and 4. Actual disk requirements also depend on the storage class’s replication and snapshot settings.

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

Use the OpenAI-compatible endpoint as shown below. The examples supply `ollama` as the client library’s API key placeholder.

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

Re-running `kip ai install` afterwards starts fresh: a new admin must be created with `kip ai admin create`. To preserve data across an uninstall, take a blocking snapshot first with `kip ai backup --name pre-uninstall --wait` (see [AI Backup & Recovery](/en/ai-backups)). The bare `kip ai backup` command exits while the snapshot is still uploading, so always pair an uninstall with `--wait` or check `kip ai backup show` for `Completed` first.

## Upgrades

Re-running `kip ai install` updates the existing bundle. Ollama uses a `Recreate` rollout: the old pod stops before the new one starts, allowing the replacement to use its memory and GPU. Chat is unavailable while the replacement starts and loads the model.

## Protecting AI data

<span id="backup-and-restore"></span>
<span id="repairing-orphan-backup-state"></span>
<span id="if-you-wiped-minio"></span>

See [AI Backup & Recovery](/en/ai-backups) for snapshots, restores, and backup-state repair.

## Choosing a model

Start with the model selected for your cluster, then evaluate it with representative prompts. Check answer quality, response time, and memory use before changing `--model`. Larger models also need more storage and backup capacity.
