<p align="center">
  <img src="console/public/logo.png" width="80" alt="Kipper" />
</p>

<h1 align="center">Kipper</h1>

<p align="center">
  Kubernetes for teams that ship. From zero to production in one command.<br />
  No Kubernetes expertise required.
</p>

<p align="center">
  <a href="https://getkipper.com">Website</a> ·
  <a href="docs/en/getting-started.md">Getting Started</a> ·
  <a href="docs/en/architecture.md">Architecture</a>
</p>

---

## What is Kipper?

Kipper gives you a production-ready Kubernetes cluster on standard Linux infrastructure, with a web console, automatic SSL, and one-command app deployments. It's for small and mid-sized teams, agencies, SaaS products, internal platforms, and independent operators who want production Kubernetes without enterprise platform complexity.

```bash
# Install a cluster
kip install --host 203.0.113.10 --ssh-key ~/.ssh/id_ed25519

# Deploy an app
kip app deploy --name api --image ghcr.io/acme/api:latest --port 3000

# Check status
kip status
```

No Helm charts. No YAML manifests. No PhD required.

## Features

- **One-command install.** SSH into any Linux server and get k3s, Traefik, cert-manager, Longhorn, Dex, and a web console.
- **One-command deploy.** Deploy from a container image with automatic TLS and DNS.
- **Web console.** Dashboard with cluster health, app management, and real-time logs.
- **Free subdomains.** Every cluster gets `*.kipper.run` with automatic SSL.
- **Secrets management.** Separate commands for env vars and secrets, with hidden input.
- **Multi-node.** Add worker nodes with `kip node add`.
- **Open source.** Apache 2.0, no vendor lock-in.

## Quick start

### Prerequisites

- A Linux server (Ubuntu 20.04/22.04/24.04/26.04 or Debian 11/12) with root SSH access
- 2 vCPU / 2 GB RAM / 30 GB free disk to install. For a cluster you will actually use, pick 4 vCPU / 8 GB / 80 GB
- An SSH key

### Install the CLI

```bash
curl -sL https://getkipper.com/install | sh
```

Downloads the binary for your platform from the [latest release](https://github.com/getkipper/kipper/releases/latest), checks it against the published checksums, and puts `kip` in `/usr/local/bin`. Linux and macOS, on x86-64 and arm64.

On Windows, run this inside [WSL](https://learn.microsoft.com/en-us/windows/wsl/), which `kip install` needs because it shares one SSH connection across hundreds of commands and Windows OpenSSH cannot. Every other command works from the native `kip-windows-amd64.exe` in the same release.

### Install the cluster

```bash
kip install --host <your-server-ip> --ssh-key ~/.ssh/id_ed25519 --admin-email you@example.com
```

### Deploy

```bash
kip app deploy --name hello --image nginx:latest --port 80
```

Your app is live at `https://hello-<cluster>.kipper.run` with a valid TLS certificate.

See the [Getting Started guide](docs/en/getting-started.md) for a complete walkthrough, or [CONTRIBUTING.md](CONTRIBUTING.md) to build from source.

## Architecture

```
User → kip CLI → SSH (install only) / K8s API (everything after)
Browser → Caddy (TLS) → Gateway (proxy) → Cluster (Traefik → App)
```

Kipper installs [k3s](https://k3s.io) with opinionated defaults:

| Component | Purpose |
|---|---|
| k3s | Lightweight Kubernetes |
| Traefik | Ingress and routing |
| cert-manager | Automatic Let's Encrypt TLS |
| Longhorn | Persistent storage |
| Dex | Authentication (OAuth2/OIDC) |

See [Architecture](docs/en/architecture.md) for the full technical deep-dive.

## Known limitations

Kipper is pre-release. These are the edges a new user is most likely to meet in the first week, and each one has a way around it today.

**Upgrades run forwards only.** `kip upgrade` moves a cluster to the current release. You cannot pin a version, nothing refuses to upgrade an unhealthy cluster, and a component that fails to start is not rolled back for you. Take a backup with `kip backup create` before you upgrade.

**Images come from a registry.** `kip app deploy --image` pulls from a registry, and `kip registry add` covers private ones. There is no way to import an image you built on your own machine and no control over the pull policy, so a local `docker build` has to be pushed somewhere the cluster can reach. Deploying from git with `kip app deploy --git` sidesteps this, because Kipper builds the image in the cluster and stores it itself.

**A database belongs to one project.** Apps and functions in the same project and environment bind to the same service, and another project gets its own instance. Cross-project links join one app to another app rather than to a service, so sharing data across projects means putting an app in front of the database, or keeping those workloads in one project.

See the [roadmap](ROADMAP.md) for where these are going, and [open an issue](https://github.com/getkipper/kipper/issues) if you hit something that is not listed.

## Repository structure

| Directory | Language | What it does |
|---|---|---|
| `kip/` | Go | CLI tool |
| `console/` | Vue 3 + TypeScript | Web dashboard |
| `console-api/` | Go | REST API for the console |
| `gateway/` | Go | kipper.run subdomain proxy |
| `docs/` | VitePress | Documentation |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, coding standards, and PR guidelines.

## License

Apache 2.0. See [LICENSE](LICENSE).

Maintained by [Labb Consulting](https://labb-consulting.com). Built for everyone.
