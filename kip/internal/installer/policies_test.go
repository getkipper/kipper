package installer

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
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
		manifests, err := AdmissionPolicyManifests(offered, nil)
		require.NoError(t, err)
		all := string(bytes.Join(manifests, nil))
		assert.Contains(t, all, fmt.Sprintf("    - name: offered\n      expression: \"%d\"\n", offered))
	}
	_, err := AdmissionPolicyManifests(3, nil)
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

const shippedServerNodes = "    - name: serverNodes\n      expression: \"[]\"\n"

func TestAdmissionPoliciesRenderTheServerNodes(t *testing.T) {
	manifests, err := AdmissionPolicyManifests(1, []string{"server-1", "cp.example.com"})
	require.NoError(t, err)
	all := string(bytes.Join(manifests, nil))
	assert.Contains(t, all, "    - name: serverNodes\n      expression: \"['server-1', 'cp.example.com']\"\n")
	assert.NotContains(t, all, shippedServerNodes)
}

func TestAdmissionPoliciesRenderNoServerNodesAsAnEmptyList(t *testing.T) {
	manifests, err := AdmissionPolicyManifests(1, nil)
	require.NoError(t, err)
	assert.Contains(t, string(bytes.Join(manifests, nil)), shippedServerNodes,
		"an empty list denies every replica delete by console-api")
}

func TestAdmissionPoliciesRefuseANodeNameThatIsNoDNSName(t *testing.T) {
	for _, name := range []string{"server'1", "Server-1", "", "a b", "x]"} {
		_, err := AdmissionPolicyManifests(1, []string{name})
		assert.Error(t, err, "%q never reaches the CEL expression", name)
	}
}

func TestInstallAdmissionPoliciesRendersTheServerNodes(t *testing.T) {
	host := &hostScript{reply: func(command string) (string, error) {
		if strings.Contains(command, "get nodes -l "+ServerNodeLabel) {
			return "node/server-1\n", nil
		}
		return "", nil
	}}
	require.NoError(t, InstallAdmissionPolicies(host))
	assert.Contains(t, strings.Join(host.commands, "\n"), "expression: \"['server-1']\"")
}

func TestInstallAdmissionPoliciesStopsWhenTheServerNodesCannotBeRead(t *testing.T) {
	host := &hostScript{reply: func(command string) (string, error) {
		if strings.Contains(command, "get nodes -l "+ServerNodeLabel) {
			return "Unable to connect to the server", errors.New("exit status 1")
		}
		return "", nil
	}}
	err := InstallAdmissionPolicies(host)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server nodes")
	assert.NotContains(t, strings.Join(host.commands, "\n"), "kubectl apply", "nothing is applied with a guessed list")
}

// The policy matches console-api by username, which must be the ServiceAccount
// console-api's Deployment runs as. A mismatch would never match and so never
// protect anything.
func TestServerReplicaPolicyMatchesConsoleAPIsServiceAccount(t *testing.T) {
	deployment := consoleManifest(t)
	account := regexp.MustCompile(`(?m)^\s+serviceAccountName: (\S+)$`).FindStringSubmatch(deployment)
	namespace := regexp.MustCompile(`(?m)^  namespace: (\S+)$`).FindStringSubmatch(deployment)
	require.Len(t, account, 2, "console-api's Deployment names its ServiceAccount")
	require.Len(t, namespace, 2, "console-api's Deployment names its namespace")
	policy, err := admissionPolicies.ReadFile("policies/kipper-longhorn-server-replicas.yaml")
	require.NoError(t, err)
	assert.Contains(t, string(policy), fmt.Sprintf("request.userInfo.username == 'system:serviceaccount:%s:%s'", namespace[1], account[1]))
}

func consoleManifest(t *testing.T) string {
	t.Helper()
	manifest := renderConsoleManifest("dex.example.com", "console.example.com", "api.example.com", "example.com", "kipper.example.com", "example.com")
	start := strings.Index(manifest, "kind: Deployment\nmetadata:\n  name: console-api\n")
	require.GreaterOrEqual(t, start, 0, "the console manifest carries console-api's Deployment")
	return manifest[start:]
}
