# Contributing

Kipper is open source under the Apache 2.0 license. Contributions are welcome.

## Development setup

### Prerequisites

- Go 1.25+
- Node.js 20+
- `golangci-lint` and `goimports` for linting and formatting
- A Linux host or VM to run the whole platform (k3s is Linux-only). A local VM works fine, see [Running Kipper locally](#running-kipper-locally).

### Clone and build

```bash
git clone https://github.com/getkipper/kipper
cd kipper

# Build the CLI
cd kip && go build -o kip . && cd ..

# Run the console in dev mode
cd console && npm install && npm run dev && cd ..

# Run the docs site
cd docs && npm install && npm run dev && cd ..
```

### Running tests

```bash
# Go tests (kip, console-api, gateway)
cd kip && go test ./...
cd console-api && go test ./...
cd gateway && go test ./...

# Console unit tests and type checking
cd console && npm run test && npm run type-check
```

Format Go imports with the project's local prefix before committing, so they group correctly:

```bash
goimports -w -local github.com/getkipper/kipper ./...
```

## Running Kipper locally

The unit tests above need no cluster and no login, so that is the fastest loop for day-to-day work. To see the whole platform running, you need a real cluster, because the console API and the controllers use the in-cluster Kubernetes config. Running a cluster is also how you dogfood Kipper: you use `kip` to manage it.

### Full platform on a Linux VM

k3s runs on Linux and `kip install` bootstraps it over SSH, so on macOS or Windows run a local Ubuntu VM. OrbStack or Lima on Apple Silicon, or multipass or UTM on any machine, all work well. Give it a few cores, 8 GB of RAM, and 40 GB of disk.

```bash
# Example with multipass
multipass launch 24.04 --name kipper --cpus 4 --memory 8G --disk 40G

# Build the CLI and install into the VM over SSH
cd kip && go build -o kip . && cd ..
./kip/kip install --host 203.0.113.10
```

`kip install` sets up k3s, Traefik, cert-manager, Longhorn, Dex, and the console, then prints the admin login. See [Installation](/en/installation) for the full flag reference.

Sign in to the console with those admin credentials. Authentication is a real Dex login, the same as production. There is no local bypass, so a running cluster with Dex is what you authenticate against.

### Console-only iteration

For quick frontend work, run the Vite dev server on its own:

```bash
cd console && npm install && npm run dev   # http://localhost:5173
```

It proxies `/api` to a console API on `http://localhost:8080` by default. To work against a running cluster, forward that cluster's console API to port 8080, or set `VITE_API_URL` to the cluster's console API URL. Live log and terminal streaming check the browser Origin, so streaming from a `localhost` origin needs that origin allowed on the target cluster. The REST screens work without it.

## Project structure

| Directory | Language | Purpose |
|---|---|---|
| `kip/` | Go | CLI tool |
| `console/` | Vue 3 + TypeScript | Web dashboard |
| `console-api/` | Go | REST API for the console |
| `gateway/` | Go | kipper.run subdomain proxy |
| `docs/` | VitePress | This documentation |

## CRD architecture

Kipper manages workloads through Custom Resource Definitions (`kipper.run/v1alpha1`). When a user deploys an app, creates a service, or adds a function, the handler creates a Custom Resource. A controller-runtime reconciler watches these CRs and ensures the underlying Kubernetes resources (Deployments, StatefulSets, Services, Ingresses) match the desired spec.

| CRD | Reconciles into |
|---|---|
| `App` | Deployment + Service + Ingress + Secrets |
| `Service` | StatefulSet + headless Service + credentials Secret |
| `Function` | Deployment + Service + KEDA HTTPScaledObject + Ingress |
| `Project` | Namespaces + shared storage PVCs |
| `Job` | CronJob or Job |
| `Volume` | PersistentVolumeClaim |

**Key directories:**
- `console-api/api/v1alpha1/`: CRD type definitions
- `console-api/controllers/`: reconcilers (one per CRD)
- `console-api/handlers/`: REST API handlers that create CRs
- `deploy/crds/`: generated CRD YAML manifests

When adding a new feature, decide whether it needs a new CRD (owns Kubernetes resources) or can be added as a field on an existing one. Use `controller-gen` to regenerate manifests after changing types.

## Code conventions

### Go

- Table-driven tests with `testify`
- Error wrapping with context: `fmt.Errorf("installing k3s: %w", err)`
- Unexported by default, only export what is used outside the package
- Interfaces belong in the package that uses them
- All exported types and functions need godoc comments

### Vue / TypeScript

- Composition API with `<script setup lang="ts">`, never Options API
- No `any`, define proper types
- Pinia stores with loading and error state
- Tailwind utility classes only, no inline styles
- `lucide-vue-next` for icons

### Commits

Format: `type: short description`

Types: `feat:`, `fix:`, `docs:`, `style:`, `refactor:`, `test:`, `chore:`, `perf:`

### Branches

Format: `prefix/short-description`

Prefixes: `feature/`, `bugfix/`, `hotfix/`, `refactor/`, `docs/`, `test/`

## PR guidelines

- New features require unit tests covering the happy path and at least two error cases
- Bug fixes require a test that would have caught the bug
- Documentation changes are required in the same PR as feature changes
- Every example must use realistic names, not `foo` or `bar`

## Licensing

All contributions must be compatible with the Apache 2.0 license. No GPL dependencies. MIT and Apache 2.0 only.
