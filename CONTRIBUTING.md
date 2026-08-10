# Contributing to Kipper

Thank you for your interest in contributing to Kipper!

The full contributing guide covers development setup, coding conventions, testing requirements, and PR guidelines. See the [Contributing documentation](docs/en/contributing.md).

## Quick start

```bash
git clone https://github.com/getkipper/kipper
cd kipper

# Build the CLI
cd kip && go build -o kip . && cd ..

# Run Go tests
cd kip && go test ./... && cd ..
cd console-api && go test ./... && cd ..
cd gateway && go test ./... && cd ..

# Run the console in dev mode
cd console && npm install && npm run dev
```

## Editing CRDs

Kipper's CRDs live in `deploy/crds/` but are generated from the Go types in `console-api/api/v1alpha1/`. When you add or change a field on a CR type, regenerate the YAMLs so the K8s API server doesn't silently strip the new field on Update:

```bash
go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1
cd console-api
controller-gen crd paths=./api/... output:crd:dir=../deploy/crds
```

CI runs the same command and fails on any drift between the regenerated YAMLs and the committed ones.

## License

By contributing, you agree that your contributions will be licensed under the Apache 2.0 License.
