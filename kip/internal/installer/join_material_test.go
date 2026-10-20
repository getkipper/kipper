package installer

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func joinMaterialServer() *storageHostRunner {
	return &storageHostRunner{replies: map[string]string{ //nolint:gosec // G101: synthetic registry fixtures
		"cat " + k3sResolvConfPath: "nameserver 9.9.9.9\nnameserver 1.1.1.1\n",
		k3sVersionProbeCmd:         "k3s version " + k3sVersion + " (abc)\n",
		"get svc zot":              "10.43.0.10\n",
		zotPullSecret:              "cHVsbA==",
		zotTLSSecret:               "Y2E=",
	}}
}

func TestReadJoinMaterialReadsWhatAWorkerNeedsFromTheServer(t *testing.T) {
	m, err := ReadJoinMaterial(joinMaterialServer())

	require.NoError(t, err)
	assert.Equal(t, JoinMaterial{K3sVersion: k3sVersion, ZotIP: "10.43.0.10", RegistryCA: "ca", PullPassword: "pull", Resolvers: []string{"9.9.9.9", "1.1.1.1"}}, m)
}

func TestReadJoinMaterialFallsBackToDefaultResolvers(t *testing.T) {
	server := joinMaterialServer()
	server.replies["cat "+k3sResolvConfPath] = "nameserver resolver.example\n"

	m, err := ReadJoinMaterial(server)

	require.NoError(t, err)
	assert.Equal(t, DefaultDNSResolvers(), m.Resolvers)
}

func TestReadJoinMaterialRefusesAServerWithoutK3s(t *testing.T) {
	server := joinMaterialServer()
	server.replies[k3sVersionProbeCmd] = "KIP_K3S_ABSENT\n"

	_, err := ReadJoinMaterial(server)

	assert.ErrorContains(t, err, "k3s is not installed")
}

// TestPrepareWorkerFromWritesTheMaterialOnTheWorker checks that saved join
// material lets preparation run on the worker with no server connection.
func TestPrepareWorkerFromWritesTheMaterialOnTheWorker(t *testing.T) {
	worker := &storageHostRunner{replies: map[string]string{k3sVersionProbeCmd: "KIP_K3S_ABSENT\n"}}
	m := JoinMaterial{K3sVersion: k3sVersion, ZotIP: "10.43.0.10", RegistryCA: "registry-ca", PullPassword: "pullsecret", Resolvers: []string{"9.9.9.9"}}

	join, err := PrepareWorkerFrom(worker, m, "node-ip: 192.0.2.11\n")

	require.NoError(t, err)
	assert.Equal(t, WorkerJoin{serverVersion: k3sVersion, zotIP: "10.43.0.10", pullPassword: "pullsecret"}, join)
	written := strings.Join(append(worker.stdin, worker.commands...), "\n")
	for _, want := range []string{"registry-ca", "pullsecret", "10.43.0.10", "node-ip: 192.0.2.11", "nameserver 9.9.9.9"} {
		assert.Contains(t, written, want)
	}
}

func TestPrepareWorkerFromRefusesMaterialItCannotUseSafely(t *testing.T) {
	good := JoinMaterial{K3sVersion: k3sVersion, ZotIP: "10.43.0.10", RegistryCA: "ca", PullPassword: "pull", Resolvers: []string{"1.1.1.1"}}
	for name, change := range map[string]func(*JoinMaterial){
		"version":       func(m *JoinMaterial) { m.K3sVersion = "v1.36.2+k3s1'; reboot; '" },
		"registry ip":   func(m *JoinMaterial) { m.ZotIP = "10.43.0.10/v2; reboot" },
		"ipv6 registry": func(m *JoinMaterial) { m.ZotIP = "fd00::10" },
		"resolver":      func(m *JoinMaterial) { m.Resolvers = []string{"1.1.1.1; reboot"} },
		"no ca":         func(m *JoinMaterial) { m.RegistryCA = "" },
		"no password":   func(m *JoinMaterial) { m.PullPassword = "" },
	} {
		m := good
		change(&m)
		worker := &storageHostRunner{}

		_, err := PrepareWorkerFrom(worker, m, "")

		assert.Error(t, err, name)
		assert.Empty(t, worker.commands, "%s: nothing runs on the worker before the material is checked", name)
	}
}

func TestStartAgentRunsTheInstallerWithTheTokenOnStdin(t *testing.T) {
	worker := &storageHostRunner{}

	err := WorkerJoin{serverVersion: k3sVersion}.StartAgent(worker, "203.0.113.10", testJoinToken)

	require.NoError(t, err)
	assert.Contains(t, worker.stdin, testJoinToken+"\n")
	for _, c := range worker.commands {
		assert.NotContains(t, c, testJoinToken, "the token stays out of the command line")
	}
	taint, install := -1, -1
	for i, c := range worker.commands {
		if strings.Contains(c, "cat > "+bootstrapTaintPath) {
			taint = i
		}
		if strings.Contains(c, "K3S_URL=https://203.0.113.10:6443") {
			install = i
		}
	}
	require.NotEqual(t, -1, install)
	assert.Less(t, taint, install, "the agent registers with the bootstrap taint")
}

func TestStartAgentRefusesAServerAddressThatIsNotAnIPv4Address(t *testing.T) {
	worker := &storageHostRunner{}

	err := WorkerJoin{serverVersion: k3sVersion}.StartAgent(worker, "203.0.113.10 sh -c reboot", testJoinToken)

	assert.Error(t, err)
	assert.Empty(t, worker.commands)
}

func TestStartAgentKeepsTheTokenOutOfItsErrors(t *testing.T) {
	worker := &storageHostRunner{fail: map[string]error{"get.k3s.io": errors.New("exit status 1: + K3S_TOKEN=" + testJoinToken + "\nsecret=secretsecret0000")}}

	err := WorkerJoin{serverVersion: k3sVersion}.StartAgent(worker, "203.0.113.10", testJoinToken)

	require.ErrorContains(t, err, "exit status 1")
	assert.NotContains(t, err.Error(), "secretsecret0000")
}

func TestStartAgentRefusesSomethingThatIsNotAJoinToken(t *testing.T) {
	for _, token := range []string{"", "not-a-token", testJoinToken + "\nextra"} {
		worker := &storageHostRunner{}

		err := WorkerJoin{serverVersion: k3sVersion}.StartAgent(worker, "203.0.113.10", token)

		assert.Error(t, err, "%q", token)
		assert.Empty(t, worker.commands, "%q", token)
	}
}

// TestPrepareWorkerFromRefusesAPullPasswordThatBreaksTheRegistryConfig checks
// that only the generated alphanumeric form is accepted because registries.yaml
// and the curl config receive the password unescaped.
func TestPrepareWorkerFromRefusesAPullPasswordThatBreaksTheRegistryConfig(t *testing.T) {
	for _, password := range []string{`abc"def`, `abc\def`, "abc\ndef", "abc def"} {
		m := JoinMaterial{K3sVersion: k3sVersion, ZotIP: "10.43.0.10", RegistryCA: "ca", PullPassword: password, Resolvers: []string{"1.1.1.1"}}
		worker := &storageHostRunner{}

		_, err := PrepareWorkerFrom(worker, m, "")

		require.Error(t, err, "%q", password)
		assert.NotContains(t, err.Error(), password)
		assert.Empty(t, worker.commands, "%q", password)
	}
}
