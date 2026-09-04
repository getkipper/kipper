---
title: 'Environment variables and secrets for Kubernetes apps'
description: 'Set plain environment variables and secrets on an app or a function, see what a change restarts, and keep secrets out of your manifests.'
---

# Secrets & Environment Variables

Kipper separates non-sensitive configuration (environment variables) from sensitive credentials (secrets). They are stored and displayed differently.

## Environment variables

For non-sensitive config like log levels, feature flags, and base URLs.

```bash
# Set one or more variables
kip app env set api LOG_LEVEL=debug API_URL=https://api.example.com

# Load from a file
kip app env set api --from-file .env.production

# List all variables (values visible)
kip app env list api

# Delete a variable
kip app env delete api LOG_LEVEL
```

Values are displayed in plain text in both the CLI and the console UI.

## Changes are saved, then applied

Setting or deleting a variable saves it and leaves the running pods alone. A
container reads its environment once, when it starts, so the pods keep the values
they came up with until something restarts them. The console says so with a
banner; the CLI says so in its output and tells you how to apply it.

```bash
kip app env set api LOG_LEVEL=debug
```

```
  ✔  Environment updated for api
      Saved. The running pods keep the values they started with until api restarts.
      Re-run with --restart to apply it now, or run 'kip app restart api'.
```

Add `--restart` to do both in one step:

```bash
kip app env set api LOG_LEVEL=debug --restart
```

The same applies to `kip app env delete`, `kip app secret set`, `kip app secret
delete` and `kip app secret rollback`, and to `kip function env set`,
`kip function env delete`, `kip function secret set` and `kip function secret
delete`. There is no `kip function secret rollback`.
Restarting drops the connections the workload is serving, which is why it is
something you ask for rather than something a configuration change does on its
own.

## Referencing another variable

An environment variable can reference another by name, and Kipper substitutes the
value when it renders the workload's configuration. The reference is what stays
on the App resource, so `kip export`, a committed `kipper.yaml`, `kip app env
list` and the console Env tab all show `${DB_PASSWORD}`. What it resolved to
lives in the published environment and in the running pod, alongside every other
credential a Kubernetes Secret holds.

This is what a framework wanting one connection string does with the five
variables a Postgres binding injects:

```bash
kip app env set docuseal \
  'DATABASE_URL=postgres://${DB_USERNAME}:${DB_PASSWORD:urlencode}@${DB_HOST}:${DB_PORT}/${DB_NAME}'
```

Single quotes matter. Without them the shell expands `${DB_HOST}` first, finds
nothing, and stores an address with a hole in it.

### What you can reference

Everything Kipper puts in the pod's environment:

| Source | Example names |
|---|---|
| The workload's own env | any other key you set |
| The app's secrets | whatever `kip app secret set` wrote |
| A service binding | `DB_HOST`, `DB_PASSWORD`, `MAIL_PORT`, … |
| A linked app | `DOCUSEAL_URL`, `BILLING_URL`, … |

Variables baked into the container image are not visible to Kipper, so they
cannot be referenced.

Where two sources set the same name, the pod uses the last one to win:
your `env` first, then the app's secrets, then binding credentials in the order
the bindings are declared, then the addresses of linked apps.

### The rules

**`${NAME:urlencode}`** percent-encodes the value for a single URL component.
Use it for anything going between `://` and `@`: a password containing `@`, `:`
or `/` ends the userinfo early and produces a connection error that names the
wrong host.

**`$${NAME}`** is an escape and produces the literal text `${NAME}`. Spring and
several template languages use the same `${}` syntax, so this is how you keep a
value that only looks like a reference.

**A name nothing defines is left exactly as written.** `${DB_HSOT}` reaches the
process as `${DB_HSOT}`, so the connection error names the typo. Substituting an
empty string would produce a connection to no host and a worse error.

**Substitution happens once.** A value referenced by another is used as it was
written, so a reference inside it is not followed:

```bash
# HOST_TEMP=${DB_HOST}
# URL=postgres://${HOST_TEMP}/app   →   postgres://${DB_HOST}/app
```

Reference `${DB_HOST}` directly in `URL` instead. The same rule is why two
variables referencing each other terminate rather than loop.

**Only the values you set are templates.** A secret or a credential containing
`${...}` is passed through as text.

**`$(NAME)` is not a Kipper reference.** Kubernetes uses that form in a pod
spec, and it is easy to reach for by habit, but Kipper resolves `${NAME}` and
leaves everything else alone. Nor does Kubernetes expand it here: your
environment arrives through an `envFrom` reference, and those values are handed
to the container exactly as they are. `$(NAME)` is expanded only in a
container's own `env`, `command` and `args`, which is not where your variables
go. A value written `$(DB_HOST)` therefore arrives at your app as that literal
text. The console flags it when you type it, so the mistake is visible before
you go looking for it in a connection error.

### When a reference does not resolve

An unresolved reference reaches the app as written, so the failure names itself:
a connection error mentioning `${DB_HSOT}` is a typo, and one mentioning
`${DB_PASSWORD}` is a binding that never arrived.

For the second case, `kip service credentials` reports whether each service's
credentials are where a bound workload can read them, and repairs them if not.

The workload also records the answer as an `EnvResolved` condition on its own
resource, covering unresolved names, a variable you set that a binding or link
overrides, and a reference to another template that a single pass cannot follow.

The console's Env tab shows the same three against the variable that causes
each, along with what every value resolves to. Anything drawn from a secret or a
credential is masked there, so the preview shows the shape of the result without
handing back what it was built from. It needs the deploy permission, because the
resolved value is a different thing from the reference you wrote.

## Secrets

For sensitive values like database passwords, API keys, and tokens.

```bash
# Interactive prompt (value hidden, not in shell history)
kip app secret set api DATABASE_URL

# Inline (warns about shell history)
kip app secret set api DATABASE_URL=postgres://user:pass@host/db

# Load from a file
kip app secret set api --from-file .secrets

# List keys only (values always masked)
kip app secret list api

# Reveal a single value
kip app secret reveal api DATABASE_URL

# Delete a secret
kip app secret delete api DATABASE_URL

# Rollback to the previous value
kip app secret rollback api DATABASE_URL
```

Secrets can also be set at deploy time, so the app never has to start without them:

```bash
# Prompted with hidden input, once per bare key
kip app deploy --name api --image ghcr.io/acme/api:latest --port 3000 \
  --secret DATABASE_URL --secret STRIPE_API_KEY

# Inline (warns about shell history)
kip app deploy --name api --image ghcr.io/acme/api:latest --port 3000 \
  --secret API_KEY=abc123
```

Both forms write the same `app-<app>-secrets` Secret as `kip app secret set`, with the same masking, rollback, and export behaviour.

### Automatic previous version

Every time you update a secret, Kipper automatically preserves the previous value (inspired by AWS Secrets Manager). The `list` command shows which keys have a previous version available:

```
$ kip app secret list api

  KEY                            PREVIOUS     VALUE
  DATABASE_URL                   yes          ••••••••
  STRIPE_KEY                                  ••••••••
```

If you accidentally set a wrong value, rollback instantly:

```bash
kip app secret rollback api DATABASE_URL
```

### JSON secrets

If a secret value is valid JSON, `kip app secret reveal` displays it as formatted JSON. This is useful for structured config like database credentials:

```
$ kip app secret reveal api DB_CONFIG
  DB_CONFIG=
  {
    "host": "db.example.com",
    "port": 5432,
    "user": "api",
    "password": "secret",
    "database": "production"
  }
```

### Why the separation?

| | Environment variables | Secrets |
|---|---|---|
| `list` output | Keys and values | Keys only (masked) |
| `set` behaviour | Inline `KEY=VALUE` | Interactive hidden prompt |
| Console UI | Plain text | Masked with reveal button |
| Shell history | Visible | Not recorded (interactive mode) |

## How it works internally

```mermaid
flowchart TD
    A[kip app env set] --> B[App resource: spec.env]
    C[kip app secret set] --> D[Secret: app-api-secrets]
    E[Service binding] --> F[Secret: db-app-api-credentials]
    B --> G[Controller resolves and publishes]
    D --> G
    F --> G
    G --> H[Immutable Secret: app-api-env-9f2c1a7b40de]
    H --> I[Pod: EnvFrom]
    I --> J[App sees all vars as standard environment variables]
```

Your environment variables live on the App resource itself, in `spec.env`.
`kip app env set`, `kip app env list` and the console all read and write that
field, so what you see is what you set. Your secrets live in `app-<app>-secrets`,
and a service binding's credentials live in a Secret of their own. Those three
are the inputs.

The controller reads them, resolves any `${NAME}` references, and writes the
result as a single Secret whose name ends in a digest of its own contents:
`app-api-env-9f2c1a7b40de`. That Secret is immutable, and the pod reads it and
nothing else. Change any value and the contents change, so the digest changes,
so it is a different Secret with a different name.

That is the whole point of the digest. The pod template names one exact object,
so a pod either reads the environment as it was before your change or as it is
after, and never a mixture of the two. When a service password rotates, the new
password and the connection string you composed from it arrive together.

The name carries the workload kind because Secret names are unique per namespace
while the Kubernetes API indexes workload names per kind. Kipper reserves a
workload name across all three kinds, so an app, a function and a job called
`api` cannot coexist in one environment today. Clusters that predate that rule
can still hold such a pair, and the kind-qualified names are what keep those
apart: `app-api-env-…` beside `function-api-env-…`, and `app-api-secrets` beside
`function-api-secrets`, so setting one workload's password leaves the other's
alone. See [names are shared across workload kinds](/en/functions#names-are-shared-across-workload-kinds).

From the application's perspective these are all standard environment variables.
They behave identically at runtime.

### When a change reaches the pod

Setting a variable publishes a new environment straight away, and the console
shows a banner saying a restart is needed to apply it. Your running pods keep
serving the environment they started with until you restart them:

```bash
kip app restart api
```

Anything that replaces the pods anyway picks the new environment up on the way:
a deploy, an image change, or a service credential rotating. So a restart is
only needed when the environment is the only thing that changed.

That also means env values show up in `kip export` output and in a committed `kipper.yaml`, and anyone with read access to App resources can see them. Keep sensitive values out of `kip app env set` and use `kip app secret set` instead, or reference a credential Kipper already injects, since a `${DB_PASSWORD}` reference exports as a reference. Secrets stay in `app-<app>-secrets` and never touch the App resource or an export.

A pod reads its environment and secrets once, at startup, so a running app keeps its current values until it restarts. Nothing restarts it for you. The web console saves the change and shows a "restart to apply" banner with a Restart button; `kip app env set` and `kip app secret set` save it and say the same thing in their output. Click Restart, run `kip app restart <app>`, or pass `--restart` to the command, when you are ready for the new values to take effect. A live service is never cycled without you asking.

The banner reflects the running pods, whichever way the change was made. A `kip apply` that updates an app's `env:` block also leaves the pods on their old values, and the console shows the same banner for that app until it restarts.
