# Blueprints

Blueprints are pre-built application stacks that you can install with a single command. Each blueprint includes everything needed for a working deployment: the application, database, storage, and configuration.

## Browsing blueprints

```bash
kip blueprint list
```

```
  NAME                 VERSION  DESCRIPTION
  ghost                1.0      Ghost publishing platform with MySQL database
  gitea                1.0      Gitea self-hosted Git service with PostgreSQL
  plausible            1.0      Plausible Analytics, privacy-friendly web analytics with PostgreSQL
  wordpress            1.0      WordPress blog with MySQL database and persistent uploads
```

## Blueprint details

View what a blueprint will create and its configurable parameters:

```bash
kip blueprint info wordpress
```

```
  wordpress (v1.0)
  WordPress blog with MySQL database and persistent uploads

  Parameters:
    projectName          Project name (required)
    environment          Target environment (e.g. test, prod)
    storageSize          Storage size for uploads [default: 5Gi]
    dbStorage            Database storage size [default: 5Gi]

  Install:
    kip blueprint install wordpress --set projectName=my-project
```

## Installing a blueprint

Install directly to your cluster:

```bash
kip blueprint install wordpress --set projectName=my-blog
```

```
  ✔  Namespace my-blog created

  Installing wordpress into my-blog...
    ✔  App/wordpress created
    ✔  Service/db created
    ✔  Volume/uploads created

  ✔  wordpress installed (3 resources)
```

### With custom parameters

Override any parameter with `--set`:

```bash
kip blueprint install wordpress \
  --set projectName=company-blog \
  --set storageSize=20Gi \
  --set dbStorage=10Gi \
  --environment prod
```

### Into a specific environment

```bash
kip blueprint install ghost \
  --set projectName=my-site \
  --set domain=https://blog.example.com \
  --environment prod
```

## Generating a manifest

Generate a `kipper.yaml` file that you can review and customise before applying, instead of installing directly:

```bash
kip init --blueprint wordpress --set projectName=my-blog
```

```
  ✔  Generated kipper.yaml from wordpress blueprint
     Edit it, then run: kip apply -f kipper.yaml
```

This creates a standard `kipper.yaml` file, not a special blueprint format, just a regular manifest. Edit it to add routes, environment variables, resource profiles, or any other configuration. Then apply:

```bash
kip apply -f kipper.yaml
```

## Available blueprints

### WordPress

Full WordPress installation with MySQL database and persistent upload storage.

| Component | Details |
|---|---|
| App | `wordpress:6-apache` on port 80 |
| Database | MySQL with configurable storage |
| Storage | Persistent volume for `wp-content/uploads` |
| Service binding | MySQL credentials injected via `WORDPRESS_DB_` prefix |

### Ghost

Ghost publishing platform with MySQL backend.

| Component | Details |
|---|---|
| App | `ghost:5-alpine` on port 2368 |
| Database | MySQL with configurable storage |
| Parameters | `domain`: the public URL for Ghost |

### Gitea

Self-hosted Git service with PostgreSQL and persistent repository storage.

| Component | Details |
|---|---|
| App | `gitea/gitea:1.22-rootless` on port 3000 |
| Database | PostgreSQL with configurable storage |
| Storage | Persistent volume for Git repositories |

### Plausible Analytics

Privacy-friendly web analytics.

| Component | Details |
|---|---|
| App | `ghcr.io/plausible/community-edition:v2.1` on port 8000 |
| Database | PostgreSQL with configurable storage |
| Parameters | `baseUrl` (required): the public URL for analytics |

### Medusa

Open source e-commerce platform (Shopify alternative).

| Component | Details |
|---|---|
| App | `medusajs/medusa:latest` on port 9000 |
| Database | PostgreSQL with configurable storage |
| Cache | Redis |

### n8n

Workflow automation platform (Zapier alternative).

| Component | Details |
|---|---|
| App | `n8nio/n8n:latest` on port 5678 |
| Database | PostgreSQL with configurable storage |

### Uptime Kuma

Monitoring and status page. No database required. Data stored on a persistent volume.

| Component | Details |
|---|---|
| App | `louislam/uptime-kuma:1` on port 3001 |
| Storage | Persistent volume for monitoring data |

### Outline

Team wiki and knowledge base.

| Component | Details |
|---|---|
| App | `outlinewiki/outline:latest` on port 3000 |
| Database | PostgreSQL with configurable storage |
| Cache | Redis |
| Storage | MinIO for file attachments |
| Parameters | `domain` (required): the public URL |

### Cal.com

Open source scheduling platform (Calendly alternative).

| Component | Details |
|---|---|
| App | `calcom/cal.com:latest` on port 3000 |
| Database | PostgreSQL with configurable storage |
| Parameters | `domain` (required): the public URL |

### Invoice Ninja

Invoicing and billing platform.

| Component | Details |
|---|---|
| App | `invoiceninja/invoiceninja:5` on port 9000 |
| Database | MySQL with configurable storage |
| Storage | Persistent volume for documents |
| Parameters | `domain` (required): the public URL |

### Mattermost

Team messaging platform (Slack alternative).

| Component | Details |
|---|---|
| App | `mattermost/mattermost-team-edition:11.7.2` on port 8065 |
| Database | PostgreSQL (10Gi default) |
| Storage | Persistent volume for uploads and attachments |
| Parameters | `domain` (required): the public URL. Optional: `smtpHost`, `smtpPort`, `smtpUsername`, `smtpPassword`, `smtpFrom`, `smtpSecurity`, `pushNotificationServer` |

**Email notifications.** Set `smtpHost` (plus `smtpUsername`, `smtpPassword`, and `smtpFrom`) and Mattermost sends email from first boot. `smtpSecurity` defaults to STARTTLS on port 587, which is what most hosted mail relays use. Set it to TLS for providers that want implicit TLS on port 465, or none for an unauthenticated internal relay. Leave `smtpHost` empty and no email config is written, so you can still set it later in the System Console.

**Mobile push.** Leave `pushNotificationServer` empty and push is off. The free Team Edition image we ship can only reach Mattermost's test push proxy (`https://push-test.mattermost.com`), which works with the official app store apps but is rate limited and meant for evaluation, not production. Production push runs through Mattermost's hosted service and needs a paid Mattermost plan plus the enterprise-edition image, so it's a deliberate later step, not a default.

### Rocket.Chat

Team communication platform.

| Component | Details |
|---|---|
| App | `rocket.chat:8.4.3` on port 3000 |
| Database | MongoDB (10Gi default) |
| Parameters | `domain` (required): the public URL. Optional: `smtpHost`, `smtpPort`, `smtpUsername`, `smtpPassword`, `smtpFrom`, `smtpSecurity` |

**Email notifications.** Same SMTP parameters as Mattermost. Set `smtpHost` and the rest, and Rocket.Chat writes the SMTP settings on startup (it uses the `OVERWRITE_SETTING_` mechanism, so the values are locked in the admin panel). `smtpSecurity` defaults to STARTTLS on port 587.

**Mobile push.** Rocket.Chat push is not something the blueprint can switch on, because it runs through Rocket.Chat Cloud. After install, connect the workspace to Rocket.Chat Cloud from the admin panel (Settings, then Connectivity). That's the step that lights up push for the official app store apps. The free community tier includes a monthly push allowance through that gateway. If you skip the Cloud connect, push silently never fires, which is the usual reason notifications "don't work" on a fresh Rocket.Chat.

## How blueprints work

A blueprint is a `kipper.yaml` template with Go template placeholders. When you install a blueprint, Kipper:

1. Loads the template from the built-in registry
2. Applies your parameter values (with defaults for optional ones)
3. Renders the template to a standard Kipper manifest
4. Creates the namespace if needed
5. Applies all resources to the cluster

The rendered output is identical to a hand-written `kipper.yaml`, with no special tracking or metadata. You can export it later with `kip export` and manage it via GitOps like any other manifest.

## Creating custom blueprints

A blueprint file contains two YAML documents separated by `---`:

1. **Metadata:** name, description, version, parameters
2. **Template:** a `kipper.yaml` with Go template placeholders (e.g. `.projectName`)

The file has two YAML documents separated by `---`. The first document defines the metadata and parameters. The second document is the manifest template using Go `text/template` syntax. Placeholders like `.projectName` and `.replicas` are replaced with parameter values at render time.

After rendering with `--set projectName=acme --set replicas=3`, the template produces:

```yaml
project: acme

apps:
  api:
    image: registry.example.com/api:latest
    port: 8080
    replicas: 3

services:
  db:
    type: postgres
    storage: 5Gi
```

All parameter values are strings. Use quotes in the template for numeric fields that YAML might interpret differently.

Parameters use Go `text/template` syntax. All parameter values are strings. Use quotes in the template for numeric fields that YAML might interpret differently.
