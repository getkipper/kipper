# Security

Kipper takes security seriously. Every cluster is hardened by default with production-grade security controls, no configuration required.

::: warning Shared responsibility
Kipper provides a strong baseline of infrastructure-level security: encrypted traffic, security headers, rate limiting, isolated namespaces, and automatic backups. However, **security is ultimately the responsibility of the development and operations teams** building on top of Kipper.

No platform can protect against insecure application code, weak passwords, leaked credentials, or misconfigured services. Kipper gives you the foundation, and you are responsible for building securely on top of it. This includes writing secure application code, managing secrets carefully, keeping dependencies updated, and following the principle of least privilege.

We encourage you to treat Kipper's security features as a baseline, not a guarantee. Review your own application security regularly and follow the best practices documented below.
:::

## What Kipper provides out of the box

### TLS everywhere

All traffic is encrypted with TLS certificates issued automatically by Let's Encrypt via cert-manager. HTTP requests are redirected to HTTPS at the ingress entrypoint, before any route or API key is processed.

`X-Forwarded-*` headers are honoured only from trusted proxies: the kipper.run gateway (trusted automatically when your cluster uses a kipper.run domain) and any addresses you pass with `--trusted-proxy`. From anyone else they are dropped. On the kipper.run path the gateway drops any client-supplied `X-Forwarded-For` and sets it to the address it measured, so the source IP in your logs is trustworthy. If you put your own load balancer in front and trust it with `--trusted-proxy`, the logged source is only as trustworthy as that balancer: most append to the client-supplied chain rather than replacing it, so treat the leftmost address as a claim and cross-check the `forwarded_for` chain that kipper-authz logs. Source IP is used for logging only, never to decide whether a key is valid.

On a `*.kipper.run` domain, the public TLS terminates at the gateway and the request travels to your cluster over a second HTTPS hop. Let's Encrypt cannot issue for that hop (its challenge would end at the gateway), so your cluster serves a stable Kipper-managed certificate instead, and the gateway verifies every connection against a pinned SHA-256 of that certificate's public key. Your cluster asserts the fingerprint through its daily gateway heartbeat, authenticated by the registration token it received at install time. Once the pin is active, someone sitting on the network path between the gateway and your server can only break connections visibly, never read or alter them, apart from a bounded window: for two minutes after a cluster's very first pin activation the gateway still accepts an as-yet-unseen certificate, so a lagging Traefik replica does not 502 during rollout. That two-minute tolerance is gone once proof-before-route is on, because then the served key must also hold a proof. A freshly installed cluster proxies unverified for the first moments until its console-api asserts the fingerprint, which normally happens seconds after installation completes. Rotating the pinned key is an explicit administrative step: annotate the `kipper-hop-cert` Secret in `kipper-system` with `kipper.run/rotate-key`, and the cluster stages a new key, clears it with the gateway, and swaps it in with both keys accepted during the handover.

A `*.kipper.run` name is meant to route to a cluster only once that cluster has proven it controls the destination. Registration alone is not enough: the gateway issues a fresh challenge, and your cluster signs it with the same hop-certificate private key it serves at its IP. The gateway checks the signature against the public key it observes at that IP, so possession of the private key, not knowledge of the public certificate, is what proves control. Someone cannot point a `*.kipper.run` name at a server they do not control and have the gateway serve it, because that server cannot produce the signature. The proof is a renewable lease refreshed by the heartbeat, so a decommissioned cluster's name stops routing on its own. The lease covers one specific key, the one whose possession was proven, and while proof-before-route is on the gateway opens a hop connection only to that key. Any other certificate is refused, including one the pin set would otherwise tolerate during a rotation or a rollout. When the key changes, the gateway stops sending requests over the connections it had opened to the old one, so a pooled connection cannot outlive the proof that allowed it. Rotating the hop key therefore converges over two heartbeats: the first stages the new key and clears it with the gateway, the second asserts it and proves it, and traffic flows again as soon as that proof lands, about a minute later. This proof-before-route enforcement is enabled at the kipper.run gateway once the fleet has acquired proofs; the gateway operator turns it on, so a cluster does not control it.

### Host hardening

Before installing Kubernetes, `kip install` audits the server for surplus services exposed on public interfaces and disables them. It then installs a host firewall with rules that work correctly with k3s pod networking.

This matters because Kipper installs on whatever Linux server you point it at, and many distro defaults leave services exposed on the public network that you wouldn't want there. A default Ubuntu install ships `nfs-common` (pulled in by Longhorn), which pulls in `rpcbind` listening on `0.0.0.0:111`. Open portmappers are abuse vectors for DDoS reflection and routinely trigger upstream-provider abuse reports. Kipper closes this by default so you don't have to know it exists.

**What gets hardened**

- `rpcbind`. Disabled and the socket is masked so it cannot be re-enabled by a package upgrade. Reversible with `systemctl unmask rpcbind.socket`.

**What the firewall opens**

UFW is installed with a default-deny incoming policy. The following ports are allowed:

| Port | Reason |
|---|---|
| Detected SSH port (22 by default) | Reading `sshd -T` so non-default ports are honoured |
| 80, 443 | Traefik ingress |
| 6443 | k3s API |

On a fresh install, pods reach the host only on the specific ports k3s needs, the kubelet (`10250`) and node-exporter (`9100`), rather than every host port, so a compromised pod cannot reach SSH or other host services. Pod-to-apiserver traffic arrives on `6443` after the service address is translated, already covered above. UFW's `DEFAULT_FORWARD_POLICY` is set to `ACCEPT` so pod-to-pod and pod-to-service traffic is not blocked. A NodePort or host-network service you add yourself needs its own UFW rule, since the pod-to-host default is now deny.

The kubelet runs with `protect-kernel-defaults`, so it refuses to start unless the host's kernel tunables already match the values Kubernetes expects. `kip install` sets those (OOM handling, panic-on-oops self-healing, and the kernel keyring limits containers rely on) before k3s starts, rather than letting the kubelet silently overwrite them.

**Existing firewall detected**

If `kip install` finds a firewall already active on the host, what happens next depends on whose it is. A firewall you set up is left alone: Kipper skips its own firewall step rather than layer rules on top of someone else's policy, and prints a notice that your firewall configuration is now your responsibility, with 22, 80, 443, and 6443 needing to stay reachable. A firewall Kipper set up earlier is Kipper's to bring back in line, so it gets reconfigured.

Kipper tells the two apart by a file at `/etc/kipper/firewall-managed`, written as the last step of the first configuration command it runs against UFW. The file records that this command completed, which is what makes the ruleset Kipper's. A run that fails before then leaves no file, so it never vouches for a firewall you set up afterwards, and a run that fails after it leaves the file over the half-built firewall, so the retry recognises the wreckage and puts it right. Without this, an install that failed after enabling UFW would leave a host whose retry silently inherited the half-finished ruleset.

Nothing rolls back if the file write itself fails: the change stays and the host is left unclaimed, which errs the safe way, since Kipper then reads the host as yours and keeps its hands off. The file is written under a temporary name of that run's own and moved into place, so a half-written one never appears, and two Kipper runs on the same host cannot publish each other's partial writes.

The file only ever speaks for UFW. If firewalld is running, Kipper leaves the host alone whatever the file says, because firewalld is not something Kipper manages.

Kipper also re-checks the host immediately before it starts work, since the first check happens before host hardening and the k3s install and can be minutes old by then. A firewall that comes up in that gap is still read as yours. The re-check narrows the window rather than removing it: a firewall enabled in the moment between that check and the first change would still be taken as Kipper's, which is why the file is worth knowing about if you configure firewalls on a host while an install is running.

`--firewall=false` stops the step either way, on your firewall and on Kipper's own.

Delete the file to take the firewall over yourself, and Kipper treats it as yours from then on. Firewalls set up before this file existed carry no claim, so they read as yours and keep their rules.

Cloud-side firewalls (Hetzner Cloud Firewall, AWS Security Groups, etc.) are external to the host and are not detected by Kipper. If you use one, make sure those four ports are reachable on the server.

**Opting out**

```bash
kip install --host <ip> --harden=false     # leave surplus services running
kip install --host <ip> --firewall=false   # do not install UFW
```

When opted out, `kip install` still prints any findings so you know what is being left in place. Use these flags only when you manage host security yourself.

**Retro-fitting an existing cluster**

For clusters installed before host hardening was the default, apply the same defaults in place:

```bash
kip cluster harden                    # uses the current cluster
kip cluster harden --cluster storefront  # specific cluster
kip cluster harden --firewall=false   # only disable surplus services
```

The command is idempotent. It runs the same audit as `kip install`, then disables `rpcbind` and configures UFW (skipping the firewall step if another firewall is already active).

### Security headers

Every response from your applications includes security headers enforced at the ingress level:

| Header | Value | Protection |
|---|---|---|
| `Strict-Transport-Security` | `max-age=31536000; includeSubDomains; preload` | Forces HTTPS for 1 year |
| `X-Frame-Options` | `SAMEORIGIN` | Prevents clickjacking |
| `X-Content-Type-Options` | `nosniff` | Prevents MIME type sniffing |
| `X-XSS-Protection` | `1; mode=block` | Blocks reflected XSS attacks |
| `Content-Security-Policy` | Restricts script, style, image, and connection sources to `self` | Prevents XSS and data injection |
| `Referrer-Policy` | `strict-origin-when-cross-origin` | Controls referrer information leakage |
| `Server` / `X-Powered-By` | Removed | Hides server technology from attackers |

Each app gets its own Traefik middleware with these headers. You do not need to configure them in your application.

### Customising security settings per app

The defaults work for most applications. Settings can be changed per app from the web console (app detail → Settings tab) or the CLI:

```bash
# Disable security headers for a specific app
kip app deploy --name api --image myimg --port 3000 --no-security-headers

# Set a custom rate limit (requests per second)
kip app deploy --name public-api --image myimg --port 8080 --rate-limit 500
```

Apps without these flags use the cluster defaults (security headers enabled, 100 req/s rate limit).

### CSP allowlist

The default Content Security Policy blocks external resources. If your app loads fonts, stylesheets, scripts, or connects to APIs on other domains, add them to the CSP allowlist.

**From the web console:** Open the app's Settings tab → enter comma-separated domains in the CSP allowlist field → Save.

**Example:** To load Google Fonts, add `fonts.googleapis.com` to the allowlist. The domains are added to `style-src`, `font-src`, `script-src`, and `connect-src` directives.

Functions have the same CSP settings. Open the function detail panel, then the Settings tab.

::: tip
Only add domains you trust. Each allowlisted domain can serve scripts, styles, and fonts to your users.
:::

### Rate limiting

All endpoints are rate-limited to **100 requests per second per IP address** with a burst allowance of 200. This protects against:

- Brute force attacks on login endpoints
- Basic DDoS attacks
- Credential stuffing
- API abuse

The rate limit applies at the ingress level before traffic reaches your application.

### Pod Security Standards

Kipper enforces the Kubernetes [Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/) baseline profile on all user namespaces. This warns when deployments attempt to:

- Run containers as root
- Use privileged mode
- Access the host network or host PID namespace
- Use host path volumes
- Escalate privileges

System namespaces (kube-system, monitoring, etc.) are excluded from enforcement to allow infrastructure components to function.

### Authentication

The web console is protected by Dex (OAuth2/OIDC identity provider) with bcrypt-hashed passwords. API access requires a valid JWT token.

Service-UI share links are the one deliberate exception: an admin can mint a signed, expiring link that opens a single browseable service UI without a Dex login. Each link is a bearer capability backed by a server-side grant, structurally separate from Dex tokens, and it reaches nothing but that one UI. If a link leaks, revoke it (or revoke every link) and rotate the signing key twice. See [Sharing a UI without a login](/en/services#sharing-a-ui-without-a-login) for the full model and the leak runbook.

### Secrets management

Application secrets are stored as Kubernetes Secrets, encrypted at rest by k3s. They are:

- Never returned in API list responses (only revealed on explicit request)
- Scoped to the project namespace (one project cannot access another's secrets)
- Support previous version tracking and rollback
- Injected into containers via `envFrom` (not visible in deployment YAML)

### Revealing stored credentials

Git credentials and container-registry credentials stored under Settings are masked in both the console UI and the list API. If you lose your copy of a token, you can recover it two ways:

- **In the console:** click the eye icon next to a credential in Settings. You'll be asked to re-enter your password before the value is shown. It stays visible for 30 seconds, then re-masks.
- **From the CLI:** run `kip credentials get <name>`. This reads the secret directly from the cluster, so it requires kubeconfig access to `kipper-system` (effectively cluster-admin).

Only users with the `admin` role can use the console reveal flow for these stored credentials. Both paths log an audit line on success and on failed attempts.

A token set per app (under the app's Git settings) is a separate case. It lives in a secret in the app's own namespace, not under Settings, so it does not appear in the lists above. Reveal it from the app's Git source panel with the same password re-entry, or from the CLI with `kip credentials get --app <app> --project <p> --environment <e>`. The app reveal is open to the `deployer` role, looser than the admin-only Settings reveal, because a deployer already manages the app's git source and can rotate that token. Scope app tokens per repository if you don't want a deployer recovering a credential that reaches other repos.

### Namespace isolation

Each project environment runs in its own Kubernetes namespace. This provides **logical isolation**: each namespace has its own Deployments, Services, Secrets, and ConfigMaps. A project cannot access another project's secrets or environment variables.

### Network isolation between namespaces

Kipper installs a NetworkPolicy called `kipper-workload-egress` in every project namespace, before
the namespace is usable. It denies egress by default and then permits three things: DNS, pods in the
same namespace, and the public internet with the cluster's own internal and node ranges excluded.

The practical effect is that a pod in `blog-test` cannot open a connection to a service in
`blog-prod`, or in any other project's namespace, even knowing its DNS name. Test reaching a prod
database is the case this closes, and it does not need separate clusters.

Two limits worth knowing:

- **Ingress is deliberately unrestricted.** Traefik has to reach your pods to serve them, and KEDA
  and the metrics stack have to scrape them. The boundary is on what a workload can call out to,
  not on what can call in.
- **The public-egress rule depends on the cluster knowing its own address ranges.** When those
  cannot be read, the namespace still gets the deny baseline of DNS and same-namespace traffic, and
  public egress is left out rather than opened.

Cross-namespace access that you *want* is arranged explicitly with [app links](/en/deploying-apps),
which open a path between two named workloads rather than between whole namespaces.

::: tip Per-project resource quotas
On shared clusters, assign each project a tier: every environment namespace then carries a ResourceQuota capping total CPU and memory, so one busy project can never crowd out the others. Projects without a tier run uncapped, which is fine for a single-team cluster. See [Project quotas](/en/resource-management#project-quotas) for tiers, per-environment overrides, and what happens when a namespace hits its ceiling.
:::

### Project access and roles

Runtime isolation is separate from who is allowed to touch a project. Access is decided by membership: a person only sees and works on projects they belong to, with a per-project role (Viewer, Deployer, or Owner) that sets what they can do. Cluster admins see and manage everything. See [Project Members](/en/project-members) for the full model.

### Membership is not the same as owning the namespace

A project's members are recorded on the Project. Which namespace a project *has* is recorded on the
namespace itself, as a `kipper.run/project` label, and that label is what every request is checked
against.

The two agree except in one state: two projects can resolve to the same namespace name. Project
`shop` with an environment `prod` and a project called `shop-prod` both point at `shop-prod`, and
whichever reconciled first holds it. The other is left with a declaration it does not own, recorded
as a conflict rather than resolved, because renaming somebody's live namespace out from under them
is worse than saying so.

So a member of the losing project gets `403` on that environment despite being a genuine member of
the project that names it. Nothing is broken and nothing needs repairing except the collision:
rename one of the two projects, or the environment. `kip project list` shows both, and the Project's
own status carries the conflict.

The same rule is why copying an environment refuses a namespace that is not the project's, rather
than writing into it.

### Platform service account scope

Project roles are enforced by the console API, not by Kubernetes RBAC. The console API runs with a service account that can read and write Secrets in every namespace and open exec sessions in application pods. That breadth is inherent to what the console does: it creates each project's secrets, injects service credentials, and runs the database console, file browser, and data migration inside service pods.

Project isolation therefore holds for console users, because every request passes the membership check first. It places no limit on anyone who can reach the Kubernetes API directly with a kubeconfig or with the console API's service account token. Treat kubeconfig access and the `kipper-system` namespace as equivalent to cluster admin and guard them accordingly.

### Automatic backups

Daily backups at 3:00 AM include all Kubernetes resources and persistent volume data (databases). Backups are retained for 7 days with one-click restore.

## Docker image best practices

Kipper works with any Docker image, but for production deployments we recommend following these security practices:

### Run as a non-root user

```dockerfile
FROM node:20-alpine

# Create a non-root user
RUN addgroup -S app && adduser -S app -G app

WORKDIR /app
COPY --chown=app:app . .
RUN npm ci --production

# Switch to non-root user
USER app

EXPOSE 3000
CMD ["node", "server.js"]
```

For Java applications:

```dockerfile
FROM eclipse-temurin:21-jre-alpine

RUN addgroup -S app && adduser -S app -G app

WORKDIR /app
COPY --chown=app:app build/libs/*.jar app.jar

USER app

EXPOSE 8080
CMD ["java", "-jar", "app.jar"]
```

### Use minimal base images

Prefer Alpine-based images (`node:20-alpine`, `eclipse-temurin:21-jre-alpine`) over full Debian images. Smaller images have fewer packages and a smaller attack surface.

### Don't store secrets in images

Never bake secrets, API keys, or passwords into Docker images. Use Kipper's secret management instead:

```bash
kip app secret set my-app DATABASE_URL
kip app secret set my-app API_KEY
```

These are injected as environment variables at runtime, not stored in the image.

### Pin image versions

Use specific version tags instead of `:latest` in production:

```bash
# Development
kip app deploy --name api --image registry.git.example.com/api:latest --port 8080

# Production
kip app update api --image registry.git.example.com/api:v1.2.3
```

### Scan images for vulnerabilities

Before deploying to production, scan your images for known CVEs:

```bash
# Using Trivy (free, open source)
trivy image registry.git.example.com/api:v1.2.3
```

Kipper does not run image scanning automatically. Build scanning into your CI pipeline so vulnerable images never reach the cluster.

## Basic authentication

You can password-protect any app with HTTP basic auth. This is useful for staging environments, internal tools, or documentation sites that aren't ready for public access yet.

Basic auth is not a substitute for proper authentication (Dex, SSO). It's a simple access gate. Use it when you need a quick way to keep casual visitors out, not for production security.

### Enabling via the console

Open the app's **Settings** tab and scroll to **Basic authentication**. Enter a username and password, then click **Add user**. The app is immediately protected. Visitors see a browser login prompt.

You can add multiple users. Each one gets their own credentials.

To remove basic auth entirely, click **Remove all**. The password prompt disappears and the app is publicly accessible again.

### How it works

Kipper creates a Traefik `basicAuth` middleware that checks credentials before requests reach your app. Usernames and bcrypt-hashed passwords are stored in a Kubernetes Secret named `{app}-basic-auth` in the app's namespace.

Your app never sees the authentication. Traefik handles it at the ingress level.

### Limitations

- No per-user access control. All users with valid credentials see the same content.
- No session management. The browser sends credentials on every request.
- Credentials are sent base64-encoded (not encrypted) in the Authorization header. Always use HTTPS.
- For proper user authentication with sessions, tokens, and roles, use Dex and the built-in auth system.

## Step-up 2FA for destructive operations

Cluster migration, the operation that copies every secret and database to another cluster, requires a TOTP code from an enrolled authenticator app on top of the admin login, and the factor must be at least 7 days old. Enrollment itself needs a one-time code issued from the cluster host with `kip 2fa bootstrap`, so a stolen console login can never enroll its own device. Every migration event and every 2FA change alerts all admins out-of-band.

The gate protects against a compromised console identity, not against a compromised server or kubeconfig. Someone with root on the node or the cluster-admin kubeconfig bypasses the console entirely. Harden the host and guard the kubeconfig accordingly. The full model, including the host-pinned notification channels and the outbound-migration kill switch, is in [Cluster Migration → Security model](/en/migration#security-model).

## Your responsibilities

Regardless of what infrastructure-level protections Kipper provides, your team is responsible for:

- **Secure application code:** parameterised queries, input validation, output encoding, CSRF protection
- **Dependency management:** keeping libraries and base images up to date, monitoring for CVEs
- **Secret hygiene:** never committing credentials to git, rotating secrets regularly, using the hidden prompt (`kip app secret set <app> KEY` without `=VALUE`)
- **Access control:** limiting who can deploy to production, using strong passwords, enrolling 2FA early so the factor is mature when a migration needs it
- **Kubeconfig custody:** `kip cluster export` hands out full cluster admin; know who holds a copy, store it like a root credential, and never paste it into chat, tickets, or shell history
- **Monitoring:** reviewing Grafana dashboards and Loki logs for suspicious activity, and making sure SMTP or Slack is configured so security alerts leave the box

## Verifying your security headers

You can verify that security headers are active on your deployment:

```bash
curl -sI https://your-app--203-0-113-10.kipper.run | grep -i 'strict-transport\|x-frame\|x-content\|x-xss\|content-security\|referrer'
```

Expected output:

```
strict-transport-security: max-age=31536000; includeSubDomains; preload
x-frame-options: SAMEORIGIN
x-content-type-options: nosniff
x-xss-protection: 1; mode=block
content-security-policy: default-src 'self'; ...
referrer-policy: strict-origin-when-cross-origin
```
