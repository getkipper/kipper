package installer

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

// The installer embeds its own copy of each policy in deploy/policies, which
// the console-api API server tests exercise.
func TestEmbeddedPoliciesMatchDeploy(t *testing.T) {
	entries, err := admissionPolicies.ReadDir("policies")
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	for _, e := range entries {
		embedded, err := admissionPolicies.ReadFile("policies/" + e.Name())
		require.NoError(t, err)
		deployed, err := os.ReadFile("../../../deploy/policies/" + e.Name())
		require.NoError(t, err, "every embedded policy has its source in deploy/policies")
		assert.Equal(t, string(deployed), string(embedded), "%s differs from deploy/policies; copy it again", e.Name())
	}
}

func TestSharedVolumesPolicyMessageMatchesTheRefusal(t *testing.T) {
	data, err := admissionPolicies.ReadFile("policies/kipper-shared-volumes.yaml")
	require.NoError(t, err)
	assert.Contains(t, string(data), `message: "`+platform.SharedVolumesUnsupported+`"`)
}

func TestInstallAdmissionPoliciesAppliesEachPolicy(t *testing.T) {
	host := &hostScript{}
	require.NoError(t, InstallAdmissionPolicies(host))
	all := strings.Join(host.commands, "\n")
	assert.Contains(t, all, "kubectl apply -f -")
	assert.Contains(t, all, "kind: ValidatingAdmissionPolicyBinding")
	assert.Contains(t, all, "name: kipper-shared-volumes")
}
