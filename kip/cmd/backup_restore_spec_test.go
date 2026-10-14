package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRestoreSpecMapsTheNamespaceTheWayVeleroReadsIt(t *testing.T) {
	spec, err := restoreSpec("nightly", "shop", "", "shop:shop-restored")
	require.NoError(t, err)

	assert.Equal(t, map[string]interface{}{"shop": "shop-restored"}, spec["namespaceMapping"])
	assert.NotContains(t, spec, "namespaceMappings")
	assert.Equal(t, []interface{}{"shop"}, spec["includedNamespaces"])
}

func TestRestoreSpecRefusesAMappingItCannotRead(t *testing.T) {
	for _, mapping := range []string{"shop", "shop:", ":shop-restored", "shop:shop", "shop:copy:extra", "shop: ", " :copy", " shop:copy", "shop:Copy", "shop:copy_1"} {
		_, err := restoreSpec("nightly", "shop", "", mapping)
		assert.Error(t, err, mapping)
	}
}

func TestRestoreSpecWithoutAMapping(t *testing.T) {
	spec, err := restoreSpec("nightly", "", "deployments,secrets", "")
	require.NoError(t, err)

	assert.NotContains(t, spec, "namespaceMapping")
	assert.NotContains(t, spec, "includedNamespaces")
	assert.Equal(t, []string{"deployments", "secrets"}, spec["includedResources"])
	assert.Equal(t, "nightly", spec["backupName"])
}

func TestRestoreSpecLimitsAMappedRestoreToItsSource(t *testing.T) {
	spec, err := restoreSpec("nightly", "", "", "shop:shop-restored")
	require.NoError(t, err)
	assert.Equal(t, []interface{}{"shop"}, spec["includedNamespaces"])

	_, err = restoreSpec("nightly", "billing", "", "shop:shop-restored")
	require.Error(t, err, "--namespace must match the mapping's source namespace")
}
