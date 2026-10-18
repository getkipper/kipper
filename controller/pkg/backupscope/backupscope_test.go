package backupscope

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBackupUsesOnlyTheScopedFiltersVeleroAllowsTogether(t *testing.T) {
	spec := map[string]interface{}{}
	Backup(spec)

	// Velero rejects a backup that mixes these with the scoped filters.
	assert.NotContains(t, spec, "excludedResources")
	assert.NotContains(t, spec, "includedResources")
	assert.NotContains(t, spec, "includeClusterResources")

	assert.Equal(t, []interface{}{
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}, spec["excludedNamespaceScopedResources"])
	assert.Equal(t, []interface{}{
		"projects.kipper.run",
		"resourceadjustments.kipper.run",
		"platformconfigs.kipper.run",
		"clusteridentities.kipper.run",
	}, spec["includedClusterScopedResources"])
}

func TestBackupOfTheWholeClusterTakesEveryClusterScopedKind(t *testing.T) {
	spec := map[string]interface{}{}
	BackupWholeCluster(spec)

	assert.Equal(t, []interface{}{"*"}, spec["includedClusterScopedResources"])
	assert.Contains(t, spec["excludedNamespaceScopedResources"], "orders.acme.cert-manager.io")
	assert.NotContains(t, spec, "excludedResources")
}

func TestRestoreLeavesKipperClusterObjectsOutByDefault(t *testing.T) {
	spec := map[string]interface{}{}
	Restore(spec, false)

	// Automatic selection allows Velero to restore PVs related to restored PVCs.
	assert.NotContains(t, spec, "includeClusterResources")
	assert.Equal(t, []interface{}{
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
		"projects.kipper.run",
		"resourceadjustments.kipper.run",
		"platformconfigs.kipper.run",
		"clusteridentities.kipper.run",
	}, spec["excludedResources"])
}

func TestRestoreWithClusterResourcesBringsBackProjectsOnly(t *testing.T) {
	spec := map[string]interface{}{}
	Restore(spec, true)

	assert.Equal(t, true, spec["includeClusterResources"])
	excluded := spec["excludedResources"]
	assert.NotContains(t, excluded, "projects.kipper.run")
	assert.NotContains(t, excluded, "resourceadjustments.kipper.run")
	// A live cluster already has its own identity, and Velero skips existing objects.
	assert.Contains(t, excluded, "platformconfigs.kipper.run")
	assert.Contains(t, excluded, "clusteridentities.kipper.run")
	assert.Contains(t, excluded, "orders.acme.cert-manager.io")
}

func TestExcludedNamespacesKeepVeleroOut(t *testing.T) {
	// Excluding velero prevents backups from reading the MinIO PVC
	// that stores their own output, which can leave them stuck in progress.
	assert.Equal(t, []string{
		"kube-system", "kube-public", "kube-node-lease",
		"monitoring", "keda", "longhorn-system", "traefik", "velero",
	}, ExcludedNamespaces())
}

func TestTransientResourcesKeepCertificatesAndTheirSecrets(t *testing.T) {
	// A restore needs the Certificate and its Secret. Restoring stale issuance
	// objects can interfere with renewal.
	assert.NotContains(t, TransientResources(), "certificates.cert-manager.io")
	assert.NotContains(t, TransientResources(), "secrets")
}

func TestListsAreFreshOnEveryCall(t *testing.T) {
	a := ExcludedNamespaces()
	a[0] = "changed"
	assert.NotEqual(t, "changed", ExcludedNamespaces()[0])

	b := BackedUpClusterResources()
	b[0] = "changed"
	assert.NotEqual(t, "changed", BackedUpClusterResources()[0])
}
