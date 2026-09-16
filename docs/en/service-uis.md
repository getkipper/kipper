---
title: Service UIs & Sharing
description: Open service web interfaces, manage sessions, and share time-limited access.
---

# Service UIs & Sharing

Some managed services include their own web interface. Use this guide to open them through Kipper, share access, and revoke sessions or links. For creating and connecting services, see [Stateful Services](/en/services).

## Browseable service UIs {#browseable-service-uis}

Some service types ship a web UI of their own. MailHog has an inbox viewer, RabbitMQ Management has a queue inspector, and so on. Kipper exposes these at `https://<service>-<namespace>.<cluster-domain>` once the service is running.

The hostname is gated by your console login. Open the URL and one of two things happens:

- If you're signed into the console, the console mints a one-time sign-in code for that host and the page loads with a session already in place. The Open UI button does this for you.
- If you're not signed in, the browser bounces to the console login. After you authenticate, it lands back on the service UI with a fresh session.

Each service UI gets its own session cookie, scoped to that one hostname (a `__Host-` cookie, so it never carries a domain and never travels to another host). No Dex token is ever placed in a cookie. Anonymous requests never reach the backend: the kipper user with their generated password sits on the inside of the auth gate, and a Kubernetes NetworkPolicy restricts the UI port to traffic from the cluster's ingress controller, so pods in the same cluster can't bypass the sign-in and read the UI directly.

The Open UI button on each service's detail page opens the right URL with a fresh sign-in code. From the CLI:

```bash
kip service info mailhog --project blog --environment test
# UI:  https://mailhog-blog-test.example.com
```

On a custom domain the console and every service UI are real subdomains of the cluster domain, so this sign-in works out of the box. On a free `*.kipper.run` cluster the hosts are flat sibling labels under the shared apex, so per-host service-UI sign-in is off there for now; use a share link (below) to hand out access.

Today's caveat: any user who can log into the console can open any service UI. There's no per-team RBAC yet; treat dev tools that have access to sensitive data accordingly (MailHog, for example, holds whatever mail your apps tried to send).

### How the session behaves {#how-the-session-behaves}

A service-UI session slides on a 30-minute idle window: every visit within that window keeps it alive, and the console re-mints the cookie in the background once less than 15 minutes remain. It caps out 12 hours after you first signed in, at which point you re-sign-in (silently, if your console session is still alive). Signing out of the console, or an admin removing your account, ends every service-UI session within about 30 seconds.

What a stolen session cookie grants, and nothing more: one service UI, as you, for up to the idle window (extendable by activity to the 12-hour cap), only while you still hold a role, and killed within about 30 seconds of a logout, an account removal, or a key reset. It opens no other UI (each cookie is pinned to a single host), and it fails the console API and the Kubernetes API outright.

Two behaviours worth knowing:

- If your browser is blocking cookies for a service-UI host, the sign-in can't complete and the console stops after a few attempts with a message naming the host to allow. Allow cookies for that host and reload.
- A UI that only talks to its backend through background requests (not full page loads) can freeze once its session lapses past the idle window. Reload the page and the sign-in re-runs.

### Revoking service-UI sessions {#revoking-service-ui-sessions}

Three levers, in order of blast radius:

- **Sign out.** `kip auth logout` (or the console's sign-out) ends your own service-UI sessions along with the console session.
- **Remove the user.** Deleting a user from the console's Users screen ends their service-UI sessions within about 30 seconds, no other action needed.
- **Revoke everything.** After a suspected cookie or key compromise, rotate the signing key and drop every session at once:

  ```bash
  kip auth sessions revoke-all
  ```

  Every open service UI signs out within about 30 seconds and outstanding sign-in codes stop working. The console session is untouched; people just open their UIs again.

Break-glass, if the CLI is unavailable: delete the signing secret directly over the server-side admin kubeconfig.

```bash
kubectl delete secret kipper-ui-session-signing -n kipper-system
```

Every session dies within about 30 seconds and the next sign-in recreates the key automatically.

### Sharing a UI without a login {#sharing-a-ui-without-a-login}

Sometimes you want to show a service UI to someone who should not have a Kipper account at all, like a client watching magic-link emails arrive during a demo. `kip service share` mints a signed link that opens one service UI for a set time, no login required:

```bash
kip service share mailhog --project blog --environment test --expires 72h --label "PO review"
```

```
  Share link for mailhog (valid until 18 Jul 2026 14:32):

  https://mailhog-blog-test.example.com/?kipper_share=eyJhbGci...

  Anyone with this link can open the UI until it expires.
  Revoke it:      kip service share mailhog --revoke 9f3c1a...
  Revoke all:     kip service share --revoke-all
```

The recipient clicks the link, their browser trades the token for a cookie scoped to that one hostname, and the UI opens. The link works only for that service's UI. It reaches nothing else: not the console, not the API, not any other service.

Treat the link like a password. It is a bearer capability, so anyone who gets hold of it can open the UI until it expires. Keep the expiry short, and remember that mail scanners and chat link previews may open a link the moment you send it. `--expires` accepts a Go duration (`24h`, `72h`); the maximum is `720h` (30 days). The optional `--label` is a note that shows up in the listing, so you can tell links apart later.

Each link is backed by a grant the cluster stores server-side, so you can list and revoke individual links:

```bash
kip service share mailhog --project blog --environment test --list
```

```
  Share links for mailhog:

  ID                                  EXPIRES               CREATED BY                LABEL
  9f3c1a2b4d5e6f7a8b9c0d1e2f3a4b5c    18 Jul 2026 14:32     alice@example.com         PO review
```

Revoke one link by its ID:

```bash
kip service share mailhog --project blog --environment test --revoke 9f3c1a2b4d5e6f7a8b9c0d1e2f3a4b5c
```

You can do all of this from the console too. Open a service with a browseable UI and pick the **Share** tab (admins only). It mints links, lists them with their labels and expiry, and revokes them, plus the emergency controls below.

Grant lookups are cached for 15 seconds, so allow about 15 seconds for a revoked link to stop working across console-api replicas. Signing keys use a separate 30-second cache; the first link created on a cluster can take that long to become usable.

#### If a link leaks {#if-a-link-leaks}

A share link is a capability URL, so once it is out you can't un-send it. Two levers contain a leak:

```bash
# Pull every live share link in the cluster at once.
kip service share --revoke-all

# Retire the signing key so even links that dodged the sweep stop verifying.
# Run it twice — one rotation keeps old links alive until they expire, two
# rotations retire the key completely.
kip service share --rotate-key
kip service share --rotate-key
```

`--revoke-all` clears the grant store; `--rotate-key` is the guaranteed kill switch for a leaked or stolen signing key. For a full compromise, run both: revoke all, then rotate twice. The same two buttons live under **If a link leaked** in the console's Share tab.

### Ingress controller selector {#ingress-controller-selector}

The UI port is locked down with a NetworkPolicy that only lets the cluster's ingress controller talk to it. By default Kipper expects a stock Traefik install: pods labelled `app.kubernetes.io/name: traefik`, in any namespace. If your cluster ships Traefik under a non-standard label, or runs a different ingress controller (Nginx, HAProxy), create a ConfigMap to override:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: ingress-controller
  namespace: kipper-system
data:
  # Pod label that identifies the ingress controller. Required.
  labelKey: app.kubernetes.io/name
  labelValue: traefik

  # Optional: restrict the policy to a specific namespace.
  # Leave unset to match the label in any namespace.
  namespace: traefik
```

Apply with `kubectl apply -f` and the next reconcile of any service-UI NetworkPolicy picks it up, with no console-api restart and no Kipper rebuild. If the ConfigMap is missing the defaults apply, so existing clusters keep working unchanged.
