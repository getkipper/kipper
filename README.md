<p align="center">
  <img src="console/public/logo.png" width="80" alt="Kipper" />
</p>

<h1 align="center">Kipper</h1>

<p align="center">
  Kubernetes for teams that ship.<br />
  One command to install. One command to deploy. Your infrastructure.
</p>

<p align="center">
  <a href="https://getkipper.com">Website</a> ·
  <a href="docs/en/getting-started.md">Getting Started</a> ·
  <a href="docs/en/architecture.md">Architecture</a>
</p>

<p align="center">
  <img src="docs/public/demo.gif" width="800" alt="A bare Ubuntu server becoming a Kubernetes cluster with an app serving over HTTPS, in two commands" />
</p>

<p align="center">
  <em>A bare server to an app on HTTPS. Recorded against a real machine, played back at 8x.</em>
</p>

---

## What is Kipper?

Kipper turns a Linux server into a Kubernetes platform with a web console, automatic HTTPS, and one-command app deployments. It brings together the tools teams need to run apps, functions, and databases on their own infrastructure, so you can focus on what you’re building.

Start with a container image and a server. Kipper handles the Kubernetes setup, routing, certificates, and storage.

## Features

- **One-command install.** Set up a supported Ubuntu, Debian, Rocky Linux, AlmaLinux or CentOS Stream server with k3s, Traefik, cert-manager, Longhorn, Dex, and a web console.
- **One-command deploy.** Deploy from a container image with automatic TLS and DNS.
- **Web console.** Dashboard with cluster health, app management, and real-time logs.
- **Free subdomains.** Get a `*.kipper.run` address with automatic HTTPS, or use your own domain.
- **Secrets management.** Separate commands for env vars and secrets, with hidden input.
- **Open source.** Apache 2.0, built on standard Kubernetes.

Kipper is pre-1.0. See [operating considerations](#operating-considerations) before running production workloads.

## Quick start

### Prerequisites

- An Ubuntu 22.04/24.04/26.04, Debian 12/13, or Rocky Linux, AlmaLinux or CentOS Stream 9/10 server with a public IP address and SSH access as root or as a user with passwordless sudo (`--ssh-user`). Kipper also accepts Ubuntu 20.04 and Debian 11, with a warning that standard support has ended.
- At least 4 GB RAM and enough disk space for Kipper's system volumes and images. Start with a 40 GB disk and allow more capacity for your workloads. See [sizing guidance](docs/en/installation.md).
- An SSH key authorized for the account you log in with: root, or the `--ssh-user` account. On Windows, kip can create the key and add its public key to the server
- Provider firewall rules allowing SSH, ports 80 and 443, and port 6443 from the computers that will manage the cluster

Run the commands below on your own computer.

### Install the CLI

Linux and macOS, on x86-64 and arm64:

```bash
curl -sL https://getkipper.com/install | sh
```

Windows, in PowerShell:

```powershell
irm https://getkipper.com/install.ps1 | iex
```

Both installers download the binary for your platform from the [latest release](https://github.com/getkipper/kipper/releases/latest) and check it against the published checksums. On Linux and macOS, the installer places `kip` in `/usr/local/bin`. On Windows, it uses `%LOCALAPPDATA%\Programs\kip` and adds this folder to your user PATH. See [Installing from Windows](docs/en/windows.md) for details.

### Install the cluster

Replace the example IP, key path, and email with your own:

```bash
kip install --host 203.0.113.10 --ssh-key ~/.ssh/id_ed25519 --admin-email you@example.com
```

On Windows, omit `--ssh-key` and leave `KIP_SSH_KEY` unset to use the default key at `%USERPROFILE%\.ssh\id_ed25519`. kip offers to create the key if it is missing and add its public key to the server if needed.

```powershell
kip install --host 203.0.113.10 --admin-email you@example.com
```

Save the console URL and admin password printed by the installer. Complete the browser sign-in when prompted, then check the cluster:

```bash
kip status
```

### Deploy your first app

```bash
kip app deploy --name hello --image nginx:latest --port 80
```

Open the HTTPS URL printed by the command. Once the deployment is ready, you should see the nginx welcome page at `https://hello--<cluster>.kipper.run`. You can also manage the app through the web console.

See the [Getting Started guide](docs/en/getting-started.md) for a complete walkthrough, or [CONTRIBUTING.md](CONTRIBUTING.md) to build from source.

## Architecture

Everyday workload management—deploying apps and functions, managing services, and viewing logs—uses the Kubernetes and console APIs. SSH handles installation, upgrades, node provisioning, and host maintenance or recovery.

Free `*.kipper.run` addresses route through the Kipper gateway to your cluster. Your own domains route directly to the cluster's Traefik ingress.

Kipper installs [k3s](https://k3s.io) with opinionated defaults:

| Component | Purpose |
|---|---|
| k3s | Lightweight Kubernetes |
| Traefik | Ingress and routing |
| cert-manager | Automatic Let's Encrypt TLS |
| Longhorn | Persistent storage |
| Dex | Authentication (OAuth2/OIDC) |

See [Architecture](docs/en/architecture.md) for the components and request flows.

## Operating considerations

**Plan for upgrades and recovery.** Create and verify a backup before upgrading. `kip upgrade` updates Kipper and can update system components, which may briefly disrupt workloads. System component upgrades have no automatic rollback; see [Upgrades & Maintenance](docs/en/maintenance.md) and [Backups](docs/en/backups.md).

**Deploy images from a registry or build from Git.** Push locally built images to a registry the cluster can reach, and use `kip registry add` for private registry credentials. With `kip app deploy --git`, Kipper builds and stores the image inside the cluster. See [Deploying Apps](docs/en/deploying-apps.md).

**Organize shared data by project and environment.** Apps and functions in the same project and environment can bind to the same database service. To share data across projects, expose it through an app API and connect the apps with [cross-project links](docs/en/routing.md).

See the [roadmap](ROADMAP.md) for planned work, and [open an issue](https://github.com/getkipper/kipper/issues) to report a problem or suggest an improvement.

## Explore the documentation

- [Getting Started](docs/en/getting-started.md): install, sign in, and deploy your first app.
- [Deploying Apps](docs/en/deploying-apps.md) and [Functions](docs/en/functions.md): ship your code.
- [Stateful Services](docs/en/services.md): add databases and other services.
- [Team Access](docs/en/team-access.md): invite teammates and assign roles.
- [CLI Reference](docs/en/cli-reference.md): find commands for daily work.

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

See the [FAQ](docs/en/faq.md) for the project’s approach to licensing and funding.

Maintained by [Labb Consulting](https://labb-consulting.com). Built for everyone.
