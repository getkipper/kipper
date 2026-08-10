# Getting Started

This guide walks you through installing Kipper on a fresh Linux server and deploying your first application. By the end, you will have a running Kubernetes cluster with automatic SSL and a web console.

## Prerequisites

- A Linux server with root SSH access (Ubuntu 20.04, 22.04, 24.04, 26.04, or Debian 11, 12)
- 2 vCPU / 2 GB RAM / 30 GB free disk minimum to install (4 vCPU / 8 GB / 80 GB realistic floor for a usable cluster)
- Ports 80, 443, and 6443 open
- An SSH key on your local machine

::: tip Any Linux VPS will work, but pick a generous one
Any cloud provider or hosting company that gives you a Linux VM with a public IP and root SSH access will work. The "minimum" line above is what the install command will accept; it isn't what makes Kipper pleasant to use. For a side-project box that will host an app or two, a database, and Kipper's own backups: pick **8 GB RAM, 4 vCPU, 80 GB SSD or larger**. If you're going to run the [AI Bundle](/en/ai), aim for **16 GB RAM, 4+ vCPU, 100+ GB SSD** at minimum. See [Installation → recommended sizing in practice](/en/installation#preflight-checks) for the full table.
:::

## Step 1: Install the CLI

**Quick install (Linux/macOS):**
```bash
curl -sL https://getkipper.com/install | sh
```

**Windows:**

Download `kip-windows-amd64.exe` from the [latest release](https://github.com/getkipper/kipper/releases), rename to `kip.exe`, and add the directory to your PATH.

::: tip Windows and kip install
All kip commands work natively on Windows. The one exception is `kip install`, which uses SSH to connect to a Linux server. For this command, use [WSL](https://learn.microsoft.com/en-us/windows/wsl/) or Git Bash.
:::

**Or build from source:**
```bash
git clone https://github.com/getkipper/kipper
cd kipper/kip && go build -o kip .
sudo mv kip /usr/local/bin/
```

Verify it works:
```bash
kip --version
```

## Step 2: Install the cluster

Point Kipper at your server's IP address. It will SSH in, run preflight checks, and install everything automatically.

```bash
kip install --host 203.0.113.10 --ssh-key ~/.ssh/id_ed25519 --admin-email admin@example.com
```

You will see output like this:

```
  Connecting to 203.0.113.10...
  ✔  Connected

  Running preflight checks...
  ✔  OS: ubuntu 24.04
  ✔  RAM: 3820MB available
  ✔  Disk: 35370MB available
  ✔  Ports: 80, 443, 6443 open
  ✔  Platform profile: small

  Auditing host security...
  ✔  No surplus services detected
  ✔  No existing host firewall

  Registering subdomain...
  ✔  Subdomain assigned: 203-0-113-10.kipper.run

  Installing cluster...
  ...  Installing k3s
  ✔  Installing k3s
  ...  Configuring firewall
  ✔  Configuring firewall
  ...  Registering Kipper CRDs
  ✔  Registering Kipper CRDs
  ...  Recording platform sizing profile
  ✔  Recording platform sizing profile
  ...  Installing Traefik ingress
  ✔  Installing Traefik ingress
  ...  Applying security hardening
  ✔  Applying security hardening
  ...  Configuring cert-manager
  ✔  Configuring cert-manager
  ...  Setting up storage
  ✔  Setting up storage
  ...  Installing KEDA autoscaler
  ✔  Installing KEDA autoscaler
  ...  Installing log aggregation (Loki)
  ✔  Installing log aggregation (Loki)
  ...  Installing metrics and dashboards (Prometheus + Grafana)
  ✔  Installing metrics and dashboards (Prometheus + Grafana)
  ...  Setting up backup and restore (Velero)
  ✔  Setting up backup and restore (Velero)
  ...  Creating kipper-system namespace
  ✔  Creating kipper-system namespace
  ...  Installing container registry (Zot)
  ✔  Installing container registry (Zot)
  ...  Configuring identity provider
  ✔  Configuring identity provider
  ...  Deploying console
  ✔  Deploying console

  Cluster ready.
  Console:    https://console--203-0-113-10.kipper.run
  Kubeconfig: /Users/you/.kip/clusters/203-0-113-10.kipper.run.yaml
  Admin:      admin@203-0-113-10.kipper.run
  Password:   02026a371f24a488a86e654cada6e1c6

  Save these credentials now. They will not be shown again.
  If lost, run: kip auth reset-password
```

## Step 3: Verify the cluster

```bash
kip status
```

```
  Cluster: 203-0-113-10.kipper.run
  Host:    203.0.113.10
  Config:  /Users/you/.kip/clusters/203-0-113-10.kipper.run.yaml

  Nodes:
    ✔  ubuntu-server-1    master   Ready    v1.34.5+k3s1

  Components:
    ✔  k3s              1 node(s)
    ✔  Traefik          1/1 replicas available
    ✔  cert-manager     1/1 replicas available
    ✔  Longhorn         1/1 replicas available
    ✔  Dex              1/1 replicas available
    ✔  Console API      1/1 replicas available
    ✔  Console          1/1 replicas available

  DNS resolvers:
    ✔  1.1.1.1, 8.8.8.8, 9.9.9.9
```

The DNS resolvers section reads the curated resolver file on the server and audits it. If someone hand-edits it into something the cluster can't use (an IPv6 entry, more than three nameservers, a hostname), if the entries drift from the set the cluster was configured with, or if a resolver stops accepting connections from the server, `kip status` warns you here before it turns into a DNS outage. `kip cluster dns repair` puts the configured resolvers back. The check is best-effort: if the server can't be reached over SSH, the section reports that it could not check instead of silently passing, and the rest of the status still prints.

## Step 4: Deploy your first app

```bash
kip app deploy --name hello --image nginx:latest --port 80
```

```
  Deploying hello...
  ✔  Deployment created
  ✔  Service created
  ✔  Ingress created
  ✔  Live at https://hello--203-0-113-10.kipper.run
```

Open the URL in your browser. You should see the nginx welcome page, served over HTTPS with a valid Let's Encrypt certificate.

## Step 5: Manage your app

```bash
# List all deployed apps
kip app list

# Stream logs
kip app logs hello

# Set environment variables
kip app env set hello LOG_LEVEL=debug

# Set a secret (prompts for hidden input)
kip app secret set hello DATABASE_URL

# Update the image
kip app update hello --image nginx:1.27

# Restart the app
kip app restart hello

# Delete the app
kip app delete hello
```

## Step 6: Add a database

```bash
kip service add postgres --name mydb
```

```
  Creating postgres service "mydb"...
  ✔  StatefulSet created
  ✔  Persistent storage provisioned
  ✔  Credentials generated

  Connection details:
    Host:     mydb.default.svc.cluster.local
    Port:     5432
    Username: kipper
    Password: a1b2c3d4e5f6...
    Database: app

  To bind to an app:
    kip service bind mydb <app>
```

Bind the database to your app so it receives connection details as environment variables:

```bash
kip service bind mydb hello
```

## Step 7: Open the console

Visit the console URL from the install output (e.g. `https://console--203-0-113-10.kipper.run`) and sign in with your admin credentials. The dashboard shows cluster health, nodes, deployed apps, and services.

## Step 8: Upgrade Kipper

When a new version of the Kipper console is available, upgrade with:

```bash
kip upgrade
```

This pulls the latest console images and restarts the system components. Your apps and services are not affected.

## What's next?

- [Deploy a real application](/en/deploying-apps) from a container image
- [Add a database or cache](/en/services) with persistent storage
- [Manage projects](/en/environments) to organize your apps
- [Configure secrets](/en/secrets) for database URLs and API keys
- [Share access with your team](/en/team-access) so developers can deploy and debug
- [Set up a custom domain](/en/domains) instead of using kipper.run
