# Kipper Roadmap

Kipper is pre-release and moving quickly. This page is a living view of where the platform is heading, so you can see what's coming and help shape it.

It shows the direction of travel rather than dated promises. Priorities shift as the community weighs in, so the order here reflects current thinking rather than a schedule. Want something moved up, or something that isn't listed? [Open an issue](https://github.com/getkipper/kipper/issues) and tell us.

Everything below is planned unless it's tagged *in progress* or *exploring*. Several items make good first contributions.

---

## Where Kipper is today

The core platform is already usable end to end. You can install a production cluster on a Linux server in one command, deploy apps from an image or a Git repo, get automatic TLS and a public subdomain, and manage everything from the web console or the `kip` CLI.

Also shipped: projects with test, acc and prod promotion. Cross-project app links, letting an app reach another project's app over the cluster network with that project's consent, with `kip app links` to check one is actually carrying traffic. CI/CD webhooks with deploy history and rollback. GitOps through `kip apply`. Managed stateful services (Postgres, MySQL, MongoDB, Redis, MinIO, OpenSearch, RabbitMQ). Serverless functions that scale to zero. Scheduled jobs. Centralised logs and metrics with Loki, Prometheus and Grafana. Velero backups. Project-based RBAC. Per-app API keys. Autoscaling. A web terminal and file browser. And an optional AI assistant for log analysis and error diagnosis.

The rest of this page is what's next.

## Infrastructure and providers

Kipper installs on any Linux server over SSH today. Managed provisioning sits behind one interface, so new providers slot in without touching the core.

- **Hetzner Cloud.** Provision cluster nodes automatically.
- **DigitalOcean.** Provision cluster nodes automatically.
- **AWS.** Provisioning with VPC and IAM.
- **GCP and Azure.** Provisioning on the larger clouds. *Exploring.*
- **Managed load balancers.** Auto-provision a cloud load balancer per provider.

## Operating systems

Ubuntu and Debian are supported today. Broadening the install targets makes a good first contribution.

- **RHEL 9, Rocky Linux 9, AlmaLinux 9.**
- **Fedora**, latest two releases.
- **openSUSE and SUSE Linux Enterprise.**
- **Alpine Linux.** Minimal and container-native.

## Git providers

GitHub and GitLab, cloud and self-hosted, work today. More providers make good first contributions.

- **Gitea.**
- **Forgejo.**
- **Bitbucket.**

## Deployment and workflows

- **Auto Mode.** Hands-off resource and scaling tuning per workload profile, with a manual override when you want it. *In progress.*
- **Community blueprint registry.** Share and install full app stacks beyond the built-in blueprints, with versioning.
- **Automatic service binding.** Inject a linked service's connection string into an app without wiring it by hand.
- **Cross-project links in the console.** An app can already reach another project's app from the CLI, once that project's owner has agreed to it. The console shows those links and can remove them, and creating one still needs `kip app link`. This adds choosing an app from another project you can see, and granting or withdrawing that agreement as its owner, without leaving the console.
- **Multiple databases per instance.** Several logical databases inside one managed service.
- **Deploy any prebuilt image.** Import a local image or pull from any registry, with pull-secret handling and image-pull-policy control.
- **Promotion history.** A record of who promoted what, and when.

## Security and access

- **Headless `kip auth login` (device flow).** A browserless device-code login for CI and remote shells, holding a short-lived token instead of an admin kubeconfig. Today's `kip auth login` opens a browser.
- **Per-user scoped kubeconfigs.** Access limited to the projects you belong to.
- **Audit log.** Who did what, and when: deploys, secret changes, service binds.
- **Role-based secret visibility.** Mask service passwords from non-admins.
- **Strict Content Security Policy per app.** The policy Kipper attaches to your app allows inline scripts by default, because most apps need it. Opt a given app into a strict one when you know it can take it.
- **Per-environment roles.** Scope a role to a single environment.
- **Configurable token lifetimes.** Set ID and refresh token expiry to match your policy.
- **Network policies.** Isolate environments so test cannot reach prod. *Exploring.*
- **Image scanning.** Block deploys with critical vulnerabilities. *Exploring.*
- **Directory and SSO login.** Connect an external identity provider for larger teams. *Exploring.*
- **Secret rotation policies.** Scheduled rotation of managed secrets. *Exploring.*

## API gateway

Whole-app API-key gating with rate limits and quotas is shipped. Finer-grained gating is designed and reviewed.

- **Partial gating by path.** Require a key only on `/api`, rather than the whole app.
- **Partial gating by HTTP method.** Gate specific methods, so reads can stay open while writes need a key.
- **Standalone API gateway.** Compose an API at one host from several apps, with deny-by-default routing. Held back until there's real demand, and capped to stay simple. See the non-goals below. *Exploring.*

## Observability and operations

- **Cluster health page.** One screen showing the health of Traefik, Longhorn, KEDA, Velero and the rest.
- **External alerting.** Route alerts to Slack, email, or an on-call tool.
- **Log export.** Ship logs to an external system you already run. *Exploring.*
- **Multi-cluster management.** Manage more than one cluster from a single console, including deploying across clusters. *Exploring.*

## Upgrades

`kip upgrade` already updates CRDs, system components and the console layer. The goal is a fully reversible version upgrade.

- **Versioned image tags.** Pin components to explicit versions rather than `latest`.
- **`kip upgrade --version`.** Move to a specific version.
- **Pre-upgrade health check.** Refuse to upgrade an unhealthy cluster.
- **Rollback on failure.** Revert automatically if a new component fails to start.
- **`kip upgrade --self`.** Update the CLI binary.
- **Upgrade changelog.** Show what changed before you upgrade.

## AI (optional, always bring-your-own-key)

AI features run only with your own key and send data only where you've configured. Log analysis, error diagnosis, and an in-console coding assistant for functions are shipped. Next:

- **Self-hosted AI bundles.** One-command private LLM chat and chat-over-your-docs on your own cluster. *In progress.*
- **Resource recommendations.** Flag over-provisioned apps and idle workloads.
- **Dockerfile generation.** Generate a Dockerfile for a repo that has none.
- **Security scan.** Flag root containers, plaintext secrets, and known vulnerabilities.
- **Natural-language commands.** Run `kip` from a plain-English request.
- **Longer-horizon assistance.** Capacity planning, deployment summaries, and guided runbooks. *Exploring.*

## Scale and teams

- **Fuller multi-tenancy.** Organisation-scoped quotas and user management on top of today's org prefixing.
- **Terraform provider.** Manage Kipper apps and services as infrastructure-as-code.

## High availability

Single-node is the default and stays one command. For workloads that can't tolerate a server going down, an optional resilient tier is planned. It is opt-in and separate from the default path, so the quick start stays quick.

- **Three-node control plane.** k3s with an embedded-etcd quorum across three servers, so the cluster keeps running when any one node fails.
- **Replicated storage.** Longhorn at multiple replicas across nodes, so a failed node loses no data. Single-node keeps its single-replica storage.
- **Load-balancer failover.** A floating IP or managed load balancer in front of the control plane and ingress, so traffic rides through a node loss. Pairs with the managed load balancers above.

---

## What Kipper won't become

Some of these are the whole point. Kipper stays deliberately smaller than the enterprise platforms it competes with, and these are things we plan to keep out rather than build.

- **A policy DSL.** The routing layer stays declarative. No scripting or plugin pipelines, no request or response body transformation, no rule language to learn.
- **A heavyweight API gateway.** If a standalone gateway ships, it matches on paths, methods and headers and stops there. Token brokering, per-request mutation, and weighted-traffic policy stay out.
- **Cloud lock-in.** Kipper runs on a plain Linux server. Managed cloud providers are a convenience, never a requirement.
- **A multi-step setup.** From zero to production stays one command. New capabilities have to earn their place against that promise.

If a feature would make Kipper harder to run for a small team, it probably belongs on this list rather than the one above.

---

*This roadmap lives in the repository and changes with the project. The best way to influence it is to [open an issue](https://github.com/getkipper/kipper/issues) describing what you're trying to do. Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md).*
