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
Choose a suitable time to restart: replacing pods can interrupt active connections.

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

Use single quotes to pass `${DB_HOST}` and the other references literally to Kipper, ready for resolution against the app's environment.

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

**Unresolved names remain literal.** For example, `${DB_HSOT}` reaches the process unchanged, making a missing or mistyped name visible.

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

**Use `${NAME}` for Kipper references.** The Kubernetes-style `$(NAME)` form remains literal in Kipper's published environment. The console flags this form so you can correct it before restarting.

### When a reference does not resolve

Check the variable name and its source. A reference such as `${DB_HSOT}` may contain a typo; a missing `${DB_PASSWORD}` may indicate a missing or unusable binding.

`kip service credentials` audits service credentials. Review the report before using its `--repair` option; see [credential recovery](/en/services#checking-that-a-service-owns-its-credentials).

The workload's `EnvResolved` condition and the console's Env tab report unresolved names, overridden variables, and references to other templates. The preview shows resolved values with secret-derived parts masked. Access requires the `env.reveal` capability, held by built-in deployers and owners.

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

Every time you update a secret, Kipper preserves the previous value. The `list` command shows which keys have a previous version available:

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

### Variables and secrets at a glance

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

The controller resolves `${NAME}` references and publishes an immutable Secret named from a digest of its contents, such as `app-api-env-9f2c1a7b40de`. The pod template refers to that exact version, keeping related values—such as a password and a connection string—together.

Names include the workload kind (`app`, `function`, or `job`). This also keeps configuration separate for same-name workloads on older clusters. New workloads follow the [shared naming rule](/en/functions#names-are-shared-across-workload-kinds).

Inside the container, the resolved values are ordinary environment variables.

### When a change reaches the pod

An environment-only edit publishes a new version while preserving the Deployment's current template. Apply it with the console's **Restart** button, `kip app restart api`, or `--restart` on the configuration command.

Other changes that replace pods, such as an image update or a service-credential rotation, also pick up the latest environment. The restart banner compares the published version with the version referenced by the Deployment template; it is not a health check for every running pod.

Plain environment values remain visible in the App resource and in `kip export`. Store sensitive values with `kip app secret set`, or use a reference such as `${DB_PASSWORD}` so the manifest contains the reference. Secret values are kept out of the export.
