---
title: 'Install Kipper and deploy your first app'
description: 'Set up Kipper on a Linux server, sign in, and deploy a container with a public HTTPS URL.'
---

# Getting Started

This guide takes you from a fresh Linux server to a running app with HTTPS and a web console. Run the commands on your own Linux, macOS or Windows computer. Kipper uses SSH to set up the server, then APIs to deploy and manage your workloads.

To join a cluster someone has already installed, follow [Team Access](/en/team-access).

## Prerequisites

- **A supported server:** Ubuntu 22.04, 24.04, or 26.04; Debian 12 or 13; or Rocky Linux, AlmaLinux or CentOS Stream 9 or 10, with a public IP address and root SSH access. Kipper also accepts Ubuntu 20.04 and Debian 11, with a warning that standard support has ended.
- **Capacity:** the installer requires at least 2 GB RAM and 30 GB free disk. Allow additional capacity for your apps and databases; see [sizing guidance](/en/installation#preflight-checks).
- **An SSH key:** add the public key to the server's root account. You should be able to connect with `ssh root@your-server`. On Windows, kip can create the key and add its public key to the server.
- **Provider firewall rules:** allow SSH and ports 80 and 443. Allow port 6443 from the addresses that will use the Kubernetes API, including your own computer.

Kipper configures the server's firewall during installation: UFW on Ubuntu and Debian, firewalld on Rocky Linux, AlmaLinux and CentOS Stream. Kipper preserves active firewalls you manage unless you grant permission to take over firewalld.

When installing over SSH from a terminal, `kip install` asks before taking over an eligible running firewalld and lists the proposed changes. With `--via-server`, including on Windows, the prompt appears on your computer before a new server job starts. Without a terminal, grant permission with `--adopt-firewall`. If you decline, Kipper preserves your firewall and checks required cluster access. Installation stops if required rules are missing or cannot be evaluated. See [host security](/en/security#host-hardening) for details.

The free `*.kipper.run` address works with Kipper-managed DNS. To use your own domain at install time, configure its [DNS records](/en/installation#dns-for-a-domain-you-run) first.

## Step 1: Install the CLI

::: code-group

```bash [Linux / macOS]
curl -sL https://getkipper.com/install | sh
kip --version
```

```powershell [Windows (PowerShell)]
irm https://getkipper.com/install.ps1 | iex
kip --version
```

:::

Both installers verify the download against the release's checksums. On Windows, the installer places kip in `%LOCALAPPDATA%\Programs\kip` and adds this folder to your user PATH. The default Windows installation requires no administrator rights. If `kip` is not found, open a new PowerShell window. See [Installing from Windows](/en/windows) for details.

To build the CLI from source, see [Contributing](/en/contributing).

## Step 2: Install the cluster

Replace the example IP, key path, and email with your own:

::: code-group

```bash [Linux / macOS]
kip install --host 203.0.113.10 --ssh-key ~/.ssh/id_ed25519 --admin-email admin@example.com
```

```powershell [Windows (PowerShell)]
kip install --host 203.0.113.10 --admin-email admin@example.com
```

:::

On Windows, unless you set `--ssh-key` or `KIP_SSH_KEY`, kip uses the default key at `%USERPROFILE%\.ssh\id_ed25519` and offers to create it if it is missing. If the server rejects the key, kip offers to add its public key through a separate SSH login, which may prompt for the root password. The installation runs on the server and continues if your connection drops.

Kipper checks the server, installs the platform, and prints the console URL and admin sign-in details. The free cluster address is derived from the server's IP; in this example it is `203-0-113-10.kipper.run`.

**Save the printed admin password.** Kipper stores its hash. If you lose the password, `kip auth reset-password` generates a replacement.

The final step opens a browser. Sign in with the admin address and password from the output so the installer can verify your Kubernetes access. If you skip sign-in, or install with `--no-login`, finish it later:

```bash
kip auth login
kip auth verify
```

## Step 3: Verify the cluster

```bash
kip status
```

Check that the node and platform components are ready. Components may take a little longer to become ready after installation; wait a minute and check again if needed.

The output also includes a DNS resolver audit. For resolver warnings, see [`kip cluster dns repair`](/en/cli-reference#kip-cluster-dns-repair).

## Step 4: Deploy your first app

```bash
kip app deploy --name hello --image nginx:latest --port 80
```

Open the HTTPS URL printed by the command. Once the deployment is ready, you should see the nginx welcome page.

Check the app and read its logs:

```bash
kip app list
kip app logs hello
```

Add `-f` to keep streaming new lines until you press Ctrl+C.

Visit the console URL printed during installation and sign in with the same account. The dashboard shows your cluster, apps, and services.

## Step 5: Try configuration and secrets

Save an environment variable or a secret:

```bash
kip app env set hello LOG_LEVEL=debug
kip app secret set hello EXAMPLE_TOKEN
```

The secret command prompts for hidden input. These commands save values for the app; they take effect when its pods restart:

```bash
kip app restart hello
```

Nginx does not use these example variables. Your own app can read them through its normal environment-variable API. See [Secrets & Environment Variables](/en/secrets) for templates, previews, and rollback.

## Step 6: Add a database when your app needs one

```bash
kip service add postgres --name mydb
kip service info mydb
```

Bind the service to an app that uses PostgreSQL:

```bash
kip service bind mydb myapp
```

Replace `myapp` with that app's name. The binding supplies connection details as environment variables. See [Stateful Services](/en/services) for supported services and binding names.

## Keeping Kipper up to date

```bash
kip upgrade
```

The command updates Kipper and offers to reconcile system components. System upgrades can briefly disrupt workloads, so review the confirmation before proceeding. See [upgrade options](/en/maintenance#kip-upgrade), including `--skip-system` for keeping component versions in place.

## What's next?

- [Deploy your application](/en/deploying-apps) from an image or Git repository.
- [Create projects and environments](/en/environments) for test and production.
- [Invite your team](/en/team-access) and assign project roles.
- [Configure backups](/en/backups) for recovery.
- [Use your own domain](/en/domains).

To remove the example app:

```bash
kip app delete hello
```
