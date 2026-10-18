package installer

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Run errors include captured output, so config read failures need to protect tokens.
func TestConfigReadsKeepTokensOutOfErrors(t *testing.T) {
	const token = "K10example-token::server:example"
	failing := &routedRunner{routes: []route{{contains: "", out: "token: " + token, err: errors.New("connection lost: token: " + token)}}}

	_, err := readK3sConfig(failing)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), token)

	_, err = clusterNetworks(failing)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), token)

	_, err = WorkerJoin{}.Register(failing, failing, "203.0.113.10", HostIdentity{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), token)
}

func TestTheUnitReadKeepsTokensOutOfErrors(t *testing.T) {
	const token = "K10example-token::server:example"
	server := &routedRunner{routes: []route{
		{contains: "k3s/config.yaml", out: ""},
		{contains: "", out: "--token " + token, err: errors.New("connection lost: --token " + token)},
	}}

	_, err := clusterNetworks(server)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), token)
}
