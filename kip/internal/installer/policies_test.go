package installer

import (
	"bytes"
	"errors"
	"fmt"
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

func TestAdmissionPoliciesRenderTheCopiesOffer(t *testing.T) {
	for _, offered := range []int32{1, 2} {
		manifests, err := AdmissionPolicyManifests(offered)
		require.NoError(t, err)
		all := string(bytes.Join(manifests, nil))
		assert.Contains(t, all, fmt.Sprintf("    - name: offered\n      expression: \"%d\"\n", offered))
	}
	_, err := AdmissionPolicyManifests(3)
	assert.Error(t, err, "only 1 or 2 is ever rendered into the policy")
}

func TestInstallAdmissionPoliciesKeepsTheAdminsOffer(t *testing.T) {
	host := &hostScript{reply: func(command string) (string, error) {
		if strings.Contains(command, "{.spec.maxCopies}") {
			return "offer=2", nil
		}
		return "", nil
	}}
	require.NoError(t, InstallAdmissionPolicies(host))
	assert.Contains(t, strings.Join(host.commands, "\n"), "    - name: offered\n      expression: \"2\"\n")

	host = &hostScript{}
	require.NoError(t, InstallAdmissionPolicies(host))
	assert.Contains(t, strings.Join(host.commands, "\n"), "    - name: offered\n      expression: \"1\"\n",
		"a cluster without a recorded offer offers one copy")
}

func offerHost(out string, err error) *hostScript {
	return &hostScript{reply: func(command string) (string, error) {
		if strings.Contains(command, "{.spec.maxCopies}") {
			return out, err
		}
		return "", nil
	}}
}

func TestInstallAdmissionPoliciesReadsTheOfferPastWarnings(t *testing.T) {
	host := offerHost("Warning: example.com/v1 Thing is deprecated\noffer=2", nil)
	require.NoError(t, InstallAdmissionPolicies(host))
	assert.Contains(t, strings.Join(host.commands, "\n"), "    - name: offered\n      expression: \"2\"\n",
		"kubectl's warnings share the output with the value")
}

func TestInstallAdmissionPoliciesStopsOnAnUnreadableOffer(t *testing.T) {
	for name, host := range map[string]*hostScript{
		"unparsable output": offerHost("Warning: something\noffer=two\n", nil),
		"failed read":       offerHost("Unable to connect to the server", errors.New("exit status 1")),
	} {
		err := InstallAdmissionPolicies(host)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "copies offer", name)
		assert.NotContains(t, strings.Join(host.commands, "\n"), "kubectl apply", "%s: nothing is applied with a guessed offer", name)
	}
}

func TestInstallAdmissionPoliciesOffersOneWhenNoOfferIsRecorded(t *testing.T) {
	for name, out := range map[string]string{
		"no PlatformConfig":           "",
		"no PlatformConfig, warned":   "Warning: example.com/v1 Thing is deprecated\n",
		"unset field":                 "offer=",
		"unset field after a warning": "Warning: example.com/v1 Thing is deprecated\noffer=",
	} {
		host := offerHost(out, nil)
		require.NoError(t, InstallAdmissionPolicies(host), name)
		assert.Contains(t, strings.Join(host.commands, "\n"), "    - name: offered\n      expression: \"1\"\n", name)
	}
}

func TestInstallAdmissionPoliciesOffersOneBeforeThePlatformConfigKindIsServed(t *testing.T) {
	host := offerHost(`error: the server doesn't have a resource type "platformconfig"`, errors.New("exit status 1"))
	require.NoError(t, InstallAdmissionPolicies(host))
	assert.Contains(t, strings.Join(host.commands, "\n"), "    - name: offered\n      expression: \"1\"\n",
		"a kind that is not served yet holds no recorded offer")
}
