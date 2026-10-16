# FAQ

## General

### What is Kipper?

Kipper is an open source platform that installs and manages Kubernetes on Ubuntu, Debian, Rocky Linux, AlmaLinux and CentOS Stream servers. It includes a web console, automatic TLS, persistent storage, and commands for deploying apps.

### Does Kipper collect telemetry?

No. Kipper sends no telemetry of its own, and it switches off the usage reports and update checks of the components it bundles. Apart from image and chart downloads, certificate renewals and DNS lookups, the only thing a cluster sends without being set up to is the daily kipper.run gateway heartbeat, and only on a cluster that holds a kipper.run name. See [what leaves your cluster](/en/security#what-leaves-your-cluster).

### Who is Kipper for?

Kipper is for teams and independent operators who want to run apps on their own Kubernetes infrastructure. It brings installation, deployment, and day-to-day management into one CLI and console.

### How is Kipper different from managed Kubernetes (EKS, GKE)?

Managed Kubernetes gives you the cluster but leaves you to figure out ingress, TLS, storage, auth, and deployment workflows. Kipper handles all of that with opinionated defaults. It also runs on any server you own, so you are not locked into a cloud provider.

### Is Kipper production-ready?

Kipper is in active development. The core install, deploy, and management commands work on real infrastructure, and we use it for staging environments and early-stage production workloads. It has not been battle-tested at scale.

### How is this funded, and could the license change?

Kipper is Apache 2.0, and that grant is irrevocable for every version already published. A release you run today stays yours to use, fork and redistribute, whatever happens later.

Funding is an open question. Kipper is written by one person and earns nothing today. Should that change, the likely shape is paid hosting or support around the same open codebase, with shipped features staying under the same license. Treat that as the current intention; the paragraph above is the part to rely on.

### Does kip work on Windows?

Yes. Download `kip-windows-amd64.exe` from the releases page and run `kip install` from PowerShell. kip runs the installation on the server and streams its progress over SSH. You can also deploy apps, manage secrets, view logs, and scale workloads from PowerShell. See [Installing from Windows](/en/windows).

One small difference: in `kip exec`, a shell keeps drawing to the size the window had when the session opened, because Windows reports resizes as console input rather than as a signal. Resize before you connect, or reconnect after.

## Installation

### What servers are supported?

Any Linux server with root SSH access running Ubuntu 22.04, 24.04 or 26.04, Debian 12 or 13, or Rocky Linux, AlmaLinux or CentOS Stream 9 or 10. Kipper also accepts Ubuntu 20.04 and Debian 11, with a warning that standard support has ended. Minimum 2GB RAM and 30GB disk.

### Can I install on a cloud VM?

Yes. Any cloud provider that gives you a Linux VM with a public IP and root SSH access will work.

### Can I install on ARM?

The `kip` CLI itself runs fine on ARM client machines (Linux arm64 and Apple Silicon). The cluster nodes you install onto need to be x86/amd64 today, because a few of the bundled component images are pinned to `linux/amd64`.

### What other Linux distributions are supported?

Ubuntu and Debian, plus Rocky Linux, AlmaLinux and CentOS Stream 9 and 10. On the latter three, Kipper configures firewalld and enables k3s's SELinux support while preserving the host's SELinux mode. Red Hat Enterprise Linux itself, Fedora, openSUSE and Alpine Linux are not supported yet.

### I lost my admin password

Run `kip auth reset-password` to generate a new one.

### Can I roll back an upgrade?

Not automatically. `kip upgrade` moves the cluster to the current release, and there is no version pinning, no check that refuses an unhealthy cluster, and no automatic revert when a component fails to start. Run `kip backup create` before upgrading so you have a restore point.

### Can I re-run kip install?

Yes, but not as a general way to catch a cluster up. Parts of it are idempotent and parts of it replace state.

It is the documented way to rotate backup credentials, and it moves k3s and cert-manager onto the versions your `kip` pins. It also re-renders the Dex configuration from install-time values, which removes every user account created through the console since, and it re-runs the rest of the install.

For routine updates use `kip upgrade`, which is designed for a running cluster. See [what an upgrade moves](/en/maintenance#what-an-upgrade-moves-and-what-it-does-not) for the full list of what each one covers.

## Apps

### What can I deploy?

Any application packaged as a Docker container image. If it listens on a port, Kipper can deploy it.

### How do I update a deployed app?

Use `kip app update` to change the image and trigger a rolling update:

```bash
kip app update backend --image ghcr.io/acme/api:v2.1.0
```

You can also update the image from the web console using the package icon in the app detail panel.

### Can I deploy an image I built locally?

Push the image to a registry the cluster can reach, then use `kip app deploy --image`. For private registries, add credentials with `kip registry add`. Alternatively, `kip app deploy --git` builds the image in the cluster.

### Can two projects share one database?

Apps and functions bind to services in their own project and environment. To share a database, place the workloads in the same environment, or expose a data API through an app and use [cross-project links](/en/routing#linking-across-projects) to reach it.

### Where are my secrets stored?

In Kubernetes Secrets, encrypted at rest by k3s. They are scoped to the project namespace and never returned in API responses unless explicitly requested.

### How do I scale an app?

```bash
kip app scale backend --replicas 3
```

The `READY` column in `kip app list` shows scaling progress. You can also scale via the web console's Scale tab.

### Can I run multiple services under one domain?

Yes, use the `--route` flag to group services by path:

```bash
kip app deploy --name frontend --image img --port 80 --route myapp/
kip app deploy --name backend --image img --port 3000 --route myapp/api
```

Both share `myapp--<cluster>.kipper.run` (or `myapp.<your-domain>` on a custom domain).

### I accidentally set the wrong secret value

Run `kip app secret rollback backend SECRET_KEY` to restore the previous value. Kipper automatically keeps the previous version of every secret.

### Can I use a private container registry?

Yes. Add the credentials once and grant the projects that may use them:

```bash
kip registry add --server ghcr.io --username acme-ci --password ghp_R2h4K9pLmN3qWtX7 --allow-project acme
```

You can also add them in the console under Settings. Kipper stores the credential in `kipper-system` and stages a pull secret for a workload when its project is on the credential's allow-list and its image comes from that registry. From then on you deploy private images the same way you deploy public ones, with nothing extra in your app configuration.

### How do I add a database?

```bash
kip service add postgres --name mydb
```

This creates a PostgreSQL instance with persistent storage and auto-generated credentials. See [Stateful Services](/en/services).

### How do I upgrade Kipper?

```bash
kip upgrade
```

This updates the console and reconciles system components. System component upgrades can briefly disrupt workloads. See the [upgrade reference](/en/maintenance#kip-upgrade) for scope and options.

### What does "stopped" mean?

An app scaled to 0 replicas. It still exists (deployment, service, ingress are preserved) but no pods are running. Start it again with `kip app scale <name> --replicas 1`.

## Networking

### How do subdomains work?

A wildcard DNS record points `*.kipper.run` to the Kipper Gateway. The gateway looks up which cluster owns the subdomain and proxies the request. Your app gets a URL like `myapp--203-0-113-10.kipper.run` automatically.

### Can I use my own domain?

Yes. The cleanest path is to pass `kip install --domain yourdomain.com` at install time. That makes the cluster's wildcard (`*.yourdomain.com`) the default for new app routes. Without `--domain`, clusters use a generated `*.kipper.run` subdomain.

After install, `kip cluster domain yourdomain.com` moves the whole serving identity, console, API, and login, onto your domain through a no-lockout cutover. Per-app custom domains are set on each route from the Routes panel. See the [Domains](/en/domains) page for the full DNS setup.

### Is traffic encrypted?

Yes. For `*.kipper.run` routes, TLS is terminated at the kipper.run gateway using a Let's Encrypt wildcard certificate, and the gateway-to-cluster hop also uses HTTPS. For routes on a custom domain, traffic goes directly to your cluster and Traefik terminates TLS in-cluster using a per-host Let's Encrypt certificate issued by cert-manager.
