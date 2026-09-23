---
title: CLI Reference
description: Commands for managing Kipper clusters, workloads, users, and configuration.
---

# CLI Reference

Use this reference to look up commands and flags. For workflows, start with the guides in the sidebar. Run `kip <command> --help` for the options available in your installed version.

- [Installation reference](/en/installation) covers `kip install`, server requirements, and initial setup.
- [Upgrades & Maintenance](/en/maintenance) covers `kip upgrade` and upgrade recovery.

## kip cluster {#kip-cluster}

Manage cluster configurations on your local machine.

```bash
kip cluster export > file.kip        # export connection details for sharing
kip cluster add file.kip              # import a cluster
kip cluster add file.kip --set-current # import and switch to it
kip cluster list                      # list all clusters
kip cluster use <name>                # switch active cluster
kip cluster remove <name>             # remove local config
kip cluster uninstall <name>          # wipe the cluster off the remote host
kip cluster domain <domain>           # move to a custom domain (no-lockout cutover)
kip cluster domain <domain> --ack-sso-callbacks # confirm SSO callback URLs are updated
kip cluster domain --sync             # finish an interrupted domain change
kip cluster domain --rollback         # return to the previous domain
kip cluster domain --repair           # rewrite local config from the cluster
kip cluster hosts                     # show the hostnames kip uses for a cluster
kip cluster hosts --dex dex.example.com # correct one without contacting the cluster
kip cluster dns repair                # restore the curated DNS resolvers and restart CoreDNS
kip platform restart <component>      # restart a platform or cluster component
```

### kip cluster uninstall {#kip-cluster-uninstall}

Wipes Kipper from a remote Linux server. Runs k3s's own uninstall script over SSH, sweeps the data directories Kipper writes outside k3s (Longhorn volumes, Zot blobs, AI bundle data), and removes the cluster from your local kip config.

```bash
kip cluster uninstall storefront                  # interactive (prompts for cluster name)
kip cluster uninstall storefront --yes            # skip the confirmation prompt
kip cluster uninstall storefront --keep-local-config  # wipe host, keep local entry
kip cluster uninstall storefront --ssh-key ~/.ssh/kipper_ed25519
```

The command prompts you to type the cluster name to confirm, so you cannot tear down a cluster by reflex. Pass `--yes` only for automation.

A cluster on a free `*.kipper.run` subdomain also hands that name back to the gateway. The name is released after the host is wiped, using a credential read from the cluster beforehand: the gateway will only release a name to whoever holds its token, and that token lives on the cluster the wipe destroys. The name is free again straight away, so a rebuild of the same server can claim it back immediately. That also means links you published under it can be claimed by someone else, so move them before you uninstall if they still matter.

If the cluster cannot be reached, kip falls back to a copy of that credential it keeps in `~/.kip/config.yaml`, recorded on an earlier command. When neither is available the command says so and asks whether to wipe anyway. Answering no is the safe choice: wiping leaves the name registered with nothing able to release it, and installing on that host again cannot serve on it. The gateway stops routing such a name about a week after the cluster's last heartbeat, the registration lapses after 30 days without contact, and the name comes free 90 days after that, so waiting it out means waiting four months. `--yes` skips this question as well as the typed-name one, so scripted teardown consents to stranding a name it could not release.

If the server itself cannot be reached, kip offers to hand the name back without it, provided a credential for that name is recorded locally. Say yes only when the server is really gone: a cluster that is merely unreachable is still serving, and releasing its name takes it off the air. `--yes` never takes this offer, because a script cannot tell those two apart.

When the host is wiped but the gateway will not take the name back, the cluster stays in `~/.kip/config.yaml` and the command tells you to run it again. That entry is deliberate, because it holds the only credential that can still release the name, and it is why `kip cluster list` can show a server you have already wiped. The re-run goes straight to the gateway: it does not touch the host, so it works just as well on a server you have since destroyed.

```bash
kip cluster uninstall storefront
#   storefront was already wiped. Releasing its gateway name.
#
#   ✔  Gateway name released
#   ✔  Local config entry for storefront removed
```

This is destructive. All cluster state and persistent volume data on the host is removed. The command does **not** revert host firewall rules or OS hardening (rpcbind disabled, etc.), because those are general OS security improvements unrelated to k3s.

Use `--keep-local-config` when you plan to reinstall immediately, so the existing cluster name and kubeconfig path stay in `~/.kip/config.yaml` for the new install to refresh.

### kip platform restart {#kip-platform-restart}

Triggers a rolling restart of a platform or cluster component, for when it has stale configuration or needs to pick up changes.

```bash
kip platform restart dex           # restart identity provider
kip platform restart console       # restart web console
kip platform restart console-api   # restart console API
kip platform restart traefik       # restart ingress controller
```

### kip cluster env {#kip-cluster-env}

Sets environment variables on a cluster component and restarts it to pick up the changes.

```bash
kip cluster env console-api LOG_LEVEL=debug
kip cluster env console-api LOG_LEVEL=debug FEATURE_X=enabled
```

See [Team Access](/en/team-access) for full documentation.

## kip tunnel {#kip-tunnel}

Opens a secure tunnel from your machine to a service running in the cluster. Use this to connect desktop database clients (DBeaver, TablePlus, pgAdmin) to databases that are not exposed to the internet.

```bash
kip tunnel mydb                        # PostgreSQL on localhost:5432
kip tunnel cache                       # Redis on localhost:6379
kip tunnel mydb --local-port 15432     # custom local port
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--local-port` | No | Same as service port | Local port to listen on |
| `--port` | No | Auto-detected from the pod | Remote container port |
| `--project` | No | Saved project, else every project | Project name |
| `--environment` | No | Saved environment, but only when using the saved project | Target environment |
| `--kind` | No | Any kind | Restrict to `app`, `function`, or `service` |

The tunnel forwards to a pod that is Ready. When a workload is running but no
replica is ready, `kip tunnel` says so instead of forwarding, because a tunnel
to a pod that cannot serve produces connection errors that look like a broken
application.

See [Naming one workload](/en/cli-reference#naming-one-workload) below, and
[Team Access](/en/team-access) for full documentation.

## kip exec {#kip-exec}

Opens an interactive shell or runs a command inside a running container.

```bash
kip exec myapp                         # interactive shell
kip exec myapp -- cat /app/config.yaml # run a single command
kip exec mydb -- psql -U kipper app   # SQL session in a database pod
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--project` | No | Saved project, else every project | Project name |
| `--environment` | No | Saved environment, but only when using the saved project | Target environment |
| `--kind` | No | Any kind | Restrict to `app`, `function`, or `service` |

Unlike `kip tunnel`, `kip exec` will enter a pod that is running but not ready,
since a container failing its readiness probe is usually the one you want a
shell in.

See [Naming one workload](/en/cli-reference#naming-one-workload) below, and
[Team Access](/en/team-access) for full documentation.

## Naming one workload {#naming-one-workload}

`kip exec` and `kip tunnel` both take a workload name, and that name has to
identify exactly one workload. Where it matches more than one, they list the
matches and stop rather than pick one for you.

A project is authoritative when you give one, either with `--project` and
`--environment` or through `kip project use`. The search is confined to it, and
a project that holds no workload of that name is an error rather than a reason
to look in someone else's project. Without a project, the search covers the
whole cluster.

Two things commonly match more than once. The same app name in several
environments is the ordinary case, since each environment is its own namespace:

```bash
$ kip exec api
Error: "api" matches more than one workload:
  app/blog-prod
  app/blog-test
Name the one you mean with --project, plus --environment if the project has environments.
```

An app and a service can also share a name inside one namespace, which naming
the project does not resolve. Use `--kind` for that:

```bash
$ kip exec api --project blog --environment prod
Error: "api" matches more than one workload in blog-prod:
  app/blog-prod
  service/blog-prod
Name the one you mean with --kind app or --kind service.

$ kip exec api --project blog --environment prod --kind service
  Connecting to blog-prod/api-0...
```

## kip status {#kip-status}

Shows cluster health, node status, and component availability.

```bash
kip status
```

It also audits the DNS resolvers your cluster forwards external queries to. Those live in a file on the server, so `kip status` makes a best-effort root SSH connection (reusing the key from install, then your SSH agent or `~/.ssh/id_ed25519`) to read it. Three things are checked: the file is still a safe set (IPv4 entries only, at most three), the entries still match the resolvers the cluster was configured with (`dns_resolvers` in `~/.kip/config.yaml`, or the default public set), and each resolver actually accepts connections from the server. Each problem gets its own warning, and drift or an unsafe hand-edit is fixed with `kip cluster dns repair`. If the server can't be reached over SSH, the section says so explicitly rather than pretending the check passed, and the rest of the status still prints.

## kip cluster dns repair {#kip-cluster-dns-repair}

Restores the curated DNS resolver file on the server and restarts CoreDNS to pick it up. This is the scoped fix for the resolver drift `kip status` warns about; nothing else about the installation is touched.

```bash
kip cluster dns repair
```

```
  ✔  Restored resolvers on 203.0.113.10: 1.1.1.1, 8.8.8.8, 9.9.9.9
  ✔  CoreDNS restarted to pick them up
```

The resolvers come from `dns_resolvers` in `~/.kip/config.yaml` (set via `--dns-resolver` at install time), or the default public set when none are configured.

## kip node add {#kip-node-add}

Kipper supports single-server clusters. This unsupported command remains callable but is hidden from help.

```bash
kip node add --host <ip> [--ssh-key <path>]
```

The command joins the host as a k3s worker using the control plane's k3s version. Worker operation requires additional setup and manual maintenance:

- **Networking:** the server's default firewall lacks rules for cross-node overlay traffic, which can disrupt pod connectivity.
- **Host setup:** the command applies kubelet hardening and storage restart safeguards, but skips firewall configuration and installation of Longhorn's host dependencies (`open-iscsi` and `nfs-common`).
- **Maintenance:** `kip upgrade`, `kip cluster harden`, and `kip cluster uninstall` perform host operations on the configured server only. Worker maintenance and removal are manual.
- **Availability:** ingress remains on the control-plane node, and the registry uses one replica with single-replica storage. Adding a worker alone does not provide high availability.

## kip node list {#kip-node-list}

Lists all nodes in the cluster with role, status, version, and IP.

```bash
kip node list
```

## kip auth verify {#kip-auth-verify}

Proves your OIDC identity authenticates and authorizes against the cluster, the same check the installer runs inline. Run it after a headless install, or any time you want to confirm the login path works end to end.

```bash
kip auth verify
```

```
  ✔  Authenticated and authorized as oidc:admin@shop.kipper.run
```

It exits non-zero when the API server rejects your token or an admin identity is denied access, so it doubles as the CI signal that the authenticator is live. It never changes your kubeconfig.

## kip auth kubeconfig {#kip-auth-kubeconfig}

Converts a legacy or admin kubeconfig to authenticate through your `kip auth login` session. Fresh installations normally use this per-user configuration already.

```bash
kip auth kubeconfig
```

```
  ...  Checking that your login reaches this cluster (up to a minute)

  ✔  /Users/anna/.kip/clusters/shop.kipper.run.yaml now authenticates as your OIDC identity
     kubectl runs `kip auth kubectl-token` for short-lived tokens.
```

The check comes first because the file being replaced is often the only credential that reaches the cluster from this machine. It is the same proof [`kip auth verify`](/en/cli-reference#kip-auth-verify) makes: your session token is sent to the API server, which has to accept it as you and grant you access. A cluster that answers anything else keeps the credential it has.

```
  ✗  This cluster did not accept your login: the API server rejected the token
     /Users/anna/.kip/clusters/shop.kipper.run.yaml is unchanged, so it still reaches the cluster.
     Run 'kip cluster ca status' to see what the API server has loaded,
     and 'kip auth verify' to re-check the login once it is fixed.

this cluster does not accept your login, kubeconfig unchanged
```

A rejected token usually means the API server has no authenticator for your issuer. `kip cluster ca status` says so directly, reporting that the authentication config names no issuer.

The command also preserves the existing kubeconfig when it cannot reach the API server:

```
  ⚠  Could not reach the API server to check your login: Get "https://203.0.113.10:6443/apis/authentication.k8s.io/v1/selfsubjectreviews": dial tcp 203.0.113.10:6443: i/o timeout
     /Users/anna/.kip/clusters/shop.kipper.run.yaml is unchanged and still works.

login could not be checked, kubeconfig unchanged
```

Sign in with `kip auth login` before running it:

```
  ✗  Your login could not be checked, so /Users/anna/.kip/clusters/shop.kipper.run.yaml is unchanged.

not authenticated. Run: kip auth login
```

After this, the kubeconfig carries no credential at all: kubectl obtains a token valid for a few minutes each time it needs one, every action in the Kubernetes audit log names your email, and removing a person's access means removing their account rather than rotating certificates. The admin certificate stays on the server as the break-glass credential (see [Architecture](/en/architecture)).

The file also names the cluster it signs in against, so run this once per cluster on each machine that holds one. `kip cluster domain` keeps that name current when a cluster's domain changes.

## kip auth kubectl-token {#kip-auth-kubectl-token}

The credential plugin the rewritten kubeconfig calls. kubectl runs it automatically whenever it needs a token; there is normally no reason to run it by hand.

```bash
kip auth kubectl-token --cluster-domain shop.kipper.run
```

```json
{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"eyJhbGciOi…","expirationTimestamp":"2026-07-21T09:15:00Z"}}
```

`--cluster-domain` names the cluster whose session to serve, and kip writes it into every kubeconfig it renders, so kubectl passes it for you. It has to come from the file because kubectl tells a credential plugin nothing about which kubeconfig invoked it: each kubeconfig therefore signs in against its own cluster whatever `kip cluster use` is set to, which is what lets you work across two clusters in two terminals.

`kip cluster use` still decides which cluster the rest of kip talks to. It has no say over kubectl.

When the session has expired entirely it prints `session expired. Run: kip auth login` on stderr, which kubectl surfaces verbatim. A kubeconfig written before this flag existed prints `this kubeconfig does not say which cluster it authenticates to. Regenerate it with: kip auth kubeconfig`, and no token is issued until you do.

## kip auth reset-password {#kip-auth-reset-password}

Generates a new admin password, writes it to Dex, displays the new credentials, and restarts Dex.

```bash
kip auth reset-password
```

This command requires the kubeconfig stored in `~/.kip/clusters/`. Only someone with cluster admin access can run it.

## kip discover {#kip-discover}

Find Kipper-labelled workloads on the cluster that have no owning Kipper CR. Read-only.

```bash
kip discover
```

Kipper considers a Service or App or Volume or Function to "exist" when its CR exists. A Deployment, StatefulSet, or PVC carrying `app.kubernetes.io/managed-by=kipper` without a matching CR is drift. It will not show up in `kip service list` or in the console, even though it occupies cluster resources.

`kip discover` lists each orphan and prints a suggested kip command that recreates it as a proper CR with the workload's current settings. Run the suggested command to bring the orphan under management. The controller adopts the existing workload to match the new CR, no deletion needed. Edit the suggested command first if you want to change anything.

For Deployments, the suggestion uses `kip app deploy` with `--image`, `--port`, `--memory`, `--cpu`, `--env`, `--secret`, `--replicas`. For StatefulSets, `kip service add` plus the service type (postgres, redis, and so on), with `--storage`, `--memory`, `--cpu`. For PVCs, `kip volume create` with `--size`. Functions print a comment instead, because the source code is not on the workload and has to come from `kip function create`.

Workloads in the `kipper-system` namespace are skipped. The console, console-api, and zot legitimately have no owning CR.

## kip cert list {#kip-cert-list}

Lists every certificate on the current cluster with its host, current state,
the age of the last state change, and cert-manager's reason when it is not
ready. The list includes platform hosts and app routes.

```bash
kip cert list
```

## kip cert email {#kip-cert-email}

Shows or updates the Let's Encrypt email used for TLS certificates. This is the email cert-manager uses when registering with Let's Encrypt for automatic certificate issuance.

```bash
kip cert email                    # show current email
kip cert email admin@example.com  # update email and renew stuck certs
```

When updating, the command re-registers with Let's Encrypt using the new email and triggers renewal for any certificates that are stuck or failed. Certificates usually come through within 1-2 minutes.

See [Domains & SSL: Troubleshooting certificates](/en/domains#troubleshooting-certificates) for common certificate issues.

## kip ai {#kip-ai}

`kip ai` covers two related things: choosing which AI provider Kipper itself uses (for log analysis, Dockerfile generation, diagnostics), and installing a private LLM stack inside the cluster that your apps can call.

### kip ai configure / kip ai status {#kip-ai-configure-kip-ai-status}

Configure which AI provider Kipper uses. Supports Claude (Anthropic), OpenAI, and Ollama (self-hosted).

```bash
kip ai configure                                     # interactive setup
kip ai configure --provider claude --key sk-ant-...  # non-interactive
kip ai configure --provider ollama                   # self-hosted (no key needed)
kip ai status                                        # show current config and bundle health
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--provider` | No | — | AI provider: `claude`, `openai`, `ollama` |
| `--key` | No | — | API key (not needed for Ollama) |
| `--model` | No | — | Model override |
| `--ollama-url` | No | `http://localhost:11434` | Ollama server URL |

### kip ai admin create {#kip-ai-admin-create}

Seed the first LibreChat admin account after `kip ai install`. The bundle ships with open registration off, so an admin must be created once before anyone can log in. Runs `npm run create-user` inside the running librechat pod via the Kubernetes API. No kubectl required.

```bash
kip ai admin create --email you@example.com --name 'Your Name' --password 'a-strong-password'
kip ai admin create --email you@example.com --name 'Your Name' --username alice --password '...'
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--email` | Yes | — | Admin email address |
| `--password` | Yes | — | At least 8 characters |
| `--name` | Yes | — | Display name shown in the chat UI |
| `--username` | No | local part of `--email` | LibreChat username |

### kip ai install / kip ai uninstall {#kip-ai-install-kip-ai-uninstall}

Install or remove the in-cluster AI bundle (Ollama and LibreChat). The full walkthrough is on the [AI Bundle](./ai) page.

```bash
kip ai install                            # detect tier, pick a model, install
kip ai install --host chat.example.com    # override the chat hostname
kip ai install --model qwen2.5:7b-instruct-q4_K_M
kip ai uninstall                          # remove the bundle and wipe its data
```

`kip ai uninstall` is destructive: it deletes the workload, the PVCs (model cache, chat history, MongoDB), credentials, and the `kipper-ai` namespace. Take a blocking snapshot first with `kip ai backup --name pre-uninstall --wait` if you want to preserve any of it. Uninstall refuses by default while a Kipper AI backup is still in flight; pass `--force` to override (you'll get an unrestorable snapshot if you do).

| Flag | Command | Default | Description |
|---|---|---|---|
| `--host` | install | `chat.<cluster-domain>` | External chat UI hostname |
| `--model` | install | tier-appropriate Qwen 2.5 | Ollama model tag to preload |
| `--pvc-size` | install | 10/30/60 GiB by tier | Model cache PVC size |
| `-y, --yes` | install | `false` | Skip the auto-configure prompt |

### AI bundle health on the Platform page {#ai-bundle-health-on-the-platform-page}

The Platform page surfaces a drift check for each installed AI bundle. The check reads the `kipper-ai-bundle-state` and `kipper-rag-bundle-state` ConfigMaps in `kipper-ai` and confirms every expected workload (Ollama, LibreChat, AnythingLLM, Qdrant, plus their Ingresses) still exists.

If a bundle was installed but a workload has disappeared, the panel renders the missing resources and points at the `kip ai install` command that reconciles. This is the same diagnosis path that used to require `ssh + kubectl get`.

### kip ai backup / kip ai restore {#kip-ai-backup-kip-ai-restore}

Velero-backed snapshot of the AI bundle. Velero is a Kipper system component, so no extra setup is needed.

```bash
kip ai backup                            # auto-name, exits after 60s warmup
kip ai backup --name pre-upgrade         # named, exits after 60s warmup
kip ai backup --name pre-upgrade --wait  # block until completion (CI scripts)
kip ai backup show --name pre-upgrade    # detailed status (phase, items, errors)
kip ai backup list
kip ai backup delete --name pre-upgrade           # exits after 60s warmup
kip ai backup delete --name pre-upgrade --wait    # block until Backup CRs are gone
kip ai restore --name pre-upgrade        # requires kipper-ai uninstalled first
```

| Flag | Command | Default | Description |
|---|---|---|---|
| `--name` | backup | timestamped | Snapshot name |
| `--wait` | backup | `false` | Block until completion instead of exiting after the 60s warmup |
| `--wait` | backup delete | `false` | Block until both Backup CRs are gone instead of exiting after the 60s warmup |
| `--name` | backup show | — | Snapshot to inspect (required) |
| `--name` | backup delete | — | Snapshot to delete (required) |
| `--name` | restore | — | Snapshot to restore (required) |

## kip app update {#kip-app-update}

Updates the container image or resource profile of a deployed application and triggers a rolling update.

```bash
kip app update api --image ghcr.io/acme/api:v2.1.0
kip app update api --profile jvm
```

| Flag | Required | Description |
|---|---|---|
| `--image` | No* | New container image |
| `--profile` | No* | Resource profile: `lightweight`, `standard`, `compute-heavy`, `memory-heavy`, or `jvm` |
| `--project` | No | Project name |
| `--environment` | No | Target environment |

*At least one of `--image` or `--profile` is required. Setting a profile replaces any custom CPU/memory values with the profile's defaults.

## kip app scale {#kip-app-scale}

Sets the replica count for a deployed application.

```bash
kip app scale api --replicas 3
```

| Flag | Required | Description |
|---|---|---|
| `--replicas` | Yes | Number of replicas |

Setting replicas to 0 stops the application without deleting it.

## kip app env / kip app secret {#kip-app-env-kip-app-secret}

Manage environment variables and secrets for an application.

```bash
kip app env set api LOG_LEVEL=debug       # set env var
kip app env list api                      # list with values visible
kip app env delete api LOG_LEVEL          # remove

kip app secret set api DATABASE_URL       # interactive hidden prompt
kip app secret list api                   # keys only, values masked
kip app secret reveal api DATABASE_URL    # show a single value
kip app secret rollback api DATABASE_URL  # restore previous value
kip app secret delete api DATABASE_URL    # remove
```

Every command that changes configuration saves it without restarting the
workload; add `--restart` to apply it immediately. See [Secrets and
configuration](/en/secrets).

Secrets can also be set at deploy time with `--secret` on `kip app deploy` (repeatable; a bare `KEY` prompts with hidden input), so the app never starts without them.

See [Secrets & Environment](/en/secrets) for full documentation.

## kip app link / kip app unlink {#kip-app-link-kip-app-unlink}

Connect apps so one can reach another via a URL.

```bash
kip app link domain-service api-gateway             # internal URL (backend-to-backend)
kip app link domain-service webapp --public          # public URL (for frontend apps)
kip app unlink domain-service api-gateway            # removes DOMAIN_SERVICE_URL
```

Use `--public` when linking to a frontend app that runs in the browser. It injects the target's public HTTPS URL instead of the internal Kubernetes DNS.

See [Deploying Apps: Linking apps](/en/routing#linking-apps) for details.

## kip service {#kip-service}

Manage stateful services (databases, caches) with persistent storage.

```bash
kip service add postgres --name mydb         # deploy PostgreSQL
kip service add redis --name cache           # deploy Redis
kip service list                             # list all services
kip service info mydb                        # show connection details
kip service share mailhog --expires 72h      # shareable link to a service web UI
kip service import mydb --file dump.sql      # load a database dump
kip service export mydb --file nightly.dump  # dump a database to a local file
kip service credentials                      # check every service owns its credentials
kip service delete mydb --delete-data        # delete the service and its volume
```

`kip service share` accepts `--project` and `--environment` to target a service outside the active project, `--expires` (up to 720h) for the link lifetime, and `--label` for a note in the listing. Use `--list` to see a service's links, `--revoke <id>` to kill one, and `--revoke-all` plus `--rotate-key` to contain a leak.

See [Stateful Services](/en/services) for full documentation.

## kip project {#kip-project}

Manage projects and environments.

```bash
kip project create blog --environments test,acc,prod
kip project create blog --display-name "example.com Domain Platform" --environments test,acc,prod
kip project list
kip project add-env blog prod
kip project remove-env blog staging
kip project delete blog
```

`kip project remove-env` deletes the matching namespace and everything in it. You'll be asked to type the environment name to confirm. See [Projects & Environments](/en/environments#adding-and-removing-environments) for details.

### kip project members {#kip-project-members}

Manage who can access a project and what they can do.

```bash
kip project members list blog
kip project members add blog jordan@acme.com deployer
kip project members remove blog jordan@acme.com
```

Roles are `owner`, `deployer`, or `viewer`. See [Project Members](/en/project-members) for what each role can do.

### kip project use {#kip-project-use}

Set a persistent project context for the current cluster, so the rest of the kip commands do not need `--project` and `--environment` flags every time.

```bash
kip project use blog           # active project: blog, default environment
kip project use blog/test      # active project: blog, environment: test
kip project use blog test      # same, with a space instead of a slash
kip project use --clear              # forget the active project on this cluster
```

The active project is stored per cluster in `~/.kip/config.yaml`. After setting it, `kip service list`, `kip app list`, `kip volume list`, `kip function list`, and similar commands resolve to the active project's namespace automatically. `kip cluster list` shows the active project on each cluster line.

Explicit `--project` flags still win. Passing `--project other-name` switches that single command to the other project; the persisted environment is not carried over so a different project never inherits a stale environment.

See [Projects & Environments](/en/environments) for full documentation.

## kip app promote {#kip-app-promote}

Promote an app from one environment to the next (copies the image tag only).

```bash
kip app promote api --from test --to acc --project blog
kip app promote --all --from acc --to prod --project blog
```

See [Projects & Environments](/en/environments) for full documentation.

## kip function {#kip-function}

Manage serverless functions. Functions scale to zero when idle and spin up on demand. Alias: `kip fn`.

```bash
kip function create process-image --image myregistry/processor:v1 --port 8080 --project blog
kip function create db-sync --trigger postgres --source mydb --query "SELECT * FROM events WHERE processed = false" --project blog
kip function create cache-worker --trigger redis --source cache --list jobs --project blog
kip function list --project blog
kip function logs process-image --project blog
kip function delete process-image --project blog
```

### kip function create flags {#kip-function-create-flags}

| Flag | Required | Default | Description |
|---|---|---|---|
| `--image` | Yes | — | Container image for the function |
| `--trigger` | No | `http` | Trigger type: `http`, `postgres`, `mysql`, `redis`, `minio` |
| `--port` | No | `8080` | Port the function listens on |
| `--source` | No | — | Service name for event triggers |
| `--query` | No | — | SQL query for postgres/mysql triggers |
| `--mark-done` | No | — | SQL to mark rows as processed |
| `--list` | No | — | Redis list name for redis triggers |
| `--bucket` | No | — | MinIO bucket name for minio triggers |
| `--project` | No | `default` | Project name |
| `--environment` | No | — | Target environment |

See [Serverless Functions](/en/functions) for full documentation.

## kip job {#kip-job}

Run one-off tasks and scheduled jobs.

```bash
kip job run --name migrate --image myapp:latest --command "npm run migrate" --project blog --environment test
kip job schedule --name cleanup --image myapp:latest --command "python cleanup.py" --cron "0 3 * * *"
kip job list --project blog
kip job history cleanup
kip job delete cleanup
```

See [Jobs & Scheduled Tasks](/en/jobs) for full documentation.

## kip volume {#kip-volume}

Create shared persistent volumes that can be mounted by multiple apps. Backed by Longhorn with ReadWriteMany access.

```bash
kip volume create uploads --size 5Gi --project blog --environment test
kip volume mount uploads webapp --path /data --project blog --environment test
kip volume unmount uploads webapp --project blog --environment test
kip volume list --project blog
kip volume delete uploads --delete-data --project blog --environment test
```

### kip volume create flags {#kip-volume-create-flags}

| Flag | Required | Default | Description |
|---|---|---|---|
| `--size` | No | `5Gi` | Volume size |
| `--project` | No | `default` | Project name |
| `--environment` | No | — | Target environment |

### kip volume mount flags {#kip-volume-mount-flags}

| Flag | Required | Default | Description |
|---|---|---|---|
| `--path` | No | `/data` | Mount path inside the container |
| `--project` | No | `default` | Project name |
| `--environment` | No | — | Target environment |

Mounts are recorded on the volume and applied to the app automatically, so they survive image updates and redeployments. `kip volume unmount` removes the mount again; the volume and its data stay available for other apps. Deleting a volume needs `--delete-data` because it permanently destroys the data.

See [Storage](/en/storage) for full documentation.

## kip user {#kip-user}

Manage cluster users and roles. Kipper supports three roles: `admin` (full access), `deployer` (deploy, scale, manage apps and services), and `viewer` (read-only).

```bash
kip user list
kip user add dev@example.com --role deployer
kip user add pm@example.com --role viewer --password secret123
kip user invite --email dev@example.com --role deployer      # invite a developer
kip user invite --email ops@example.com --role admin --expires 24h
kip user role dev@example.com admin                # change role
kip user remove dev@example.com
kip user import dex-snapshot.yaml                  # bulk-import Dex users from a snapshot
kip user import dex-snapshot.yaml --restart-dex    # also roll Dex so the new config takes effect
```

### kip user import {#kip-user-import}

Merges the `staticPasswords` and `connectors` blocks from a captured `dex-config` snapshot into the live `dex/dex-config` ConfigMap. The snapshot can be either a full ConfigMap manifest (`kubectl get cm dex-config -n dex -o yaml`) or the raw Dex config YAML directly.

Existing entries on the live side always win on conflicts, so the install admin cannot get overwritten with stale snapshot data. Use this after a Velero restore that brought a pre-rename `dex-config` across. Without it, production users live in the snapshot but the new install only knows about the bootstrap admin.

Pass `--restart-dex` to roll the Dex Deployment automatically. Without it, run `kubectl -n dex rollout restart deploy/dex` once the import finishes.

### kip user add flags {#kip-user-add-flags}

| Flag | Required | Default | Description |
|---|---|---|---|
| `--role` | No | `deployer` | Role: `admin`, `deployer`, or `viewer` |
| `--password` | No | prompted | Password (prompted interactively if not provided) |

### kip user invite flags {#kip-user-invite-flags}

| Flag | Required | Default | Description |
|---|---|---|---|
| `--email` | Yes | — | Address of the person being invited. The account is created under it, and only that address can accept the invite |
| `--role` | No | `deployer` | Role: `admin`, `deployer`, or `viewer` |
| `--expires` | No | `48h` | Expiry: `24h`, `48h`, `7d` |

Invites are tied to the supplied email address. The CLI prints a link you can share even when email delivery is unconfigured.

Create the account with an appropriate cluster role, then assign access to individual projects. For a project deployer:

```bash
kip user invite --email jordan@example.com --role viewer
# After Jordan accepts the invitation:
kip project members add acme-shop jordan@example.com deployer
```

Project access depends on membership; cluster admins have access to every project.

See [Team Access](/en/team-access) for full documentation.

## kip 2fa {#kip-2fa}

Manage two-factor authentication for console users. Destructive operations, meaning starting a cluster migration and applying its cutover, require a TOTP code on top of the admin login, and enrolling a factor requires a one-time code that only these host-level commands can issue.

```bash
kip 2fa bootstrap admin@example.com    # issue a one-time enrollment code
kip 2fa remove admin@example.com       # remove a factor (lost phone, no recovery codes)
```

### kip 2fa bootstrap {#kip-2fa-bootstrap}

Issues a single-use enrollment code, valid for 15 minutes:

```bash
kip 2fa bootstrap admin@example.com

  ✔  Enrollment code for admin@example.com

     K7QT-M3XP-9WLC-R2VD

  Valid for 15 minutes, single-use.
  Enter it in Console → Settings → Two-factor authentication to enroll.
```

The user enters the code in **Console → Settings → Two-factor authentication**, scans the QR code with an authenticator app, and confirms. Issuing a new code for the same email replaces any unused one. Enrollment is gated on this code because it can only be issued with kubeconfig access; a stolen console login alone can never enroll a device.

### kip 2fa remove {#kip-2fa-remove}

Deletes a user's enrolled factor, leaving the account unenrolled. This is the recovery path when the phone is gone and no recovery codes are left. Re-enrollment needs a fresh bootstrap code, and the new factor waits the full eligibility period (7 days by default) before it can authorise a migration.

See [Cluster Migration](/en/migration#two-factor-authentication) for the full 2FA and migration security model.

## kip registry {#kip-registry}

Manage container registry credentials. A credential is stored once in `kipper-system` and carries an allow-list of projects. When a workload in an allowed project runs an image from that registry, Kipper stages a pull secret in the workload's namespace, scoped to that single registry. Build containers run user code, so they carry no registry credentials and pull base images anonymously.

```bash
kip registry add --server ghcr.io --username myuser --password ghp_token123 --allow-project acme
kip registry add --server registry.git.example.com --username deploy --password secret --allow-project acme
kip registry allow ghcr-io --project shop      # adds shop, keeps acme
kip registry revoke ghcr-io --project shop     # removes shop, keeps the rest
kip registry list
kip registry remove ghcr-io
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--server` | Yes | — | Registry server (e.g. `ghcr.io`, `registry.git.example.com`) |
| `--username` | For a new registry | — | Registry username or token name |
| `--password` | For a new registry | — | Registry password or access token |
| `--name` | No | auto-generated | Credential name (derived from the server's host if omitted) |
| `--allow-project` | No | none | Project allowed to pull with this credential (repeatable; replaces the allow-list) |

A credential is used only by projects on its allow-list, so grant at least one. Re-running `kip registry add` for an existing name updates just the flags you pass, so granting a project keeps the stored password.

`--allow-project` **replaces** the allow-list. Naming one project takes every other away, which is what it has always done; it now prints what it removed so you can see it happen. To add a project without disturbing the others, use `kip registry allow`, and to take one away use `kip registry revoke`:

```bash
kip registry allow ghcr-io --project shop --project blog
kip registry revoke ghcr-io --project blog
```

`allow` checks that the project exists and refuses a name the cluster does not have, since a project name is matched exactly at pull time and a typo would be stored as a grant that can never work. `revoke` takes any name, because it is also how you remove one that should never have been there.

Pointing an existing credential at a different registry needs `--password`. The credential is addressed by its name, so changing the server would otherwise hand the new registry the password stored for the old one.

The console's registry settings edit the password and the server. Who may pull with a credential is changed with the commands above, and the settings API refuses a request that would change the allow-list on a credential that already exists.

When a workload in a granted project runs an image from that registry, the credential is staged into the workload's namespace as a pull secret, where the project's members can read it. Grant a credential only to projects that may share that registry login. When projects must stay isolated, create a separate registry account per project and add each as its own credential; a project's workloads then use the credential granted to them.

## kip credentials {#kip-credentials}

Read back the git and container-registry credentials stored in the cluster. Useful if you no longer have a copy of a token and need to reuse it somewhere else, such as another cluster or a CI pipeline.

```bash
kip credentials list                          # masked overview, both types, with allowed projects
kip credentials list --type git               # masked overview, git only
kip credentials get git-acme-tools            # plaintext token to stdout
kip credentials get ghcr-io --type registry   # disambiguate if names collide
kip credentials get --app blog --project acme --environment prod   # an app's own git token
kip credentials allow git-acme-tools --project acme    # let a project build with it
kip credentials revoke git-acme-tools --project acme   # stop it building with it
```

### Granting a project {#granting-a-project}

A shared git credential is usable only by the projects on its allow-list, so a new one builds nothing until you allow a project. Granting never asks for the token: it changes who may use the credential, not what it is.

```bash
kip credentials allow git-acme-tools --project acme
kip credentials allow git-acme-tools --project acme --project blog
kip credentials revoke git-acme-tools --project blog
```

A build refused for want of a grant says so and names the command:

```
git credential "git-acme-tools" is not allowed for project "acme". Allow it with 'kip credentials allow git-acme-tools --project acme'
```

Revoking leaves running apps alone. They keep the image they have, and the next build for that project is refused.

Container registry credentials have their own allow-list, granted with `kip registry add --allow-project` as described above. That flag replaces the list rather than adding to it, so name every project that should keep access.

The console's credential settings edit the token and the server. Who may build with an existing credential is changed with the commands above; `allow` checks that the project exists, and `revoke` takes any name, since it is also how you remove one that should never have been there. The settings API accepts a credential's allow-list when creating it, and refuses a request that would change the list on one that is already there.

`kip credentials list` shows each credential's allowed projects, so you can check a grant landed where you meant it.

On clusters that predate credential allow-lists, `kip upgrade` previews the projects whose apps reference each undecided shared credential. Accept the prompt to grant those pairs, or decline and grant access later with `kip credentials allow`. The migration runs once per cluster.

For automation, `--seed-credential-grants` accepts the previewed grants. A run with no terminal grants nothing unless that flag is set.

Rotate or replace shared credentials before or after an upgrade. If a credential changes during the upgrade, Kipper reports it and leaves its grants for you to review. After a rollback to a version older than 0.14, check `kip credentials list` and restore missing grants explicitly.

A token configured per app (under the app's Git settings) lives in a secret in the app's namespace, separate from the named credentials above. It does not show up in `kip credentials list` or the global credentials screen. To read it back, pass `--app` with the project and environment instead of a credential name. Kipper finds the secret the app references for its git source and prints that token.

The console masks credential values by default. To reveal one in the browser, click the eye icon next to a credential in Settings and re-enter your password. On the CLI, `kip credentials get` prints plaintext to stdout without prompting, so make sure nothing is watching your terminal.

CLI credential reads require Kubernetes permission to read the relevant Secret. Named credentials live in `kipper-system`; an app’s own token lives in its project namespace.

| Flag | Required | Default | Description |
|---|---|---|---|
| `--type` | No | — | Restrict or disambiguate: `git` or `registry` |

## kip backup {#kip-backup}

Create, list, and restore cluster backups using Velero.

```bash
kip backup create                                                   # backup user namespaces (system namespaces excluded by default)
kip backup create weekly --project blog                       # backup one project
kip backup create --project blog --environment prod --ttl 720h  # 30-day retention
kip backup create everything --include-system                       # also back up system namespaces
kip backup list
kip backup restore weekly-20260413
kip backup restore weekly-20260413 --namespace blog-prod      # restore one namespace
kip backup schedules                                                # list scheduled backups
```

A default `kip backup create` excludes system namespaces (`kube-system`, `kube-public`, `kube-node-lease`, `traefik`, `longhorn-system`, `keda`, `monitoring`, `velero`). The exclusion is the same one the daily/weekly schedules use. Without it, Velero would try to back up its own MinIO PVC and the backup would hang. Pass `--include-system` if you really want a backup of those namespaces too.

Every backup also skips cert-manager's transient issuance objects (CertificateRequests, Orders, Challenges), including when `--include-system` is set. cert-manager recreates them on demand, and restoring them stops certificates renewing.

### kip backup create flags {#kip-backup-create-flags}

| Flag | Required | Default | Description |
|---|---|---|---|
| `--project` | No | — | Backup a specific project only |
| `--environment` | No | — | Backup a specific environment only |
| `--ttl` | No | `168h` (7 days) | Backup retention period |
| `--include-system` | No | `false` | Include system namespaces (kube-*, traefik, longhorn-system, monitoring, keda, velero). Off by default because Velero recurses into its own MinIO PVC otherwise. |

### kip backup restore flags {#kip-backup-restore-flags}

| Flag | Required | Default | Description |
|---|---|---|---|
| `--namespace` | No | — | Restore only a specific namespace |
| `--namespace-mapping` | No | — | Restore to a different namespace (format: `source:target`) |
| `--resources` | No | — | Restore only specific resource types (comma-separated) |

## kip platform {#kip-platform}

Manage system component sizing, including the observability stack (Prometheus, Grafana, Loki). See [Platform Resources](/en/platform-resources) for the full reference.

```bash
kip platform status                          # active profile + per-component state
kip platform resize prometheus --memory 2Gi  # set a manual memory override
kip platform disable loki                    # turn a component off
kip platform enable loki                     # turn it back on
kip platform restart prometheus              # rolling restart
kip platform profile show                    # current profile
kip platform profile set large               # change profile
kip platform tuning show                     # active resource tuning mode
kip platform tuning expert                   # stop automatic resource changes
kip platform tuning auto                     # resume automatic tuning
```

## kip blueprint {#kip-blueprint}

Browse and install application blueprints. Blueprints are pre-built templates for common application stacks.

```bash
kip blueprint list
kip blueprint info wordpress
kip blueprint install wordpress --project myblog --set domain=blog.example.com
```

### kip blueprint install flags {#kip-blueprint-install-flags}

| Flag | Required | Default | Description |
|---|---|---|---|
| `--project` | No | — | Project name (overrides template) |
| `--environment` | No | — | Target environment |
| `--set` | No | — | Parameter values (`key=value`, repeatable) |

## kip apply {#kip-apply}

Apply a `kipper.yaml` manifest to the cluster. This is the declarative way to manage Kipper resources: the manifest is the desired spec, and on update a field left out of it is cleared.

```bash
kip apply                                         # apply ./kipper.yaml
kip apply -f myapp.yaml                           # apply a specific file
kip apply -f kipper/                              # apply all manifests in a directory
kip apply --dry-run                               # preview changes without applying
kip apply --project blog --environment prod  # override project/environment
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `-f`, `--file` | No | `kipper.yaml` | Path to manifest file or directory |
| `--dry-run` | No | `false` | Print what would be applied without making changes |
| `--project` | No | from manifest | Override the project name |
| `--environment` | No | from manifest | Override the environment |

## kip diff {#kip-diff}

Show differences between a `kipper.yaml` manifest and the live cluster state. Useful for reviewing changes before applying.

```bash
kip diff                               # diff ./kipper.yaml against live state
kip diff -f myapp.yaml                 # diff a specific file
kip diff --project blog          # override project
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `-f`, `--file` | No | `kipper.yaml` | Path to manifest file or directory |
| `--project` | No | from manifest | Override the project name |
| `--environment` | No | from manifest | Override the environment |

## kip export {#kip-export}

Export the current cluster state as a `kipper.yaml` manifest. Useful for backing up configuration or migrating between clusters.

```bash
kip export --project blog                                # export to stdout
kip export --project blog -o kipper.yaml                 # export to file
kip export --project blog --split -o blog-exports  # export every env, one file per env, into a directory
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--project` | Yes | — | Project name |
| `--environment` | No | — | Export a specific environment only. Mutually exclusive with `--split`. |
| `-o`, `--output` | No | stdout | Output file. With `--split`, this is the output directory. |
| `--split` | No | `false` | Read the project's `spec.environments` and write one manifest per env into `--output`. The directory is created if it does not exist. |

## kip init {#kip-init}

Generate a `kipper.yaml` manifest from a blueprint template.

```bash
kip init --blueprint wordpress --set domain=blog.example.com
kip init --blueprint nodejs -o kipper.yaml
```

| Flag | Required | Default | Description |
|---|---|---|---|
| `--blueprint` | Yes | — | Blueprint name to use as template |
| `--set` | No | — | Parameter values (`key=value`, repeatable) |
| `-o`, `--output` | No | `kipper.yaml` | Output file |
