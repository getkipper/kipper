package cmd

import (
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/auth"
	"github.com/getkipper/kipper/kip/internal/config"
)

// stageTwoSessions writes a config holding two clusters with a live session
// each, and points current_cluster at the one the tests do NOT ask for. Every
// test here turns on the plugin serving the cluster it was asked for rather
// than the one kip happens to be pointed at.
func stageTwoSessions(t *testing.T) {
	t.Helper()
	withFakeHome(t)

	cfg := &config.Config{
		CurrentCluster: "warehouse",
		Clusters: []config.Cluster{
			{Name: "warehouse", Host: "203.0.113.10", Domain: "warehouse.example"},
			{Name: "shop", Host: "203.0.113.11", Domain: "shop.example"},
		},
	}
	require.NoError(t, config.Save(cfg))

	store := &auth.Store{Clusters: map[string]*auth.Credentials{
		"warehouse.example": {IDToken: "warehouse-token", Email: "anna@warehouse.example", ExpiresAt: time.Now().Add(time.Hour)},
		"shop.example":      {IDToken: "shop-token", Email: "anna@shop.example", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	require.NoError(t, store.Save())

	t.Cleanup(func() { clusterDomainFlag = "" })
}

// emitToken runs the credential plugin the way kubectl does and returns what
// reached stdout, which is the whole wire protocol.
func emitToken(t *testing.T) (string, error) {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() {
		runErr = runAuthKubectlToken(authKubectlTokenCmd, nil)
	})
	return out, runErr
}

func TestKubectlTokenServesThePinnedClusterNotTheCurrentOne(t *testing.T) {
	stageTwoSessions(t)
	clusterDomainFlag = "shop.example"

	out, err := emitToken(t)
	require.NoError(t, err)

	var creds execCredential
	require.NoError(t, json.Unmarshal([]byte(out), &creds))
	assert.Equal(t, "shop-token", creds.Status.Token,
		"the kubeconfig asked for shop.example; current_cluster is warehouse and must not decide")
	assert.Equal(t, "client.authentication.k8s.io/v1", creds.APIVersion)
	assert.Equal(t, "ExecCredential", creds.Kind)
}

func TestKubectlTokenRefusesAnUnpinnedKubeconfig(t *testing.T) {
	stageTwoSessions(t)
	clusterDomainFlag = ""

	out, err := emitToken(t)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip auth kubeconfig",
		"the refusal has to carry the fix; kubectl shows the operator nothing else")
	assert.Empty(t, out, "no credential may reach kubectl when the cluster is unknown")
}

func TestKubectlTokenRefusesADomainTheConfigDoesNotHave(t *testing.T) {
	stageTwoSessions(t)
	clusterDomainFlag = "someone-elses.example"

	out, err := emitToken(t)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "someone-elses.example")
	assert.Empty(t, out)
}

func TestClusterForKubectlTokenResolvesByDomainNotName(t *testing.T) {
	stageTwoSessions(t)

	cluster, err := clusterForKubectlToken("shop.example")
	require.NoError(t, err)
	assert.Equal(t, "shop", cluster.Name)

	_, err = clusterForKubectlToken("shop")
	assert.Error(t, err, "the name is a local alias; only the domain identifies the session")
}

// The command tree itself, so the test fails if the flag stops being
// registered or stops reaching the handler. Everything above this calls the
// handler directly and would keep passing with the wiring cut.
func TestKubectlTokenCommandTakesThePinFromItsArguments(t *testing.T) {
	stageTwoSessions(t)

	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{"auth", "kubectl-token", "--cluster-domain", "shop.example"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })

	var runErr error
	out := captureStdout(t, func() { runErr = rootCmd.Execute() })
	require.NoError(t, runErr)

	var creds execCredential
	require.NoError(t, json.Unmarshal([]byte(out), &creds))
	assert.Equal(t, "shop-token", creds.Status.Token)
}
