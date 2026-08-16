package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// stageHostTrust replaces the host-side steps with recorders and returns the
// order they ran in. Everything below drives reconcileHostTrust itself, which is
// what an upgrade calls, so moving the repair out of it fails these rather than
// passing quietly.
func stageHostTrust(t *testing.T, synced bool, changed bool) *[]string {
	t.Helper()
	var order []string

	hop, api, sync, ensure := ensureHopMaterial, ensureAPIServerConfig, syncOperatorAuth, ensureOperatorAuth
	t.Cleanup(func() {
		ensureHopMaterial, ensureAPIServerConfig, syncOperatorAuth, ensureOperatorAuth = hop, api, sync, ensure
	})

	ensureHopMaterial = func(*ssh.Client) error {
		order = append(order, "hop")
		return nil
	}
	ensureAPIServerConfig = func(*ssh.Client, func(string)) (bool, error) {
		order = append(order, "apiserver")
		return changed, nil
	}
	syncOperatorAuth = func(*ssh.Client) (bool, error) {
		order = append(order, "sync")
		return synced, nil
	}
	ensureOperatorAuth = func(_ *ssh.Client, hosts ...string) error {
		order = append(order, "issuer:"+hosts[0])
		return nil
	}
	return &order
}

// The repair has to run before anything asks the API server to load an
// authentication config: on an old cluster there is no flag for it to load one
// with, so a sync that ran first would report a cluster it could not fix.
func TestUpgradeRepairsTheAPIServerBeforeLoadingTrustIntoIt(t *testing.T) {
	order := stageHostTrust(t, false, true)

	require.NoError(t, reconcileHostTrust(nil, "dex.shop.example"))

	assert.Equal(t, []string{"hop", "apiserver", "sync", "issuer:dex.shop.example"}, *order)
}

// A cluster that already has an issuer is synced and left there; configuring it
// again would probe Dex and wait on the API server for nothing.
func TestUpgradeStopsAtTheSyncWhenAnIssuerIsAlreadyConfigured(t *testing.T) {
	order := stageHostTrust(t, true, false)

	require.NoError(t, reconcileHostTrust(nil, "dex.shop.example"))

	assert.Equal(t, []string{"hop", "apiserver", "sync"}, *order)
}

// Without a domain there is no issuer to point the API server at, and the
// arguments are still worth having.
func TestUpgradeRepairsTheAPIServerEvenWithNoIssuerToConfigure(t *testing.T) {
	order := stageHostTrust(t, false, true)

	require.NoError(t, reconcileHostTrust(nil, ""))

	assert.Equal(t, []string{"hop", "apiserver", "sync"}, *order)
}

// Dex being unreachable leaves the cluster where it started, so it is reported
// rather than raised: an upgrade that failed here would strand a cluster that
// cannot reach its own Dex on an old version too.
func TestUpgradeReportsAnUnreachableIssuerAndCarriesOn(t *testing.T) {
	stageHostTrust(t, false, true)
	ensureOperatorAuth = func(*ssh.Client, ...string) error { return assert.AnError }

	assert.NoError(t, reconcileHostTrust(nil, "dex.shop.example"))
}

// A failed repair stops the upgrade. The API server's arguments are the ground
// everything after this stands on, and its error already says what the rollback
// managed.
func TestUpgradeStopsWhenTheAPIServerRepairFails(t *testing.T) {
	order := stageHostTrust(t, false, false)
	ensureAPIServerConfig = func(*ssh.Client, func(string)) (bool, error) {
		*order = append(*order, "apiserver")
		return false, assert.AnError
	}

	err := reconcileHostTrust(nil, "dex.shop.example")

	require.Error(t, err)
	assert.NotContains(t, *order, "sync", "nothing may load trust into an API server that was not repaired")
}
