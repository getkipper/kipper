---
name: kipper
description: Expert use of Kipper, the open-source one-command Kubernetes platform, through its kip CLI. Covers installing a cluster, deploying and managing apps, stateful services, functions, jobs, projects and environments, GitOps with kipper.yaml, credentials, backups, upgrades, and knowing when to use kip versus kubectl. Use for any task involving Kipper, the kip command, a kipper.yaml manifest, or a *.kipper.run cluster.
---

# Kipper expert

Kipper turns a plain Linux server into a production-ready Kubernetes cluster with one command. It ships a web console, automatic Let's Encrypt TLS, a free `*.kipper.run` subdomain, and one-command app deploys, so a team can go from zero to production without Kubernetes expertise. It is open source (Apache 2.0), maintained by Labb Consulting, and lives at https://github.com/getkipper/kipper with docs at https://getkipper.com.

Everything a user does goes through the `kip` CLI, the web console, or a `kipper.yaml` manifest. Under the hood `kip` creates Custom Resources and the console-api reconcilers turn them into native Kubernetes objects.

## Golden rules

- **Prefer `kip` over `kubectl`.** Kipper models apps, services, functions, jobs, secrets, backups, and more as its own resources. Manage them with `kip`, never with `kubectl apply`/`kubectl create` or raw manifests. If `kip` can't do something, that's a missing feature, not a reason to reach around it.
- **`kubectl` is a read-and-debug escape hatch.** Use the kubeconfig to inspect state (`kubectl get`, `describe`, `logs`), read a Secret, or run a third-party controller (ArgoCD/Flux syncing Kipper CRs is supported). Don't use it to create or mutate resources Kipper owns.
- **`kip auth login` needs a browser.** It opens a Dex OAuth flow. An automated agent cannot complete it. If a command reports `session expired — run: kip auth login` or `not authenticated — run: kip auth login`, ask the user to run `kip auth login` themselves, then continue. **Not every auth failure is that one.** When console-api answers 401 to a token that is still locally valid, the message says so — *"your session is not expired"* — and names the `kip` and console-api versions, because two different causes give the same 401: a CLI/cluster version skew, or the cluster's login being reconfigured. A fresh login fixes the second (the new token comes from the reconfigured issuer) and will not fix the first. The message itself stops short of choosing, and so should you: read which of the two messages came back, and if it is this one, check the versions it prints before assuming the browser is the answer.
- **Verify before you pin.** Image tags, chart versions, and ports change. Read them from source (a repo's Dockerfile `EXPOSE`, the release page) rather than guessing.
- **Credentials go in secrets, config goes in env.** Use `kip app secret set <app> KEY` for API keys, tokens, passwords, and connection strings that embed credentials; use `--env` / `kip app env set` only for non-sensitive config (log levels, URLs, feature flags). Env values live on the App CR in `spec.env`: they print in full in `env list`, show plain in the console, and land in every `kip export` and committed `kipper.yaml`. Secrets stay in the `app-<app>-secrets` Secret, masked everywhere, and never appear in an export. `kip app deploy` takes `--secret KEY=VALUE` (or a bare `KEY` for a hidden prompt), so credentials are in place before the first start; `kip app secret set` covers changes later, saving the new value and leaving the running pods on the old one until `--restart` or `kip app restart <app>`.

## Mental model

- **Project** groups related apps and services. It has a short code (used in commands and namespaces) and a display name, and is backed by a Project resource that is the source of truth. Deleting a project cascades.
- **Environment** is a promotion stage inside a project, conventionally **test → acc → prod**. Each environment is its own isolated Kubernetes namespace.
- **Namespace mapping:** `<project>-<environment>` (for example `blog-prod`). If an org prefix was set at install with `--org acme`, it becomes `<org>-<project>-<environment>` (for example `acme-blog-prod`). **Two things suppress the suffix, and one suppresses the prefix.** An environment named `default` takes the bare project name, so `blog` rather than `blog-default` — and `default` is what a project gets when created without `--environments`. **An omitted or empty environment does the same**, so a command run without `--environment` and with no active one set resolves to the bare project namespace rather than erroring, which on a project whose environments are all named lands you somewhere that may not exist. Separately, the org is not prepended twice: a project already named `acme-deck` on an `acme` cluster resolves to `acme-deck-prod`, not `acme-acme-deck-prod`, which is where a migrated project usually lands. Pointing kubectl at any of the wrong forms finds nothing and reads as a missing app.
- **Apps** are Deployment + Service + Ingress, deployable from a container image, a Git repo (built in-cluster), or a CI push. **Services** are stateful workloads (Postgres, MySQL, MongoDB, Redis, RabbitMQ, MinIO, OpenSearch, MailHog) run as StatefulSets. **Functions** are scale-to-zero HTTP or event-triggered workloads (via KEDA). **Jobs** are one-off or scheduled (cron) tasks.
- Each cluster gets a free `*.kipper.run` subdomain with automatic TLS. Apps are reachable at a `*.kipper.run` host or a custom domain you point at the server.

## How state works (and how to debug drift)

The App resource (a `kipper.run` Custom Resource) is the single source of truth. console-api keeps **no separate database** — the console reads and writes the App CR directly. So "the console shows X" means the CR's *desired* spec is X. That is not proof X is *running*. Two indirections cause almost all "it looks set but isn't working" confusion:

- **Env vars reach a pod indirectly.** `kip app env set` writes `spec.env` on the App CR; the reconciler resolves the whole environment and publishes it as one immutable Secret named for a digest of its contents, `app-<app>-env-<digest>`; the pod names that exact object in `envFrom` and loads it at startup. A change publishes a new generation under a new name rather than rewriting the old one, so a pod reads one environment or another and never a mixture. A running pod reads its env **once, at start**, so a live pod goes on naming the generation it started with and keeps those values until it restarts. Nothing restarts it for you: `kip app env set` saves the change and says so, and `--restart` (or `kip app restart <app>`) is what applies it. The console shows `spec.env` (desired), never the pod's live env.
- **A git app's running image is build output, not manifest state.** For a git-built app the desired state is `spec.git` (repo + branch). An in-cluster build produces an image, pushes it to the in-cluster Zot registry, and records its tag in `spec.image`. Treat that image as controller-owned: don't hand-edit it, don't delete it from a manifest, don't expect it in a hand-written `kipper.yaml`.

To check whether config is actually **live**, read the rendered Secret and the pod — not the CR or the console, which both show desired state:

```bash
export KUBECONFIG=~/.kip/clusters/<name>.yaml
# which generation each running pod names — ask the pods, not the Deployment:
# mid-rollout the template already names the new one while old pods still serve
# the previous, and that gap is usually the thing being investigated.
kubectl get pods -n <project>-<env> -l app=<app> -o go-template='{{range .items}}{{.metadata.name}} {{range (index .spec.containers 0).envFrom}}{{.secretRef.name}}{{end}}{{"\n"}}{{end}}'
# and what is in one:
kubectl get secret <generation> -n <project>-<env> -o go-template='{{range $k,$v := .data}}{{$k}}={{$v|base64decode}}{{"\n"}}{{end}}'
# what that pod actually has — name the pod rather than deploy/<app>, which
# picks one for you and may not be the one whose generation you just read:
kubectl exec -n <project>-<env> <pod> -- printenv | sort
# did the pod start before or after your change?
kubectl get pod -n <project>-<env> -l app=<app> -o wide
```

If the Secret has your value but the pod doesn't, the pod just needs a restart (`kip app restart <app>`). Do **not** conclude "something reverted my change" from `kubectl --show-managed-fields` showing `manager: console-api` on `f:spec` — that only means the reconciler owns those fields, not that a hidden store is overwriting you. There is no hidden store.

## Cluster components

`kip install` provisions: k3s (Kubernetes), the Kipper CRDs, Traefik (ingress), cert-manager (Let's Encrypt), Longhorn (storage), Dex (identity), Zot (in-cluster registry), KEDA (scale-to-zero), the API-key gateway (kipper-authz), Velero (backup/restore), and the Console plus console-api. Observability (Loki, Prometheus, Grafana) scales with the sizing profile and is turned off on the smallest (nano) installs. Host hardening and a firewall are applied by default.

## Orientation and config

- Local config lives in `~/.kip/`: `config.yaml` (clusters + kubeconfigs), `auth.json` (Dex session tokens). Per-cluster kubeconfigs are under `~/.kip/clusters/<name>.yaml`.
- Select a cluster: `kip cluster list`, then `kip cluster use <name>`. Override per command with `--cluster <name>` or the `KIP_CLUSTER` env var.
- Set a default project/environment for the active cluster with `kip project use <project>[/<env>]`. **Then pass `--project` and `--environment` anyway on anything that writes.** The saved context is honoured by some commands and ignored by others, the split does not follow the command tree, and the failure is silent: a command that ignores it does not say so. These are the behaviours that have been traced against source; treat the grouping as the commands that were checked rather than a rule you can extrapolate from:
  - *Honours it* — `kip app list`; the `service` and `job` commands; `kip function create/list/bind/unbind`; `kip volume create/list/delete`.
  - *Honours the saved project but not the environment* — `kip project allow-links` / `links`.
  - *Falls back to the literal project `default`* — **`kip app deploy`, `kip app rebuild`**. With `shop/prod` active a flagless deploy creates the app in `default`.
  - *Searches every namespace for the workload by name, refusing an ambiguous match* — most other app- and function-scoped commands (`logs`, `restart`, `update`, `delete`, `scale`, `env`, `secret`, …) **and `kip volume mount` / `unmount`**, which are the two volume commands that act on an app rather than on the volume alone. They find it wherever it lives and stop rather than guess.
  - *Honours it, and refuses an ambiguous match* — **`kip exec`, `kip tunnel`**. With a project known, from the flags or from `kip project use`, they look only there, and a project holding no such workload is an error rather than a reason to search elsewhere. Without one they search every namespace. Either way a name matching more than one workload lists the matches and stops. `--kind app|function|service` separates an app from a service of the same name inside one namespace, which naming the project cannot.
  - *Neither, because the project comes from somewhere else* — `kip apply` and `kip diff` read it from each manifest unless a flag overrides; `kip export` and `kip app promote` require `--project` outright; `kip backup create` with no project deliberately means every non-system namespace.
- For raw kubectl: `export KUBECONFIG=~/.kip/clusters/<name>.yaml`.

## Two kinds of credential

1. **kubeconfig — by default it carries no credential at all.** A fresh install writes `~/.kip/clusters/<name>.yaml` holding the cluster address, the CA, and a `kip auth kubectl-token` exec plugin that serves *your* short-lived OIDC token. The shared k3s admin certificate never leaves the server; it stays there as the documented break-glass credential (`ssh`, then `sudo k3s kubectl`). So every action names the person who took it, and removing someone's access means removing their account rather than rotating a certificate. `kip auth kubeconfig` converts an older cluster's stored kubeconfig to this shape.

   The shared certificate reaches a machine only if asked for: `kip install --admin-kubeconfig` is the CI escape hatch, and its own help calls the certificate unattributed and revocable only by rotation. `kip cluster export > team.kip` / `kip cluster add team.kip` moves a cluster to a second machine, but **`add` refuses any kubeconfig carrying an embedded credential** — a client certificate, key or static token — and says to import a credential-free export and run `kip auth login` instead. So the pair round-trips a default install and not an `--admin-kubeconfig` one: exporting there writes the shared certificate to a file that nothing will import, which hands the credential around for nothing.
2. **Dex session — for the console and a few CLI commands.** `kip auth login` opens a browser, authenticates against Dex, and stores an ID token plus a refresh token in `~/.kip/auth.json`, refreshing automatically so you stay logged in without re-authenticating. **The two have very different lifetimes, and the short one is the credential.** Kipper pins Dex's `idTokens` to **15 minutes**, because the Kubernetes API accepts that token directly as a bearer credential; the login *session* behind it lasts as long as the rotating refresh token, which dies after **7 days unused** or **30 days** outright. So a captured ID token is worth minutes, not a day. The web console and invited team members use Dex, gated by a cluster-wide role (**admin**, **deployer**, **viewer**). A project carries its own membership on top of that, with its own roles (**owner**, **deployer**, **viewer**) — see `kip project members`. On the CLI, these commands go through the console-api and need the session: **`kip app rebuild`**, **`kip service bind`/`unbind`**, **`kip function bind`/`unbind`**, **`kip service share`**, **`kip project links`/`allow-links`**, and **`kip auth sessions revoke-all`**. **`kip auth kubectl-token`** needs it too, and is the one nobody types — kubectl invokes it, so after `kip auth kubeconfig` an expired session surfaces as `not authenticated — run: kip auth login` from an ordinary `kubectl get pods`. If one prints `session expired — run: kip auth login`, that browser login is required (an agent must hand it to the user).

Practical rule: **on a default install the Dex session underpins nearly everything**, because the kubeconfig has no credential of its own. The everyday app/service/function/project/backup/platform commands still talk straight to the Kubernetes API, but the token they present comes from your login, so `kip auth login` is a prerequisite for them rather than an extra for the console. The console-api commands listed above need it for a second reason — they call the API server's HTTP surface rather than Kubernetes. Install and host operations (`install`, `cluster harden/uninstall`, `node add`) do their substantive work over SSH and do not. **`kip upgrade` is the exception that looks like one of them and is not:** it updates CRDs and console RBAC through the Kubernetes API before it reaches the SSH half, so on a default install it needs the session like anything else. Purely local commands (`cluster list/use/add/export`, `project use`, `auth logout`, `completion`) make no cluster call. On a cluster installed with `--admin-kubeconfig`, the certificate authenticates the Kubernetes-API commands on its own and only the console-api ones need a login.

## Installing Kipper

Get the CLI. The one-line installer is Linux/macOS only: it accepts `Linux*` and `Darwin*` from `uname -s` and exits on anything else, so it runs under WSL and refuses Git Bash and MSYS. **Windows has a native binary** — download `kip-windows-amd64.exe` from the release, rename it `kip.exe` and put it on `PATH` (amd64 only; there is no Windows arm64 build). `kip install` runs `ssh` from `PATH` rather than speaking the protocol itself, so that one command needs a compatible OpenSSH client — one that takes the options it passes, `ControlMaster` among them. It needs no `scp`: a file upload streams over the same session into `cat > <path>`. WSL or Git Bash is the usual way to have one on Windows, but it is the executable that is required, not either shell.

```bash
curl -sL https://getkipper.com/install | sh
```

It installs `kip` to `/usr/local/bin/kip`. Pin a version with `KIP_VERSION=v0.1.0`. Binaries also come from GitHub Releases (`kip-<os>-<arch>`); Homebrew is coming.

Install a cluster on a fresh server over SSH:

```bash
kip install --host 203.0.113.10 --ssh-key ~/.ssh/id_ed25519 --admin-email you@example.com
```

**Pass `--host` as an IP address.** The free-subdomain path registers the value verbatim with the kipper.run gateway, which takes addresses only, so a hostname fails after preflight with `registering subdomain: gateway: ip must be a public address`. The message names neither the flag nor the hostname, so it reads as a server problem when it is an argument one. A hostname is fine only alongside `--domain <your-own-domain>`, which registers nothing.

Prerequisites: Ubuntu 20.04/22.04/24.04/26.04 or Debian 11/12, root SSH access, ports 80/443/6443 open, an SSH key. Floor is 2 vCPU / 2 GB / 30 GB; 4 vCPU / 8 GB / 80 GB is a realistic minimum. Useful flags: `--domain`, `--org` / `--org-display-name` (namespace prefix), `--console-domain`, `--harden` (host hardening, default on), `--firewall` (UFW, default on), `--backup-storage-bucket/-endpoint/-region/-credentials` (external S3 for Velero). Full reference: https://getkipper.com/en/installation. Walkthrough: https://getkipper.com/en/getting-started.

`--domain` takes either kind of name (0.11.0 and later). A name ending in `.kipper.run` is a free one the gateway registers for you, so `--domain lab.kipper.run` serves the cluster on `lab.kipper.run` instead of the address-derived `203-0-113-10.kipper.run` default, and keeps the server's IP out of every URL. It must be a single label, and names that read as the platform's own (`console`, `login`, `status`, `docs` and similar) are refused, as is a name spelling an address other than the server's own. Anything not ending in `.kipper.run` is a domain whose DNS you run yourself. Apps hang off the cluster label with a double dash either way, so `lab` gives `todo-app--lab.kipper.run`.

## Command reference

Global flag on every command: `--cluster <name>`. Most workflow commands accept `--project` and `--environment`.

**Cluster and context**
- `kip status` — cluster status overview.
- `kip cluster list | use <name> | rename | remove | add <file.kip> | export` — manage local cluster connections.
- `kip cluster domain <domain>` — set a custom console domain (`--repair` rebuilds local config from live Ingresses).
- `kip cluster env <component> KEY=VALUE` — set env on `console`, `console-api`, `dex`, `traefik`. (Restarting a component is `kip platform restart <component>`; there is no `kip cluster restart`.)
- `kip cluster auth sync` — reconcile how the API server authenticates operators. `kip cluster ca status` — the CA that signs what the cluster serves to the gateway and that the API server trusts for logins; it lasts 30 years, so replacing one is a response to a leak rather than a schedule. `kip cluster dns repair` — the cluster's upstream resolvers.
- `kip cluster harden` — apply host hardening/firewall to an existing cluster (SSH). `kip cluster uninstall` — wipe Kipper from the server.
- `kip node list | add --host <ip> --ssh-key <path>` — manage worker nodes (add is over SSH).
- `kip upgrade` — update CRDs, console layer, and system components. `--skip-system` upgrades only the Kipper layer; `--yes` skips the confirm.
- `kip cert email <address>` — set the Let's Encrypt ACME email (triggers renewal).

**Auth and users**
- `kip auth login | logout | status` — browser session against Dex. `kip auth reset-password` — regenerate the admin password.
- `kip auth kubeconfig` — replace the stored kubeconfig with one carrying **no credential at all**: kubectl fetches short-lived tokens through `kip auth kubectl-token` from your own session, so every action is attributed to you personally and access ends with your account. The shared admin certificate stays on the server as the documented break-glass credential; this removes it from the machine. `kip auth kubectl-token` is the credential plugin the kubeconfig references (the analogue of `aws eks get-token`) — kubectl runs it, not you.
- `kip auth verify` — confirm the API server accepts your login token as your own identity and grants access. The installer runs it inline; run it yourself after a headless install (`kip auth login && kip auth verify`). Exits non-zero when rejected, and never changes your kubeconfig.
- `kip auth sessions revoke-all` — drop the service-UI browser sessions.
- `kip user add <email> --role <admin|deployer|viewer> | list | role <email> <role> | remove <email>` — manage users in Dex.
- `kip user invite --email <address> --role <role> [--expires 24h|48h|7d]` — one-time invite URL for one person; `--email` is required and only that address can accept it. **`--role` is cluster-wide.** To scope someone to a single project, invite them as `--role viewer` and then `kip project members add <project> <email> <role>` — `--role deployer` on the invite would let them deploy to every project, and the project role would not take that back. **The two commands are not back to back:** accepting the invite is what creates the account, and `members add` is refused until it exists, so the second command waits until they have opened the link and set a password. For someone who already authenticates through an upstream connector, there is nothing to invite: `kip user role <email> <role>` writes the account record they are missing, and membership works from there. `kip user import <file>` — merge Dex users/connectors from a snapshot.

**Projects and environments**
- `kip project create <name> [--display-name ..] [--environments test,acc,prod]`
- `kip project list | use <name>[/<env>] | add-env <project> <env> | remove-env <project> <env> | delete <name>`
- `kip project members list <project>` · `kip project members add <project> <email> <owner|deployer|viewer>` (also changes an existing member's role) · `kip project members remove <project> <email> [--force]` — who can reach a project. **Project roles are a different axis from the cluster roles in `kip user`:** owner manages the project's members, deployer deploys, viewer is read-only. A cluster admin reaches every project as owner regardless.
- `kip project allow-links <from-project> --project <this-project> [--remove]` · `kip project links --project <p>` — consent to being linked to from another project. The decision belongs to the target project because a link goes past the ingress, so an API key, forward auth or rate limit on a public route is not in the way of it.

**Apps**
- `kip app deploy --name <app> [--image <ref> | --git <url> --branch <b> --git-token <t>] --port <n> [--project ..] [--environment ..] [--replicas] [--cpu] [--memory] [--env KEY=VAL] [--secret KEY[=VAL]] [--route <group>/<path>] [--redirect-from host,host] [--rate-limit] [--no-security-headers]` — `--name`, `--port` and one of `--image`/`--git` are always required, so this is the create path; to change one field on a live app use `kip app update`.
- `kip app update <app> [--image <ref>] [--profile <name>] [--redirect-from host,host]` — the edit path for a live app: it takes the app name as an argument, where `kip app deploy` makes you re-state the create flags (`--name`, `--port`, an image or git source) even to change one thing. Both merge what you pass into the live spec, leaving untouched keys alone — but a flag can reach fields you did not name, wherever one setting owns another. Known cases, and treat the list as the ones that have been checked rather than as all of them — several are specific to one of the two commands, so read which: **`kip app deploy --image` on a git app deletes `spec.git`** (so the old repo's webhook or a rebuild cannot overwrite the new image), while **`kip app update --image` writes `spec.image` alone and leaves `spec.git` in place**, which means the live-edit path does *not* give you that protection and the git source stays eligible to replace the image you just set. On both paths: pointing `--git` at a *different* URL replaces the whole git block rather than merging (so the new repo does not inherit the old branch or credentials); `--profile` replaces `spec.resources` wholesale (explicit cpu/memory would otherwise override the profile); and a single `--memory` or `--cpu` writes **both** the request and the limit, matching them for Guaranteed QoS, and stamps `resources.profile` as `custom`, which supersedes whatever profile was there. A burstable config with request below limit comes from the console or `kip apply`, not from these flags. · `kip app rebuild <app> [--commit <sha>]` (rebuild from Git; needs a Dex session) · `kip app list`
- `kip app logs <app> [--tail N]` · `kip app build-logs <app>` (Git build output) · `kip app history <app>` · `kip app rollback <app> [--revision N]`
- `kip app restart <app>` · `kip app scale <app> --replicas N` · `kip app autoscale <app> [--min --max --cpu --memory | --off | --status]`
- `kip app promote <app> --from <env> --to <env> --project <p>` (or `--all` instead of `<app>` for every app in the env) — copy an image tag onward.
- `kip app delete <app>` · `kip app link <target-app> <app> [--public]` / `kip app unlink` — inject the target's internal URL into the calling app as `<TARGET>_URL` (`--public` uses the public route instead). The target may live in another project, written `project/app`: `kip app link docuseal/docuseal hrportal-backend`. A cross-project link also opens the egress, because workloads are otherwise confined to their own project and there is no path between two projects without one. The other project must have consented first with `kip project allow-links`.
- `kip app links <app>` — list what an app declares it reaches and check each hop: the other project's consent, the target existing and serving a port, the allowance being in place, the address having reached the running pods, then the connection itself from inside the calling pod. That is the only place the allowance applies, so it is the only place worth testing from.
- `kip app env set|list|delete <app> [KEY=VAL]` · `kip app secret set|list|reveal|delete|rollback <app> [KEY[=VAL]]` — secrets for anything sensitive (see Golden rules). An env value may reference another variable by name, which is how a framework wanting one connection string gets it without the password being stored in plain text: `kip app env set docuseal 'DATABASE_URL=postgres://${DB_USERNAME}:${DB_PASSWORD:urlencode}@${DB_HOST}:${DB_PORT}/${DB_NAME}'`. The resource keeps the reference and only the rendered Secret and the pod hold the credential, so `kip export` and the console show `${DB_PASSWORD}`.
- `kip app webhook enable|disable|status <app>` — CI/CD deploy webhook.

**Services** (types: postgres, mysql, mongodb, redis, rabbitmq, minio, opensearch, mailhog)
- `kip service add <type> --name <name> [--storage --cpu --memory --version --project --environment]`
- `kip service list | info <name> | update <name> [--storage --memory --cpu --version] | delete <name> [--delete-data]`
- `kip service share <service> [--expires 72h] [--label "PO review"] [--list | --revoke <id> | --revoke-all | --rotate-key]` — a signed, expiring link that opens a service's web UI (a MailHog inbox, say) with no Kipper login, for someone who needs to see the UI but not the console. Max 720h. Minting, listing and revoking go through the console API, so a link is a revocable grant rather than a bare signed token; `--rotate-key` retires a leaked key over two rotations.
- `kip service export <service> --file <out> [--database DB]` / `kip service import <service> --file <dump> [--database DB]` — run the engine's own dump and restore tools inside the pod. mongodb produces a gzipped archive, postgres a custom-format dump (pg_restore input), mysql a gzipped SQL script; import takes those back, optionally gzipped. The resource tuner is paused during an import so a resize cannot restart the database mid-restore.
- `kip service credentials [--repair]` — report whether each service's credentials Secret is still owned by that service. Ownership is what admits those credentials into a bound workload, so a Secret that lost its owner leaves the service running while everything bound to it is refused. Ask before a controller rollout and after restoring a backup.
- `kip service bind <service> <app> [--prefix DB_] [--database DB]` / `kip service unbind <service> <app>` — inject connection env. The prefix is prepended verbatim (include the trailing underscore) and auto-detects from the service type (`DB_`, `REDIS_`, …) when omitted; databases get a per-app database automatically. redis, opensearch and mailhog inject `HOST` and `PORT` only: they start with authentication off, so there is no password to inject and a connection string carrying one fails (redis answers AUTH with an error when no password is set).

**Functions** (triggers: http, cron, postgres, mysql, redis, minio; runtimes: node, python)
- `kip function create <name> [--trigger http|cron|postgres|mysql|redis|minio] [--image <ref> | --code-file <path> --runtime node|python --dependency name@ver] [--port] [--schedule "<cron>" (with --trigger cron)] [--source --query --list --mark-done --bucket --volume name:/path]` (`--list` names the Redis list; `--mark-done` is the SQL to mark rows processed)
- `kip function list | logs <name> | delete <name...>`
- `kip function bind <function> <service> [--prefix --database]` / `kip function unbind`
- `kip function env set|list|delete` · `kip function secret set|list|delete`

**Jobs**
- `kip job run --name <n> --image <ref> --command "<cmd>"` — one-off.
- `kip job schedule --name <n> --image <ref> --command "<cmd>" --cron "<expr>"` — scheduled.
- `kip job list | history <name> | delete <name>`

**Shared volumes** (Longhorn RWX)
- `kip volume create <name> --size 5Gi | list | mount <volume> <app> --path /data | unmount <volume> <app> | delete <volume> [--delete-data]` — `mount` and `unmount` act on an app, so they resolve it by name across the cluster rather than from the active project (see Orientation); the other three use the saved context.

**Blueprints and GitOps**
- `kip blueprint list | info <name> | install <name> [--project --environment --set key=value]` — one-command app stacks.
- `kip init [--blueprint <name>] [-o kipper.yaml] [--set key=value]` — generate a `kipper.yaml`.
- `kip apply -f kipper.yaml [--dry-run --force --project --environment]` — apply a manifest declaratively: the manifest is the desired spec, so a field left out of it is cleared. That makes it the way to remove a spec field **that has no emptying form of its own** — where a command offers one (`--redirect-from` with no value, `autoscale --off`, `env delete`), prefer it, because it touches that field alone. **It refuses and writes nothing when it finds a field it would take away**, listing each one; `--force` is how you say you meant it. Two kinds count: a field it removes outright, and one the CRD defaults, where the cluster writes the default back over the value you set (omitting `replicas` scales a live app of four down to one). A live value that already equals the default loses nothing and is not mentioned. The scan covers every manifest before any of them is written, so a refusal leaves the cluster untouched even when applying a directory. Exception: a git app's built image is build output, not manifest state, so apply of a git-only spec preserves the running image (see How state works).
- `kip diff -f kipper.yaml` — show manifest-vs-cluster drift, field by field: `+` a field only the manifest has, `~` a field both have with different values, `-` a field only the cluster has, which applying removes or resets to the CRD default. A resource whose fields all match is not listed. `kip export --project <p> [--environment <e> | --split] [-o <path>]` — export live state to a manifest.
- `kip discover` — find Kipper-labelled workloads with no owning resource, and print the `kip` commands to adopt them.

**Credentials and registries**
- `kip credentials list [--type git|registry] | get <name> [--type ..] [--app <app> --project --environment]` — read git/registry credentials (reads Secrets in `kipper-system`; cluster-admin).
- `kip registry add --server <host> --username <u> --password <p> [--name] | list | remove <name>` — pull secrets for private registries.

**Backups** (Velero)
- `kip backup create [name] [--project --environment --ttl 168h --include-system] | list | restore <name> [--namespace --namespace-mapping src:target --resources] | schedules`

**Platform and observability**
- `kip platform status` · `kip platform profile show|set <nano|small|medium|large|xlarge>` · `kip platform resize <component> --memory <M>`
- `kip platform enable|disable <prometheus|loki>` · `kip platform restart <component>`
- `kip platform tuning show|auto|expert` — the resource controller watches every Kipper-managed workload and adjusts CPU and memory itself in **auto**; in **expert** it only reports problems and leaves every resource value alone.

**Debug and access**
- `kip exec <name> [--project <p>] [--environment <e>] [--kind <kind>] [-- <command>]` — shell/run in an app, function, or service pod. **Every flag goes before `--`**: `--` ends cobra's parsing, so anything after it is the command run inside the container. Resolves the workload from `--project`/`--environment` or the saved context, and refuses a name matching more than one workload rather than choosing. Enters a pod that is running but not ready, which is usually the one worth a shell.
- `kip tunnel <name> [--project <p>] [--environment <e>] [--kind <kind>] [--port <remote>] [--local-port <local>]` — local port-forward. Same resolution as `kip exec`, but it wants a Ready pod and says so when none is, since forwarding to a pod that cannot serve reads as a broken app.

**AI assistant and bundle**
- `kip ai configure --provider <claude|openai|ollama> [--key --model --ollama-url] | status` — wire an AI provider for log analysis and diagnosis (bring your own key).
- `kip ai install` — deploy a private LLM (Ollama) + chat UI in-cluster. Also `kip ai admin create`, `kip ai rag install`, `kip ai backup [--name]` (with `list|show|delete|repair`), `kip ai restore --name <backup>`, `kip ai uninstall`.

## Common workflows

**Deploy a prebuilt image**
```bash
kip app deploy --name web --image ghcr.io/acme/web:v1 --port 8080 --project blog --environment prod
```

**Deploy from a Git repo** (built in-cluster; `--port` is required and nothing infers it — read the value off the repo's Dockerfile `EXPOSE` and pass it)
```bash
kip app deploy --name api --git https://github.com/acme/api.git --branch main \
  --git-token "$TOKEN" --port 3000 --project blog --environment prod
kip app build-logs api --project blog --environment prod   # watch the build
```
`kip app deploy` creates the workload and stores the credential but does not build. Trigger the first build with `kip app rebuild api --project blog --environment prod` (rebuild is one of the commands that ignore the active project — see Orientation), or wire a webhook with `kip app webhook enable`.

**Add a database and bind it to an app** (injects `DB_*` connection env)
```bash
kip service add postgres --name db --project blog --environment prod
kip service bind db web --project blog --environment prod   # prefix auto-detects to DB_
```

**Promote through environments**
```bash
kip app promote web --from test --to acc --project blog
kip app promote web --from acc --to prod --project blog
# or promote every app in the environment at once:
kip app promote --all --from test --to acc --project blog
```

**Declarative / GitOps**
```bash
kip export --project blog --split -o ./manifests   # capture live state
kip diff -f ./manifests/prod.yaml                  # preview drift
kip apply -f ./manifests/prod.yaml                 # apply
```

## Gotchas

- **A project + environment is one namespace:** `<project>-<environment>`, with the exceptions in the mental model above — a `default` *or omitted* environment is unsuffixed, and an org prefix is not added to a project name that already carries it. Apps in `blog-test` are invisible when you're pointed at `blog-prod`.
- **`kip app deploy` is imperative; `kip apply` is declarative.** A redeploy writes the flags you pass, so bumping an image leaves replicas, route, bindings and volumes untouched — but not *everything* else, because a flag that owns another field takes it too (a deploy's `--image` clears the git source — `kip app update --image` does not — and `--cpu`/`--memory` restamp the whole resources block; see `kip app update` above). To *change* a field, pass its flag. To *clear* one, check for an emptying form on the command first — `kip app update <app> --redirect-from` with no value clears the redirects and says so, `kip app autoscale --off`, `kip app env delete`, `kip app secret delete` — because those touch one field and nothing else. Only where no such form exists does clearing mean applying a `kipper.yaml` with `kip apply`, and that replaces the whole spec, so everything else in it has to be right too.
- **`kip apply` is `kubectl replace`, not `kubectl apply`.** This one catches people who know Kubernetes. `kubectl apply` does a three-way merge against a last-applied annotation, so a field you set imperatively and never put in the manifest *survives* later applies. Kipper does `existing.Object["spec"] = newSpec` and every field absent from the manifest goes, whether or not the manifest ever mentioned it. Apply refuses rather than doing it silently, so the first thing you see is the list of fields at risk and the `--force` flag, but the semantics underneath are unchanged. Terraform is the closer intuition: the config is the whole desired state and drift is reverted. The single carve-out is a git app's built image, carried forward so an apply cannot reset a running app to the build placeholder. Practical consequence: after any imperative change to a project you apply from a manifest, either add the field to the manifest or re-export with `kip export`.
- **A git app's built image stays out of the manifest.** The desired state of a git app is `spec.git`; the built image in `spec.image` is build output the controller owns. `kip export` emits a git app as git-only and `kip apply` of a git-only spec preserves the running image. Never hand-add an `image:` line for a git app, and never resolve an *image and git are mutually exclusive* error by deleting the built image and re-applying — on older CLIs that reconciled the app down to the `busybox` placeholder and 502'd the live service.
- **Config in the console is desired state, not live state.** `spec.env` (and the whole CR) is what you *asked for*; a running pod only picks up an env change when it restarts. To confirm what's live, read the environment generation the pod names and `kubectl exec ... printenv`, not the console (see How state works).
- **An app's Git token lives in its own namespace,** as a Secret named by `app.spec.git.credentialsSecret` (usually `<app>-git-credentials`, key `token`). The console and `kip credentials list` show global credentials; a per-app token is read with `kip credentials get --app <app>` or straight from the Secret. There is no fallback chain — the build uses only that one secret.
- **Fine-grained GitHub PATs are scoped to selected repos.** A token that builds one app may 403 cloning another. Mint a repo-scoped token per repo.
- **`${NAME}` in an env value is resolved by Kipper; `$(NAME)` is not.** Single-quote it in the shell or your own shell expands it first and stores a hole. One pass, so a reference inside a referenced value stays literal; an unknown name reaches the process as written, which is what makes a typo name itself in the connection error. `${NAME:urlencode}` is the only modifier and belongs on any credential going between `://` and `@`. `$${NAME}` is the escape. `$(NAME)` is Kubernetes' own form and Kipper leaves it alone — and so does the kubelet, because an app's environment arrives through `envFrom`, where nothing is expanded, so it reaches the process as typed.
- **Only the values you set are templates.** A secret or a binding credential containing `${...}` is passed through as text. The console's Env tab previews what a value resolves to with the secret-derived parts masked, and needs the deploy permission because the resolved value is a different thing from the reference.
- **Redirect domains send another hostname to this app's own, with a `301`.** On an existing app this is a config edit, not a deployment: `kip app update shop --redirect-from www.example.com,old.example` writes `spec.route.redirectFrom` and nothing else, so no pod restarts — the reconciler rebuilds the Ingress and middlewares in place. **If the project is applied from a `kipper.yaml`, put the list in the manifest too.** `kip apply` replaces an app's whole spec, so an apply of a manifest that does not mention `redirectFrom` would remove it — not as a redirect-specific rule but because that is what apply does to every omitted field. It now names the field and refuses instead of removing it, which is the point at which to add it to the manifest. `kip export --project <p> --environment <e> -o <file>` captures the live state including the redirects, which beats transcribing them. The flag **replaces** the list rather than appending to it, so adding a second redirect means passing both hostnames; read the current list back with `kip export` or the console first if you are not sure what is there. Pass the flag with no value to clear them. At create time the same list is settable on `kip app deploy` alongside `--route`, and in `kipper.yaml` as `route.redirectFrom`. Up to 10, each needs its own A record, and `*.kipper.run` subdomains cannot be one. The CLI and `kip apply` share one validator and refuse a malformed host before writing; **the console does not pre-validate**, so a bad hostname entered there is written and then skipped by the reconciler. Distinct from `route.redirects`, which rewrites URLs *within* the hostname the app already serves.
- **A project created without `--environments` gets one called `default`, in the *unsuffixed* namespace.** `kip project create myapp` gives you environment `default` in namespace `myapp`, not `myapp-default` — `default` is the one environment name that does not suffix. Adding a second starts suffixing: `myapp` and `myapp-prod` side by side. A Project whose environment list is genuinely empty is a different thing that neither the CLI nor the console creates; the reconciler gives that one `test` in `myapp-test`.
- **`kip app promote` moves an image between environments and nothing else.** It writes `spec.image` on the target app's CR — the desired state — and reads back what the cluster stored before printing a tick, so a promotion that did not land is reported as a failure. The app must already exist in the target (`kip app deploy` it there first; promotion copies no route, resources or bindings), and an app that builds from git in the target is refused, because its image is build output the controller owns and the next build would undo the write. `kip app update <app> --image <ref>` is the same write for one environment.
- **A configuration change is saved, not applied.** `kip app env set|delete`, `kip app secret set|delete|rollback`, and `kip function env set|delete` and `kip function secret set|delete` (a function has no secret rollback), write the change and leave the running pods on the values they started with — a container reads its environment once, at start. The command says so and names `--restart`, which does both in one step. The console has always behaved this way (it raises a "restart to apply" banner), and the platform is built for it: an environment is published as its own immutable generation and pods move to it when they restart. Restarting drops the connections the workload is serving, so it is asked for rather than assumed. `kip app deploy` and `kip app update --image` still roll, because deploying is meant to.
- **A project keeps at least one owner, and the way back is to add rather than force.** `kip project members remove` refuses to remove the last owner. The ordinary repair for a mistyped owner is `kip project members add <project> <real-address> owner` and then an unforced remove: it needs no flag and ends with the project owned. `--force` removes a last owner outright and leaves the project with none — it is for retiring a phantom before a replacement has been chosen, not the standard recovery. A cluster admin is exempt from the rule, since they already reach every project as owner.
- **Images tagged `:latest` re-pull on restart.** For predictable rollouts, deploy explicit tags and roll with `kip app update --image <ref:tag>`.
- **A fresh custom-domain A record can take a full TTL to reach cert-manager.** Public resolvers cache the previous answer, and if a wildcard (`*.example.com`) already points at another server they keep synthesising that old IP for the new hostname until the cache expires. cert-manager's self-check then reports `wrong status code '404', expected '200'` or connection errors against an IP that is not the cluster. The platform is fine; issuance retries and completes on its own once the caches expire. Confirm with `dig <host> @1.1.1.1` and `@8.8.8.8` before suspecting the cluster. Note also that plain HTTP always answers `301` (Traefik redirects everything to HTTPS before routing), so a 301 on an acme-challenge URL is normal, and no evidence of a broken solver route.

## Where the docs are

Docs publish at `https://getkipper.com/en/<page>`. Point users to the right page:

- Install and first app: `getting-started`, `installation`
- Projects and promotion: `environments`, `resource-management`
- Apps: `deploying-apps`, `redirects`, `secrets`, `git-providers`, `webhooks`
- Services and storage: `services`, `storage`, `shared-storage`, `database-console`
- Functions and jobs: `functions`, `jobs`
- GitOps and blueprints: `gitops`, `blueprints`, `migration`
- Domains and TLS: `domains`
- Observability and health: `observability`, `dashboard`, `alerts`, `platform-resources`
- Access and security: `authentication`, `team-access`, `project-members`, `api-keys`, `security`
- Backups and AI: `backups`, `ai`
- Console features: `web-terminal`, `files`
- Contributing: `contributing`, `architecture`
