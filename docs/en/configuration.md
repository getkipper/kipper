# Configuration

Kipper stores its configuration in `~/.kip/config.yaml`. This file is created automatically during `kip install`.

## Config file

```yaml
clusters:
  - name: production
    provider: baremetal
    host: 203.0.113.10
    domain: 203-0-113-10.kipper.run
    console_domain: kipper.example.com
    kubeconfig: ~/.kip/clusters/203-0-113-10.kipper.run.yaml
    gateway_token: 5b2bf14ef65250c82504a721c4353c2e...
    org: acme                      # optional, set via kip install --org
    org_display_name: Acme Inc

current_cluster: production

ai:
  provider: none
```

### Fields

| Field | Description |
|---|---|
| `clusters` | List of configured clusters |
| `clusters[].name` | Cluster identifier (rename with `kip cluster rename`) |
| `clusters[].provider` | Infrastructure provider (`baremetal`, future: `hetzner`, `digitalocean`, `aws`) |
| `clusters[].host` | Server hostname or IP address |
| `clusters[].domain` | Auto-generated kipper.run subdomain (used internally for app routing) |
| `clusters[].console_domain` | Custom console domain (set via `kip cluster domain`) |
| `clusters[].kubeconfig` | Path to the cluster's kubeconfig file |
| `clusters[].gateway_token` | Token for managing the kipper.run subdomain. The source of truth is the `gateway-credentials` Secret on the cluster; this local copy is the disaster-recovery fallback that lets you deregister the subdomain if the cluster itself is gone. Because of it, `config.yaml` is written readable by your user only (mode 0600) |
| `clusters[].dns_resolvers` | Resolvers CoreDNS forwards external queries to (set via `--dns-resolver` on install). Empty means the default public set. `kip status` warns when the file on the server drifts from this, and `kip cluster dns repair` restores it |
| `clusters[].org` | Organisation short code (optional), prefixes all namespaces |
| `clusters[].org_display_name` | Human-readable organisation name for the console |
| `current_cluster` | Which cluster `kip` commands target by default |
| `ai` | AI provider configuration (optional, all features disabled by default) |

## Kubeconfig

Each cluster's kubeconfig is stored in `~/.kip/clusters/<domain>.yaml`. Current installs use `kip auth kubectl-token` to authenticate as the signed-in operator. Kubernetes bindings determine that operator's permissions.

Older kubeconfigs may contain an admin certificate. Use [`kip auth kubeconfig`](/en/cli-reference#kip-auth-kubeconfig) to inspect and convert them, and protect any file that still contains credentials.

## Multiple clusters

Kipper supports managing multiple clusters from the same machine. After installing each cluster, they all appear in your config:

```bash
kip cluster list
```

```
    dev
      Console: https://console--203-0-113-10.kipper.run
      Server:  203.0.113.10

  → production
      Console: https://kipper.example.com
      Server:  198.51.100.1
```

The arrow (`→`) indicates the active cluster.

### Switching clusters

Switch the active cluster:

```bash
kip cluster use production
```

Partial name matching works if the name is unique:

```bash
kip cluster use prod
```

### Per-command override

Target a specific cluster for a single command without switching:

```bash
kip --cluster dev app list
```

Or set the `KIP_CLUSTER` environment variable:

```bash
export KIP_CLUSTER=dev
kip app list           # targets dev
kip service list       # targets dev
```

Resolution order: `--cluster` flag > `KIP_CLUSTER` env var > `current_cluster` in config.

### Renaming clusters

Cluster names default to the kipper.run domain, which can be unwieldy. Give them short memorable names:

```bash
kip cluster rename 203-0-113-10.kipper.run dev
kip cluster rename example.kipper.run production
```

After renaming, all commands use the short name:

```bash
kip cluster use production
kip --cluster dev app list
```

### Sharing cluster access

Export a cluster for a team member. The file carries no credential:

```bash
kip cluster export > production.kip
```

They import it on their machine:

```bash
kip cluster add production.kip --set-current
```

### Removing a cluster

```bash
kip cluster remove dev
```

This removes the cluster from your local config and deletes the stored kubeconfig. It does not affect the server.

### Telling the browser tabs apart

With several consoles open, every tab shows the same blue icon. Give each
cluster its own colour under **Settings → Appearance** in the console, and the
tab icon changes to match.

## Custom console domain

By default, the web console is available at `console--{domain}.kipper.run`. Move the whole serving identity, console, API, and login, onto your own domain:

```bash
kip cluster domain kipper.example.com --yes
```

The command derives three hosts from your domain (`console.kipper.example.com`, `console-api.kipper.example.com`, `dex.kipper.example.com`) and drives them through a no-lockout transition: the new hosts come up alongside the old ones, kip verifies them from outside with a valid certificate, and only then approves the single cutover that moves the login issuer. If verification fails, the old hosts keep serving and nothing changes. The [Domains](/en/domains#custom-console-domain) page shows the full expected output, the SSO acknowledgement flow, and the `--sync`, `--rollback`, and `--repair` modes.

::: tip
Point DNS A records for `console.`, `console-api.`, and `dex.` under your domain at the server before running the command. cert-manager issues the Let's Encrypt certificates once DNS resolves.
:::

## AI provider settings

Configure an AI provider to enable code assistance, log analysis, and diagnostics. You can use Claude, OpenAI, or a self-hosted Ollama server.

### Configure in the console

Open **Settings → AI Configuration**, choose a provider, enter the model and connection details, and click **Save**. The console stores these settings in the `kipper-ai-config` Kubernetes Secret in `kipper-system` and masks the API key when displaying it.

### Configure from the CLI

```bash
kip ai configure
kip ai status
```

The interactive setup saves the configuration in `~/.kip/config.yaml` and attempts to sync it to the current cluster. Check the output for a sync warning. Editing the local YAML alone changes the local configuration; use the console to update the cluster settings directly.

| Provider | CLI value | Required connection details |
|---|---|---|
| Claude (Anthropic) | `claude` | API key and model |
| OpenAI | `openai` | API key and model |
| Ollama | `ollama` | Server URL and model |

For Ollama, use an address reachable by the component making the request. In cluster settings, `localhost` refers to the console API container. See [AI Bundle](/en/ai) to run Ollama inside the cluster.

### Local AI configuration fields

These keys live under `ai` in `~/.kip/config.yaml`:

| Key | Purpose |
|---|---|
| `provider` | `claude`, `openai`, or `ollama`; `none` disables the local AI configuration |
| `api_key` | Provider credential for Claude or OpenAI |
| `model` | Model name |
| `ollama_url` | Ollama server address |

The config format also accepts `features.log_analysis`, `features.anomaly_detection`, and `features.dockerfile_generation`. These fields are currently unused: they do not control the console’s Analyse button or enable and disable individual AI features.

To disable AI through the CLI, run `kip ai configure` and choose **None**. This saves `provider: none` locally and attempts to sync it to the cluster. Check for sync warnings; editing the local file alone leaves the console’s settings unchanged.

### API key storage and use

CLI setup stores the API key in `~/.kip/config.yaml`, which Kipper writes with user-only permissions (`0600`). A successful sync also copies the key into the cluster’s `kipper-ai-config` Secret. Console setup stores it directly in that Secret.

The console API uses the key to authenticate requests to the selected AI provider. Those requests include the code, logs, or diagnostic context supplied by the feature you use. Protect both the local config and access to the cluster Secret, and review what you share with your provider.

## Resource management mode

Kipper automatically manages CPU and memory for your apps. A background controller monitors usage and adjusts allocations to match. It scales up under load, scales down when idle, and recovers from OOM kills.

See [Resource Management](/en/resource-management) for full details on how the auto controller works, resource profiles, expert mode, and the resource log.

## Slack notifications

Kipper can send alerts to a Slack channel when the auto controller makes resource changes, detects OOM kills, or clears stuck pods.

### Setup

1. Create a [Slack incoming webhook](https://api.slack.com/messaging/webhooks) for your channel
2. In the web console, go to **Settings** → **Slack**
3. Paste the webhook URL and click **Save**

Or configure via the API:

```
PUT /api/v1/settings/slack
{"webhook_url": "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"}
```

The webhook URL is stored as a Kubernetes secret (`kipper-slack` in the `kipper-system` namespace). The console displays a masked version for security.

### What gets sent

Every alert generated by the resource controller is forwarded to Slack with a severity indicator:

- **Green:** informational changes (scale down, profile defaults applied)
- **Yellow:** warnings (resource increases, stuck pod recovery)
- **Red:** critical events (OOM kills, emergency memory doubling)

To stop notifications, clear the webhook URL in Settings.
