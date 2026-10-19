package doctor

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// routedRunner answers each command with the output of the first route whose
// key it contains, and records every command it was asked to run.
type routedRunner struct {
	routes   [][3]any // Each route holds a command substring, output and error.
	commands []string
}

func (r *routedRunner) on(contains, out string, err error) *routedRunner {
	r.routes = append(r.routes, [3]any{contains, out, err})
	return r
}

func (r *routedRunner) Run(command string) (string, error) {
	r.commands = append(r.commands, command)
	for _, rt := range r.routes {
		if strings.Contains(command, rt[0].(string)) {
			err, _ := rt[2].(error)
			return rt[1].(string), err
		}
	}
	return "", nil
}

const nodesJSON = `{"items":[{"metadata":{"name":"server-1"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`

const deploymentsJSON = `{"items":[
 {"metadata":{"namespace":"traefik","name":"traefik"},"spec":{"replicas":1},"status":{"availableReplicas":1}},
 {"metadata":{"namespace":"kipper-system","name":"console-api"},"spec":{"replicas":1},"status":{"availableReplicas":0}},
 {"metadata":{"namespace":"default","name":"shop"},"spec":{"replicas":1},"status":{"availableReplicas":1}}]}`

const tlsStoresJSON = `{"items":[{"metadata":{"namespace":"kipper-system","name":"default"},"spec":{"defaultCertificate":{"secretName":"kipper-hop-cert"}}}]}`

func healthyRunner() *routedRunner {
	return (&routedRunner{}).
		on(`-n traefik get secret kipper-hop-cert`, "-----BEGIN CERTIFICATE-----\nCOPY\n-----END CERTIFICATE-----", nil).
		on("get nodes", nodesJSON, nil).
		on("get deployments", deploymentsJSON, nil).
		on(`{.data.tls\.crt}`, "-----BEGIN CERTIFICATE-----\nCURRENT\n-----END CERTIFICATE-----", nil).
		on(`{.data.candidate\.crt}`, "", nil).
		on("tlsstores", tlsStoresJSON, nil).
		on("clusteridentities", "203.0.113.10", nil).
		on("platformconfigs", "small", nil).
		on("logs deploy/console-api", "2026-10-06T00:07:40Z serving on :8080\n2026-10-06T00:07:48Z gateway heartbeat: renewed lab.kipper.run (hop pin active)\n", nil)
}

func TestFetchOverSSH_ReadsEveryPart(t *testing.T) {
	snap := FetchOverSSH(healthyRunner())

	require.Empty(t, snap.Missing)
	require.Len(t, snap.Nodes, 1)
	assert.Equal(t, "server-1", snap.Nodes[0].Name)
	require.Contains(t, snap.Workloads, "traefik/traefik")
	assert.Equal(t, int32(1), snap.Workloads["traefik/traefik"].Status.AvailableReplicas)
	assert.Contains(t, snap.Workloads, "kipper-system/console-api")
	assert.NotContains(t, snap.Workloads, "default/shop", "only core workloads are kept")
	assert.Contains(t, snap.HopCert, "CURRENT")
	assert.Empty(t, snap.HopCandidate)
	assert.Contains(t, snap.TraefikCopyCert, "COPY")
	assert.Equal(t, []TLSStore{{Namespace: "kipper-system", Name: "default", DefaultCert: "kipper-hop-cert"}}, snap.TLSStores)
	assert.Equal(t, "203.0.113.10", snap.ClusterHost)
	assert.Equal(t, "small", snap.Profile)
	assert.Equal(t, []string{"2026-10-06T00:07:48Z gateway heartbeat: renewed lab.kipper.run (hop pin active)"}, snap.Heartbeat)
}

func TestFetchOverSSH_RecordsAFailedPartAndReadsTheRest(t *testing.T) {
	runner := healthyRunner()
	runner.routes = append([][3]any{{"get nodes", "", errors.New("The connection to the server 127.0.0.1:6443 was refused")}}, runner.routes...)

	snap := FetchOverSSH(runner)

	require.Contains(t, snap.Missing, PartNodes)
	assert.Contains(t, snap.Missing[PartNodes].Error(), "was refused")
	assert.Contains(t, snap.HopCert, "CURRENT", "a failed part must not stop the others")
	assert.Equal(t, "small", snap.Profile)
}

func TestFetchOverSSH_NeverReadsAPrivateKey(t *testing.T) {
	runner := healthyRunner()
	FetchOverSSH(runner)

	require.NotEmpty(t, runner.commands)
	for _, cmd := range runner.commands {
		assert.NotContains(t, cmd, "tls.key")
		assert.NotContains(t, cmd, `tls\.key`)
		if strings.Contains(cmd, "secret") {
			assert.Contains(t, cmd, "jsonpath", "a Secret is only ever read one public key at a time: %s", cmd)
		}
	}
}

func TestFetchOverSSH_ReportsAFailedHeartbeatRead(t *testing.T) {
	runner := healthyRunner()
	runner.routes = append([][3]any{{"logs deploy/console-api", "error: the server could not find the requested resource", errors.New("exit status 1")}}, runner.routes...)

	snap := FetchOverSSH(runner)

	require.Contains(t, snap.Missing, PartHeartbeat)
	assert.Empty(t, snap.Heartbeat, "kubectl's error text is not a heartbeat line")
}

func TestFetchOverSSH_BoundsEveryKubectlCall(t *testing.T) {
	runner := healthyRunner()
	FetchOverSSH(runner)

	for _, cmd := range runner.commands {
		if strings.Contains(cmd, "kubectl") {
			assert.Contains(t, cmd, "--request-timeout=", cmd)
		}
	}
}

func TestFetchOverSSH_IgnoresKubectlWarnings(t *testing.T) {
	runner := healthyRunner()
	runner.routes = append([][3]any{
		{"get nodes", "Warning: v1 ComponentStatus is deprecated\n" + nodesJSON, nil},
		{"platformconfigs", "Warning: something\nsmall", nil},
	}, runner.routes...)

	snap := FetchOverSSH(runner)

	require.Empty(t, snap.Missing)
	assert.Len(t, snap.Nodes, 1)
	assert.Equal(t, "small", snap.Profile)
}
