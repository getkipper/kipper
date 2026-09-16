---
title: 'Install Kipper and deploy your first app'
description: 'Set up Kipper on a Linux server, sign in, and deploy a container with a public HTTPS URL.'
---

# Getting Started

This guide takes you from a fresh Linux server to a running app with HTTPS and a web console. Run the commands on your own computer. Kipper uses SSH to install the server, then APIs to deploy and manage your workloads.

To join a cluster someone has already installed, follow [Team Access](/en/team-access).

## Prerequisites

- **A supported server:** Ubuntu 20.04, 22.04, 24.04, or 26.04; or Debian 11 or 12, with a public IP address and root SSH access.
- **Capacity:** the installer requires at least 2 GB RAM and 30 GB free disk. Allow additional capacity for your apps and databases; see [sizing guidance](/en/installation#preflight-checks).
- **An SSH key:** install its public half on the server's root account. You should be able to connect with `ssh root@your-server`.
- **Provider firewall rules:** allow SSH and ports 80 and 443. Allow port 6443 from the addresses that will use the Kubernetes API, including your own computer.

Kipper configures the server's UFW firewall during installation. If an existing firewall is already active and managed separately, Kipper preserves it and reports that you need to maintain its rules. See [host security](/en/security#host-hardening) for details.

The free `*.kipper.run` address works with Kipper-managed DNS. To use your own domain at install time, configure its [DNS records](/en/installation#dns-for-a-domain-you-run) first.

## Step 1: Install the CLI

On Linux or macOS:

```bash
curl -sL https://getkipper.com/install | sh
kip --version
```

On Windows, follow [Installing from Windows](/en/windows). That guide uses WSL for installation and the native Windows CLI for daily work.

To build the CLI from source, see [Contributing](/en/contributing).

## Step 2: Install the cluster

Replace the example IP, key path, and email with your own:

```bash
kip install --host 203.0.113.10 --ssh-key ~/.ssh/id_ed25519 --admin-email admin@example.com
```

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

Check the app and stream its logs:

```bash
kip app list
kip app logs hello
```

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
