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
- Minimum 2GB RAM, 30GB disk
- An SSH key

### Install

```bash
# Build the CLI (Homebrew coming soon)
cd kip && go build -o kip .

# Install the cluster
./kip install --host <your-server-ip> --ssh-key ~/.ssh/id_ed25519 --admin-email you@example.com
```

### Deploy

```bash
./kip app deploy --name hello --image nginx:latest --port 80
```

Your app is live at `https://hello-<cluster>.kipper.run` with a valid TLS certificate.

See the [Getting Started guide](docs/en/getting-started.md) for a complete walkthrough.

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
