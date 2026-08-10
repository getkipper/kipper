# GitOps

Kipper supports declarative infrastructure through a `kipper.yaml` manifest file. Define your entire project (apps, services, volumes, and jobs) in a single file, commit it to Git, and apply it to the cluster.

## The kipper.yaml format

```yaml
project: blog
environment: test
environments:
  - test
  - acc
  - prod
displayName: My Platform

apps:
  frontend:
    image: registry.git.example.com/frontend:latest
    port: 80
    route:
      group: blog
      path: /

  api:
    image: registry.git.example.com/api:latest
    port: 8080
    replicas: 2
    resources:
      profile: jvm
    env:
      LOG_LEVEL: info
      DATABASE_URL: postgres://${DB_USERNAME}:${DB_PASSWORD:urlencode}@${DB_HOST}:${DB_PORT}/${DB_NAME}
    serviceBindings:
      - name: db
        prefix: DB_
    autoscale:
      enabled: true
      minReplicas: 2
      maxReplicas: 5
      cpuTarget: 70

services:
  db:
    type: postgres
    version: "16"
    storage: 5Gi
    resources:
      cpuRequest: 250m
      cpuLimit: 750m
      memoryRequest: 1Gi
      memoryLimit: 1Gi

volumes:
  uploads:
    size: 10Gi
    mounts:
      - app: api
        mountPath: /data/uploads

functions:
  image-resize:
    image: registry.git.example.com/resizer:latest
    port: 8080
    triggers:
      - type: minio
        config:
          source: storage

jobs:
  cleanup:
    image: registry.git.example.com/cleanup:latest
    schedule: "0 3 * * *"
    env:
      RETENTION_DAYS: "30"
      LOG_LEVEL: info
```

### The route block

`route` is the largest block an app can carry. Every field is optional, and the manifest is the only
place several of them can be set at all — `kip app deploy` covers `host`, `group`, `path`,
`redirectFrom`, `rateLimit` and `noSecurityHeaders`, and the rest are manifest or console.

| Field | What it does |
|---|---|
| `host` | The hostname this app serves. Leave it out on a `*.kipper.run` cluster and the app gets a derived subdomain. |
| `redirectFrom` | Other hostnames that answer `301` to `host`, same path and query. Up to 10, each needing its own DNS record. See [redirect domains](/en/domains#redirect-domains). |
| `path` | The path prefix this app answers on, for sharing a hostname through `group`. |
| `group` | Serves several apps on one hostname, each on its own `path`. See [route groups](/en/domains#route-groups). |
| `redirects` | URL rewrite rules within the hostname this app already serves. See [Redirects](/en/redirects). |
| `rateLimit` | Requests per second per client IP before Traefik starts refusing. |
| `requireApiKey` | Gates the route behind an API key. See [API keys](/en/api-keys). |
| `basicAuth` | Gates the route behind HTTP basic auth. See [Security](/en/security). |
| `cspAllowlist` | Extra origins to permit in the Content-Security-Policy header. |
| `noSecurityHeaders` | Drops the security header middleware, for an app that sets its own. |
| `noInstanceHeader` | Drops the header naming which pod answered. |

```yaml
apps:
  shop:
    image: registry.git.example.com/shop:latest
    port: 3000
    route:
      host: example.com
      redirectFrom:
        - www.example.com
        - old-brand.example
      rateLimit: 100
      requireApiKey: false
```

A `redirectFrom` entry has to be at least two lowercase DNS labels, and `kip apply` refuses the
manifest before applying anything if one is malformed or the list exceeds 10. `*.kipper.run`
subdomains cannot be redirect domains. The same list is settable imperatively:

```bash
kip app update shop --redirect-from www.example.com,old-brand.example
```

`kip apply` and the CLI flag run the same validation, so a hostname one accepts the other cannot
refuse. Both replace the whole list: the manifest because apply owns the spec, and the flag because
it writes what you pass. The console stores what you type and lets the reconciler skip an invalid one, so a hostname
entered there is not checked in advance.

Each file must have a `project` field. The `environment` field determines which namespace the resources are created in (`blog-test` in this example).

The `environments` and `displayName` fields are optional. When present, `kip apply` creates the Project CR and any missing namespaces automatically, making the manifest self-contained for bootstrapping a new cluster.

The Project itself is merged, not replaced. A Project also holds fields the manifest never carries, such as members, tier, quota and shared storage, which admins manage through the console or dedicated commands. Applying a manifest updates the Project's `displayName` and adds any new environments, and leaves those other fields untouched. Apply never removes an environment, because dropping one deletes its namespace and everything in it. That stays an explicit, confirmed action: `kip project remove-env <project> <env>` for one, or `kip project delete <project>` for the whole project.

## Applying a manifest

```bash
kip apply -f kipper.yaml
```

```
  ✔  Project blog created (environments: [test acc prod])
  ✔  Namespace blog-test created

  Applying to blog (blog-test)...
    ✔  App/frontend created
    ✔  App/api created
    ✔  Service/db created
    ✔  Volume/uploads created
    ✔  Function/image-resize created
    ✔  Job/cleanup created

  Done: 6 created, 0 updated
```

Kipper creates the Project CR and namespaces if they don't exist, then creates or updates the corresponding Custom Resources. For apps, services, volumes, functions and jobs, the update replaces the live spec, so the manifest is the desired state and a field you leave out is removed.

If you know `kubectl`, note that this is closer to `kubectl replace` than to `kubectl apply`. `kubectl apply` merges against a record of what it last applied, so a field set some other way and never named in your YAML survives. `kip apply` writes the spec wholesale, so it does not: anything the manifest does not carry is gone, whether or not the manifest ever carried it. `terraform apply` is the nearer comparison — the config is the whole desired state and drift is reverted.

The practical consequence is that a change made with `kip app update`, `kip app env set` or the console does not survive the next apply unless the manifest knows about it. `kip export` is the way to fold live state back in rather than transcribing it.

`kip diff` names what would go, field by field:

```
  ~ App/website
      ~ image: registry.example.com/website:v1 -> registry.example.com/website:v2
      - git.credentialsSecret: website-git-credentials (will be cleared)
      - route.redirectFrom: [www.example.com] (will be cleared)
      - replicas: 4 -> 1 (the cluster's default)
```

and `kip apply` prints the same list and stops rather than clearing it:

```
  These are set on the cluster and absent from the manifest, so applying takes them away:
    - App/website  git.credentialsSecret: website-git-credentials (removed)
    - App/website  route.redirectFrom: [www.example.com] (removed)
    - App/website  replicas: 4 -> 1 (the cluster's default)

  Error: refusing to clear 3 field(s) the manifest does not carry; add them to the
  manifest, run 'kip export' to fold the live state in, or re-run with --force
```

A field the CRD gives a default is the second kind. Leaving `replicas` out of a
manifest does not remove it, because the cluster writes its default back, but the
four replicas you scaled to are gone just the same and nothing you wrote asked for
one. Where the live value already is the default there is nothing to lose and
nothing is said.

If the CLI cannot read the cluster's resource schemas it says so under the list,
because without them a value the cluster fills in for itself cannot be told from
one the manifest removes, and both get listed. A project-scoped role does not have
that access today, so an operator with one is asked about fields that are not
really going anywhere. Running the same apply as a cluster admin shows the shorter,
accurate list.

Pass `--force` when clearing is what you meant. A git app's built image is never reported, because apply preserves it. The Project is the exception and is merged, as described above. If you also set fields in the web console, for example API-key gating on a route, include them in the manifest or the next apply clears them. Run `kip export` to capture the live state into a manifest that round-trips.

### Overriding project and environment

```bash
kip apply -f kipper.yaml --project different-project --environment prod
```

### Dry run

Preview what would change without applying:

```bash
kip apply -f kipper.yaml --dry-run
```

## Exporting from a live cluster

Generate a `kipper.yaml` from your current cluster state:

```bash
kip export --project blog --environment test
```

This outputs the manifest to stdout. Save it to a file:

```bash
kip export --project blog --environment test -o kipper.yaml
```

The export includes all apps, services, volumes, jobs, functions, project metadata (display name, environments list), resource profiles, autoscale config, service bindings, and routes. Secrets are excluded by design.

Use this to bootstrap a GitOps workflow from an existing cluster, or to replicate a cluster on a new server.

### Replicating a cluster

```bash
# On the source cluster: export each environment
kip export --project blog --environment test -o test.yaml
kip export --project blog --environment acc -o acc.yaml
kip export --project blog --environment prod -o prod.yaml

# On the new cluster (after kip install)
kip apply -f test.yaml
kip apply -f acc.yaml
kip apply -f prod.yaml

# Recreate secrets (not included in the export)
kip app secret set api DATABASE_URL --project blog --environment test
```

## Comparing against the cluster

See what would change before applying:

```bash
kip diff -f kipper.yaml
```

```
  Comparing blog (blog-test)...
    + App/new-service (new)
    ~ App/api
        ~ image: nginx:1.25 -> nginx:1.27
        - route.redirectFrom: [www.example.com] (will be cleared)
        - replicas: 4 -> 1 (the cluster's default)
    ~ Service/db
        + storage: 5Gi

  2 field(s) would be lost: they are set on the cluster and absent from the
  manifest, and apply replaces a spec rather than merging into it. Add them to the
  manifest to keep them, or run 'kip export' to fold the live state back in.
```

The markers at the resource level:

- `+` means the resource will be created
- `~` means it exists and some of its fields differ

And inside a resource, one line per field:

- `+` a field the manifest carries and the cluster does not
- `~` a field both carry, with the live value and the manifest value
- `-` a field live in the cluster that the manifest does not carry. Applying either
  removes it or, where the CRD gives that field a default, puts the default back in
  place of the value you set. Both lose what is there, so both are confirmed.

A resource whose fields all match is not listed at all, so an empty comparison means
the manifest already describes the cluster.

Only ordinary configuration is printed with its value: the image, replicas, the
route's hostname, resource requests and limits, autoscaling, the schedule, and
the like. A route's path is withheld along with the rest, because an unguessable
prefix is a normal way to protect a webhook. Everything else is named and its
value withheld, because a spec carries the operator's own text — an environment
variable, a build argument, a command line, a function's source — and any of it
can hold a token. This output ends up in terminal scrollback and, from a CI job,
in durable logs. The path tells you which field is affected, which is the part
you need. A git URL is the one thing shown in part: it keeps its host and
repository and loses whatever came before the `@`.

The `-` lines are the ones to read closely. A spec is replaced rather than merged, so
a value set through `kip app update` or the console and never written into the
manifest is removed by the next apply. `kip apply` refuses to run when it finds any,
and `--force` is how you say you meant it. See [Applying a manifest](#applying-a-manifest)
above.

## File structure for larger clusters

A single `kipper.yaml` works for small setups. For larger clusters, split by project and environment:

```
kipper/
├── blog/
│   ├── test.yaml
│   ├── acc.yaml
│   └── prod.yaml
├── another-project/
│   ├── test.yaml
│   └── prod.yaml
```

Apply an entire directory:

```bash
kip apply -f kipper/
```

This reads all `.yaml` and `.yml` files recursively and applies each one. Each file is self-contained with its own `project` and `environment` fields.

Apply a single environment:

```bash
kip apply -f kipper/blog/test.yaml
```

## Git-based apps

Apps can reference a Git repository instead of a pre-built image:

```yaml
apps:
  api:
    port: 8080
    git:
      url: https://github.com/acme/api.git
      branch: main
```

When applied, the app is created with a placeholder image. Use `kip app rebuild api` or configure a webhook to trigger the first build. See [Deploying Apps: From a Git repository](/en/deploying-apps#from-a-git-repository) for details.

## Secrets

Secrets are never stored in `kipper.yaml`. Manage them separately:

```bash
kip app secret set api DATABASE_URL --project blog --environment test
```

The manifest references services via `serviceBindings`. Credentials are injected automatically at runtime.

An `env:` value may reference an injected credential by name, as `DATABASE_URL` does in the example above. The manifest carries the reference and the cluster resolves it, so the same file applied to test and to prod produces a different connection string in each without either password being written down. The [syntax and its rules](/en/secrets#referencing-another-variable) are on the secrets page.

## Using with ArgoCD or Flux

Kipper uses standard Kubernetes Custom Resources (`kipper.run/v1alpha1`). Any GitOps tool that applies Kubernetes YAML works out of the box:

**ArgoCD:**

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: blog
spec:
  source:
    repoURL: https://github.com/acme/infrastructure.git
    path: kipper/blog
  destination:
    server: https://kubernetes.default.svc
    namespace: blog-test
```

**Flux:**

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: infrastructure
spec:
  url: https://github.com/acme/infrastructure.git
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: blog
spec:
  sourceRef:
    kind: GitRepository
    name: infrastructure
  path: ./kipper/blog
```

You can also write the custom resources as raw Kubernetes YAML and apply them with `kubectl apply -f` for full control.

### Validating resources against the CRD schemas

The schemas for all Kipper resource types live in
[`deploy/crds/`](https://github.com/getkipper/kipper/tree/main/deploy/crds) as a kustomize unit,
regenerated from the controller's Go types on every commit. Check out the release tag matching your
cluster version and point schema-aware tooling at it, for example your IDE's Kubernetes plugin or
kubeconform in CI. That catches an invalid resource before your GitOps tool ever syncs it.

Applying the CRDs to the cluster stays with `kip install` and `kip upgrade`. Keep your GitOps tool
pointed at your own resources, as in the examples above, and treat `deploy/crds/` as a read-only
schema reference. Syncing the CRDs from git as well would leave two writers fighting over the same
objects.

## CI/CD integration

Run `kip apply` in your CI/CD pipeline to deploy on every merge. Since apply replaces each resource's spec, keep every field you care about in the manifest. Anything set only in the console is cleared on the next run.

```yaml
# .gitlab-ci.yml
deploy:
  script:
    - kip apply -f kipper.yaml --environment $CI_ENVIRONMENT_NAME
```

```yaml
# GitHub Actions
- name: Deploy
  run: kip apply -f kipper.yaml --environment production
```

## Blueprints

Blueprints are pre-built `kipper.yaml` templates for common application stacks. Install a complete stack with one command:

```bash
kip blueprint install wordpress --set projectName=my-blog
```

Or generate a manifest to customise before applying:

```bash
kip init --blueprint wordpress --set projectName=my-blog
```

See the [Blueprints](/en/blueprints) page for the full catalogue and usage guide.
