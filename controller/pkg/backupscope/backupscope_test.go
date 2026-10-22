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
		"monitoring", "keda", "longhorn-system", "traefik", "velero", DrillNamespace,
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

func TestCoversFollowsVeleroNamespacePatterns(t *testing.T) {
	for _, tc := range []struct {
		name               string
		included, excluded []string
		covers, mayCover   bool
	}{
		{"no lists", nil, nil, true, true},
		{"star", []string{"*"}, nil, true, true},
		{"name", []string{"shop"}, nil, true, true},
		{"other name", []string{"blog"}, nil, false, false},
		{"prefix glob", []string{"sh*"}, nil, true, true},
		{"single character", []string{"sho?"}, nil, true, true},
		{"bracket range is uncertain", []string{"[r-t]hop"}, nil, false, true},
		{"negated set is uncertain", []string{"[!d]*"}, nil, false, true},
		{"alternatives are uncertain", []string{"{shop,blog}"}, nil, false, true},
		{"uncertain exclusion", []string{"*"}, []string{"[!s]*"}, false, true},
		{"glob for others", []string{"blog-*"}, nil, false, false},
		{"excluded name", nil, []string{"shop"}, false, false},
		{"excluded star", nil, []string{"*"}, false, false},
		{"excluded glob wins", []string{"*"}, []string{"sh*"}, false, false},
		{"excluded glob for others", []string{"*"}, []string{"blog-*"}, true, true},
		{"unparsed inclusion", []string{"[shop"}, nil, false, true},
		{"unparsed exclusion", nil, []string{"[shop"}, false, true},
		{"escape", []string{`sh\op`}, nil, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.covers, Covers(tc.included, tc.excluded, "shop"), "Covers")
			assert.Equal(t, tc.mayCover, MayCover(tc.included, tc.excluded, "shop"), "MayCover")
		})
	}
}

func TestUnfinishedCountsEveryPhaseThatIsNotAnEnd(t *testing.T) {
	for _, phase := range []string{"", "New", "InProgress", "WaitingForPluginOperations", "Finalizing", "Queued", "ReadyToStart"} {
		assert.True(t, Unfinished(phase), phase)
	}
	for _, phase := range []string{"Completed", "PartiallyFailed", "Failed", "FailedValidation", "Deleting"} {
		assert.False(t, Unfinished(phase), phase)
	}
}
