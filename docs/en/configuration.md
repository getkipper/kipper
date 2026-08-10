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

Each cluster's kubeconfig is stored separately in `~/.kip/clusters/<domain>.yaml`. This file provides full admin access to the Kubernetes API.

::: warning
The kubeconfig grants full cluster admin access. Treat it like a root password.
:::

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

Export cluster credentials for a team member:

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

Kipper's AI features (code assistant, log analysis, diagnostics, and resource optimisation) are all optional and disabled by default. To enable them, configure an AI provider in the web console under **Settings** → **AI Configuration**, or edit the `ai` section in `~/.kip/config.yaml` directly.

### Supported providers

| Provider | `provider` value | Requirements |
|---|---|---|
| OpenAI | `openai` | API key, model name (e.g. `gpt-4o`) |
| Anthropic | `anthropic` | API key, model name (e.g. `claude-sonnet-4-20250514`) |
| Ollama (self-hosted) | `ollama` | Ollama URL, model name, no API key needed |

### Configuration example

```yaml
ai:
  provider: anthropic
  api_key: sk-ant-...
  model: claude-sonnet-4-20250514
  ollama_url: ""
  features:
    log_analysis: true
    anomaly_detection: true
    dockerfile_generation: true
```

For Ollama, set `provider: ollama` and provide the URL where Ollama is running:

```yaml
ai:
  provider: ollama
  api_key: ""
  model: llama3
  ollama_url: http://192.168.1.50:11434
  features:
    log_analysis: true
    anomaly_detection: true
    dockerfile_generation: true
```

### Feature flags

Each AI feature can be toggled independently:

| Feature | Description |
|---|---|
| `log_analysis` | Analyse button in log viewers (apps, functions, jobs) |
| `anomaly_detection` | Diagnose button and resource optimisation in app detail panels |
| `dockerfile_generation` | AI-assisted Dockerfile generation (planned) |

Set `provider: none` to disable all AI features. API keys are stored locally in `~/.kip/config.yaml` and are never sent to Kipper infrastructure.

### Settings page

The web console Settings page (gear icon in the sidebar) provides a UI for configuring the AI provider without editing YAML. Select your provider, enter the API key and model, toggle individual features, and click **Save**. Changes take effect immediately.

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
