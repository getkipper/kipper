# deploy/

This directory is a drift-gated reference, kept byte-for-byte in sync with what
`kip install` ships. `kip install` is the only supported way to install Kipper.
It builds every manifest internally, so applying this directory does not
produce a working platform.

## crds/

Canonical `controller-gen` output for the Kipper CRDs, plus a generated
`kustomization.yaml`. CI regenerates all of it from the Go types on every push.

Use it as a versioned schema reference: pin the release tag matching your
cluster and validate your git-managed Kipper resources with kubeconform, CI,
or your IDE. Applying the CRDs is owned by `kip install` and `kip upgrade`.
Syncing them from git as well would give the cluster two writers fighting over
the same fields, so point your GitOps tooling at your own `kipper.yaml`
manifests (see the GitOps docs) and leave CRD management to kip.

## authz.yaml

Reference copy of the authz service manifests, gated by
`TestAuthzManifestMatchesDeploy` against the installer's embedded copy. It
assumes an existing Kipper cluster: the `kipper-system` namespace, service
accounts, and secrets that `kip install` creates. On its own it deploys none
of those.

## console / console-api

These have no static manifest. Both are rendered per cluster by `kip install`
with cluster-specific values (domain, OIDC issuer, registry, secrets).

Everything here needs a drift gate proving it matches the installer. CI fails
on any other entry in this directory.
