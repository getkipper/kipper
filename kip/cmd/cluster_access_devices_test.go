package cmd

import (
	"encoding/base64"
	"errors"
	"io"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/transport"
)

var testNodeKey = wordKey("pubcpubcpubcpubcpubcpubcpubcpubc")

// wordKey is a WireGuard key whose 32 bytes spell word, so it is plainly
// synthetic.
func wordKey(word string) string {
	return base64.StdEncoding.EncodeToString([]byte(word))
}

// testOpsChains are the chains of a loaded kipper_ops table.
var testOpsChains = map[string]installer.OpsChain{
	"input": {Hook: "input", Rules: 6}, "forward": {Hook: "forward", Rules: 2}, "raw": {Hook: "prerouting", Rules: 1},
}

// observeFunc is an AddressObserver for one path to a node.
type observeFunc func(family string) (string, error)

func (f observeFunc) ObservedAddress(family string) (string, error) { return f(family) }

// deviceFixture is the one-node access fixture with this workstation's
// device in the local config, a fake tunnel and fake operator interface.
type deviceFixture struct {
	*accessFixture
	public    string
	cluster   *config.Cluster
	tunnelErr error
	opsCalls  []installer.OpsNodeIntent
	revoked   []string
	routes    []netip.Prefix
	service   netip.Prefix
	reopened  int
	reopenErr error
}

func newDeviceFixture(t *testing.T, state string, observed map[string]string) *deviceFixture {
	t.Helper()
	writeDeviceKey(t)
	t.Setenv("KIP_CLUSTER", "")
	public, err := devicePublicKey(testDeviceKey)
	require.NoError(t, err)
	cluster := config.Cluster{Name: "prod", Host: "192.0.2.10", Ops: &config.Ops{Device: "laptop", State: state, Address: "10.99.0.129", APIHost: "192.0.2.10"}}
	require.NoError(t, config.Save(&config.Config{CurrentCluster: "prod", Clusters: []config.Cluster{cluster}}))
	f := &deviceFixture{accessFixture: newAccessFixture(t, observed), public: public, cluster: &cluster}
	f.c.cluster = f.cluster
	f.c.device = &localDevice{name: "laptop", address: netip.MustParseAddr("10.99.0.129"), public: public, state: state}
	f.c.useTunnel = state == config.OpsActive
	f.c.tunnel = func(accessNode) (installer.AddressObserver, error) {
		return observeFunc(func(string) (string, error) {
			if f.tunnelErr != nil {
				return "", f.tunnelErr
			}
			return "10.99.0.129", nil
		}), nil
	}
	f.c.opsKey = func(installer.CommandRunner) (string, error) { return testNodeKey, nil }
	f.c.applyOps = func(_ installer.CommandRunner, in installer.OpsNodeIntent, _ string) error {
		f.opsCalls = append(f.opsCalls, in)
		return nil
	}
	f.c.revoked = func(installer.CommandRunner) ([]string, error) { return f.revoked, nil }
	f.service = netip.MustParsePrefix("10.43.0.0/16")
	f.c.routes = func(installer.CommandRunner) ([]netip.Prefix, error) { return f.routes, nil }
	f.c.service = func() (netip.Prefix, error) { return f.service, nil }
	f.c.direct = func(host string) installer.AddressObserver {
		for _, n := range f.c.nodes {
			if n.rec.Host == host {
				return n.base
			}
		}
		return nil
	}
	f.c.reopen = func() (*accessCluster, []installer.NodeRecord, error) {
		f.reopened++
		if f.reopenErr != nil {
			return nil, nil, f.reopenErr
		}
		d := *f.c
		d.useTunnel, d.noDirect, d.direct = false, false, nil
		f.c.server = closedRunner{f.c.server}
		f.c.nodes = nil
		for _, n := range d.nodes {
			n.node = closedRunner{n.node}
			f.c.nodes = append(f.c.nodes, n)
		}
		return &d, nil, nil
	}
	f.c.opsState = func(installer.CommandRunner) (installer.OpsHostState, error) {
		return installer.OpsHostState{Dir: true, Enabled: true, Table: true, Up: true, PublicKey: testNodeKey, Port: 443, Address: "10.99.0.1/32",
			MTU: 1380, Routes: []string{"10.99.0.128/25"}, Chains: testOpsChains, TableSum: "abc", LoadedSum: "abc",
			Peers: map[string]string{f.public: "10.99.0.129/32"}}, nil
	}
	return f
}

// deviceRecord is generation 3 with this workstation's device active and
// sources as the address list.
func (f *deviceFixture) deviceRecord(t *testing.T, sources ...string) installer.AccessRecord {
	t.Helper()
	rec := installer.AccessRecord{
		Version: 2, Generation: 3,
		Ops: &installer.AccessOps{Range: installer.DefaultOpsRange, Port: 443, NextNode: 2, NextDevice: 130,
			Nodes: []installer.AccessOpsNode{{MachineID: accessServerID, Address: "10.99.0.1", PublicKey: testNodeKey}}},
		Devices: []installer.AccessDevice{{Name: "laptop", PublicKey: f.public, Address: "10.99.0.129", State: installer.DeviceActive}},
	}
	for _, s := range sources {
		rec.Sources = append(rec.Sources, installer.AccessSource{Prefix: s})
	}
	f.server.store(t, rec)
	f.committed(f.server, 3)
	return rec
}

func TestDeviceOnlyEmptiesTheListAndChecksThroughTheTunnel(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	rec := f.deviceRecord(t, "198.51.100.7/32")
	f.c.lastBaselines = map[string]accessBaseline{accessServerID: {observed: map[string]netip.Addr{"4": netip.MustParseAddr("198.51.100.7")}}}

	require.NoError(t, f.c.offerDeviceOnly(rec, true, nil, false))

	stored := f.server.stored(t)
	assert.Empty(t, stored.Sources)
	assert.Equal(t, 1, stored.ActiveDevices())
	require.Len(t, f.applied, 1)
	a := f.applied[0]
	assert.Empty(t, a.Families, "the direct address it no longer admits is not checked")
	require.NotNil(t, a.Tunnel)
	assert.Equal(t, netip.MustParseAddr("10.99.0.129"), a.Device)
	assert.Equal(t, []string{"t"}, a.ReadyzOn)
	assert.True(t, a.Intent.Devices)
	require.Len(t, f.opsCalls, 1, "the operator interface is loaded before the access table")
	assert.Equal(t, 443, f.opsCalls[0].Port)
	assert.Contains(t, f.out.String(), "accept only registered devices")
	assert.Contains(t, f.out.String(), "admits only registered devices on SSH and the API")
	assert.NotContains(t, f.out.String(), "The list has one entry")
}

func TestDeviceOnlyLeavesOtherPrefixes(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	rec := f.deviceRecord(t, "198.51.100.7/32", "203.0.113.0/24")
	f.c.lastBaselines = map[string]accessBaseline{accessServerID: {observed: map[string]netip.Addr{"4": netip.MustParseAddr("198.51.100.7")}}}

	require.NoError(t, f.c.offerDeviceOnly(rec, false, nil, true))
	assert.Contains(t, f.out.String(), "keeps direct access for 203.0.113.0/24")

	err := f.c.offerDeviceOnly(rec, true, nil, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "203.0.113.0/24")
	assert.Empty(t, f.server.writes)
}

func TestDeviceOnlyAsksInATerminal(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	rec := f.deviceRecord(t, "198.51.100.7/32")
	f.c.lastBaselines = map[string]accessBaseline{accessServerID: {observed: map[string]netip.Addr{"4": netip.MustParseAddr("198.51.100.7")}}}

	require.NoError(t, f.c.offerDeviceOnly(rec, false, strings.NewReader("n\n"), true))
	assert.Empty(t, f.server.writes)
	require.NoError(t, f.c.offerDeviceOnly(rec, false, nil, false))
	assert.Empty(t, f.server.writes, "without a terminal kip only says how")
	assert.Contains(t, f.out.String(), "kip cluster access revoke 198.51.100.7/32")

	require.NoError(t, f.c.offerDeviceOnly(rec, false, strings.NewReader("y\n"), true))
	assert.Empty(t, f.server.stored(t).Sources)
}

func TestRemovingTheDeviceThatIsTheOnlyWayInIsRefused(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{})
	f.deviceRecord(t, "203.0.113.0/24")

	err := f.c.change("device-removed", removeDeviceEdit("laptop"), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "only through device laptop")
	assert.Empty(t, f.server.writes)
}

func TestRemoveDeviceEditKeepsAWayIn(t *testing.T) {
	rec := installer.AccessRecord{Devices: []installer.AccessDevice{{Name: "laptop", State: installer.DeviceActive}}}
	_, _, err := removeDeviceEdit("laptop")(rec, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "last active device")

	_, _, err = removeDeviceEdit("desktop")(rec, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no device called desktop")

	rec.Sources = []installer.AccessSource{{Prefix: "203.0.113.0/24"}}
	next, changed, err := removeDeviceEdit("laptop")(rec, nil)
	require.NoError(t, err)
	assert.Empty(t, next.Devices)
	assert.Equal(t, []string{"laptop"}, changed)
}

func TestRevokeMayEmptyTheListOnlyWithAnActiveDevice(t *testing.T) {
	rec := installer.AccessRecord{Sources: []installer.AccessSource{{Prefix: "198.51.100.7/32"}}}
	_, _, err := revokeEdit([]string{"198.51.100.7/32"})(rec, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty the access list")

	rec.Devices = []installer.AccessDevice{{Name: "laptop", State: installer.DevicePending}}
	_, _, err = revokeEdit([]string{"198.51.100.7/32"})(rec, nil)
	assert.Error(t, err, "a pending device is no way in")

	rec.Devices[0].State = installer.DeviceActive
	next, _, err := revokeEdit([]string{"198.51.100.7/32"})(rec, nil)
	require.NoError(t, err)
	assert.Empty(t, next.Sources)
}

func TestADeviceRevokedFromTheConsoleBlocksChangesOtherThanAllow(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t, "198.51.100.7/32", "203.0.113.0/24")
	f.revoked = []string{f.public}

	err := f.c.change("revoke", revokeEdit([]string{"203.0.113.0/24"}), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "revoked on server")
	assert.Empty(t, f.server.writes)

	require.NoError(t, f.c.change("allow", f.c.allowEdit([]string{"198.51.100.0/24"}, false, ""), false), "allow only adds a way in, so a recovery address can be recorded")

	require.NoError(t, f.c.change("device-removed", removeDeviceEdit("laptop"), false), "removing it is the way out")
	stored := f.server.stored(t)
	assert.Empty(t, stored.Devices)
	assert.Equal(t, "laptop", stored.History[len(stored.History)-1].Device)
	assert.Empty(t, stored.History[len(stored.History)-1].Prefixes)
	last := f.applied[len(f.applied)-1]
	assert.False(t, last.Intent.Devices, "the last device takes the interface rule with it")
	assert.Nil(t, last.Tunnel, "a removed device is not checked")
}

func TestAddingADeviceSetsUpEveryNodeAndVerifiesThroughTheTunnel(t *testing.T) {
	f := newDeviceFixture(t, config.OpsPending, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 3, Sources: []installer.AccessSource{{Prefix: "198.51.100.7/32"}}})
	f.committed(f.server, 3)
	f.c.verifyTunnel = true

	require.NoError(t, f.c.change("device-added", f.c.addDeviceEdit("laptop", f.public, installer.DefaultOpsRange, 443, map[string]string{accessServerID: testNodeKey}), false))

	assert.NotContains(t, f.out.String(), "The list has one entry", "the device being added is the second way in")
	stored := f.server.stored(t)
	assert.Equal(t, 2, stored.Version)
	require.NotNil(t, stored.Ops)
	assert.Equal(t, []installer.AccessOpsNode{{MachineID: accessServerID, Address: "10.99.0.1", PublicKey: testNodeKey}}, stored.Ops.Nodes)
	require.Len(t, stored.Devices, 1)
	assert.Equal(t, installer.DevicePending, stored.Devices[0].State)
	assert.Equal(t, "10.99.0.129", stored.Devices[0].Address)
	assert.Equal(t, "operator@198.51.100.7", stored.Devices[0].AddedBy)
	last := stored.History[len(stored.History)-1]
	assert.Equal(t, "device-added", last.Action)
	assert.Equal(t, "laptop", last.Device)

	require.Len(t, f.opsCalls, 1)
	assert.Equal(t, netip.MustParseAddr("10.99.0.1"), f.opsCalls[0].Address)
	require.Len(t, f.applied, 1)
	a := f.applied[0]
	assert.Equal(t, []string{"4"}, a.Families)
	require.NotNil(t, a.Tunnel, "the pending device is verified through the tunnel")
	assert.Equal(t, []string{"4", "t"}, a.ReadyzOn)
	assert.True(t, a.Intent.Devices)

	cfg, err := config.Load()
	require.NoError(t, err)
	ops := cfg.GetCluster("prod").Ops
	assert.Equal(t, "10.99.0.129", ops.Address)
	require.Len(t, ops.Nodes, 1)
	assert.Equal(t, config.OpsNode{MachineID: accessServerID, Name: "server", Host: "192.0.2.10", Endpoint: "192.0.2.10:443", PublicKey: testNodeKey, Address: "10.99.0.1", SSHPorts: []int{22}, Server: true}, ops.Nodes[0])

	activated, err := f.c.activateDevice("laptop", f.public)
	require.NoError(t, err)
	assert.Equal(t, installer.DeviceActive, activated.Devices[0].State)
	assert.Equal(t, stored.Generation, activated.Generation, "activation changes no host, so the generation stays")
	assert.Equal(t, "device-activated", activated.History[len(activated.History)-1].Action)
}

func TestAddingADeviceFailsWhenTheTunnelDoesNotCarry(t *testing.T) {
	f := newDeviceFixture(t, config.OpsPending, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 3, Sources: []installer.AccessSource{{Prefix: "198.51.100.7/32"}}})
	f.committed(f.server, 3)
	f.c.verifyTunnel = true
	f.c.apply = func(a installer.AccessApply) (string, error) {
		f.applied = append(f.applied, a)
		if _, err := a.Tunnel.ObservedAddress("4"); err != nil {
			return installer.AccessRestored, err
		}
		return installer.AccessCommitted, nil
	}
	f.tunnelErr = errors.New("handshake did not complete")

	err := f.c.change("device-added", f.c.addDeviceEdit("laptop", f.public, installer.DefaultOpsRange, 443, map[string]string{accessServerID: testNodeKey}), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "handshake")
	_, err = f.c.activateDevice("laptop", f.public)
	require.NoError(t, err, "a later run may still activate the recorded pending device")
}

func TestAStaleDeviceStopsUsingItsKey(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 3, Sources: []installer.AccessSource{{Prefix: "198.51.100.7/32"}}})
	f.committed(f.server, 3)

	require.NoError(t, f.c.change("allow", f.c.allowEdit([]string{"203.0.113.0/24"}, false, ""), false))

	assert.Contains(t, f.out.String(), "no longer lists this workstation's device laptop")
	assert.Nil(t, f.c.device)
	assert.Nil(t, f.applied[0].Tunnel)
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, config.OpsStale, cfg.GetCluster("prod").Ops.State)
}

func TestManifestRefusesANodeWhoseKeyChanged(t *testing.T) {
	cluster := &config.Cluster{Ops: &config.Ops{Nodes: []config.OpsNode{{MachineID: accessServerID, PublicKey: testDeviceKey}}}}
	ops := &installer.AccessOps{Port: 443, Nodes: []installer.AccessOpsNode{{MachineID: accessServerID, Address: "10.99.0.1", PublicKey: testNodeKey}}}
	records := []installer.NodeRecord{{Name: "server", Host: "192.0.2.10", MachineID: accessServerID, Role: installer.NodeRoleServer}}

	_, err := manifestFor(cluster, ops, records, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "device repin server")

	cluster.Ops.Nodes[0].PublicKey = testNodeKey
	nodes, err := manifestFor(cluster, ops, records, map[string][]int{accessServerID: {2222}})
	require.NoError(t, err)
	assert.Equal(t, []int{2222}, nodes[0].SSHPorts)
}

func TestAccessReportChecksTheAPIThroughTheDevice(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t, "198.51.100.0/24")
	f.c.families = func(string) []string { return []string{"4"} }
	f.c.apiFamilies = []string{"4"}
	f.c.live = func(installer.CommandRunner) (installer.AccessLive, error) { return installer.AccessLive{}, nil }

	r := f.c.report([]showNode{{rec: f.c.nodes[0].rec, client: f.server}})
	assert.Contains(t, r.Nodes[0].Admitted, "API through device laptop")

	f.c.readyz = func(family string) error {
		if family == "t" {
			return errors.New("no handshake")
		}
		return nil
	}
	r = f.c.report([]showNode{{rec: f.c.nodes[0].rec, client: f.server}})
	assert.Contains(t, strings.Join(r.Nodes[0].Problems, "\n"), "did not answer through device laptop")
}

func TestResetRemovesEveryOperatorInterfaceAfterTheRecordAndForgetsTheDevice(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t, "198.51.100.7/32")
	worker := f.addWorker()

	require.NoError(t, f.c.resetAll(nil))

	assert.True(t, f.server.deleted)
	assert.Equal(t, []installer.FirewallRunner{worker, f.server}, f.opsRemoved, "the server, which carries the session, goes last")
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Nil(t, cfg.GetCluster("prod").Ops)
	path, err := config.OpsKeyPath("prod")
	require.NoError(t, err)
	assert.NoFileExists(t, path)
}

func TestAutoUsesTheDirectPathWhenTheTunnelIsDown(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t, "198.51.100.7/32")
	f.tunnelErr = errors.New("handshake did not complete")

	require.NoError(t, f.c.change("allow", f.c.allowEdit([]string{"203.0.113.0/24"}, false, ""), false))

	require.Len(t, f.applied, 1)
	assert.Nil(t, f.applied[0].Tunnel, "only the direct path got in, so only it is checked")
	assert.Equal(t, []string{"4"}, f.applied[0].Families)
	assert.NotContains(t, f.applied[0].ReadyzOn, "t")
}

func laptopRequest() deviceRequest {
	return deviceRequest{name: "laptop", apiHost: "192.0.2.10", rng: installer.DefaultOpsRange, port: 443}
}

func TestEnrollmentPreparesEveryNodeBeforeListingThisWorkstation(t *testing.T) {
	f := newDeviceFixture(t, config.OpsPending, map[string]string{"4": "198.51.100.7"})
	f.c.opsKey = func(installer.CommandRunner) (string, error) {
		return "", errors.New("firewalld runs on this node")
	}

	_, err := f.c.enroll(laptopRequest())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "firewalld")
	assert.Empty(t, f.server.writes, "a refused device leaves the access list alone")
}

func TestEnrollmentRefusesARangeThatOverlapsANodeNetwork(t *testing.T) {
	for name, tc := range map[string]struct {
		routes  []netip.Prefix
		service string
		want    string
	}{
		"route":   {routes: []netip.Prefix{netip.MustParsePrefix("10.99.0.128/25")}, service: "10.43.0.0/16", want: "10.99.0.128/25"},
		"service": {service: "10.99.0.0/16", want: "the Service network"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDeviceFixture(t, config.OpsPending, map[string]string{"4": "198.51.100.7"})
			f.server.store(t, installer.AccessRecord{Version: 1, Generation: 3, Sources: []installer.AccessSource{{Prefix: "198.51.100.7/32"}}})
			f.committed(f.server, 3)
			f.routes, f.service = tc.routes, netip.MustParsePrefix(tc.service)
			prepared := false
			f.c.opsKey = func(installer.CommandRunner) (string, error) {
				prepared = true
				return testNodeKey, nil
			}

			_, err := f.c.enroll(laptopRequest())

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.False(t, prepared, "the range is checked before any node is touched")
			assert.Empty(t, f.server.writes)
		})
	}
}

func TestEnrollmentRefusesWhenThePodNetworkCannotBeRead(t *testing.T) {
	f := newDeviceFixture(t, config.OpsPending, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 3, Sources: []installer.AccessSource{{Prefix: "198.51.100.7/32"}}})
	f.committed(f.server, 3)
	f.c.pod = func() (netip.Prefix, error) { return netip.Prefix{}, errors.New("reading the k3s config failed") }
	prepared := false
	f.c.opsKey = func(installer.CommandRunner) (string, error) {
		prepared = true
		return testNodeKey, nil
	}

	_, err := f.c.enroll(laptopRequest())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading the k3s config failed")
	assert.False(t, prepared, "an unchecked range touches no node")
	assert.Empty(t, f.server.writes)
}

func TestEnrollmentMovesPastADeviceTheRecordNoLongerLists(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 3, Sources: []installer.AccessSource{{Prefix: "198.51.100.7/32"}}})
	f.committed(f.server, 3)
	req := laptopRequest()
	req.name = "desktop"

	rec, err := f.c.enroll(req)

	require.NoError(t, err)
	assert.Contains(t, f.out.String(), "no longer lists this workstation's device laptop")
	d, ok := rec.Device("desktop")
	require.True(t, ok)
	assert.Equal(t, installer.DeviceActive, d.State)
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, config.OpsActive, cfg.GetCluster("prod").Ops.State)
	assert.Equal(t, "desktop", cfg.GetCluster("prod").Ops.Device)
}

func TestEnrollmentAfterAResetForgetsTheOldNodeKeys(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 3, Sources: []installer.AccessSource{{Prefix: "198.51.100.7/32"}}})
	f.committed(f.server, 3)
	old := []config.OpsNode{{MachineID: f.c.nodes[0].rec.MachineID, Name: "server", PublicKey: wordKey("a key from before the reset.....")}}
	require.NoError(t, updateLocalOps(f.cluster, func(o *config.Ops) *config.Ops {
		o.Nodes = old
		return o
	}))

	_, err := f.c.enroll(laptopRequest())

	require.NoError(t, err, "the nodes' new keys come from this enrollment, so the old pins are dropped")
	require.NotEmpty(t, f.cluster.Ops.Nodes)
	assert.Equal(t, testNodeKey, f.cluster.Ops.Nodes[0].PublicKey)
}

func TestAllowHereThroughTheTunnelAsksForThePrefix(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{})
	f.deviceRecord(t, "198.51.100.7/32")

	err := f.c.change("allow", f.c.allowEdit(nil, true, ""), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Name the prefix to admit")
	assert.NotContains(t, err.Error(), "--via direct", "a workstation that only reaches the cluster through its device cannot connect directly")
}

func TestALostDeviceKeyLeavesTheDeviceStale(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	path, err := config.OpsKeyPath("prod")
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))

	device, err := loadLocalDevice(f.cluster)

	require.NoError(t, err, "a lost key must not close every access command, device remove included")
	assert.Nil(t, device)
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, config.OpsStale, cfg.GetCluster("prod").Ops.State)
	assert.Equal(t, transport.Direct, transport.Mode(cfg.GetCluster("prod")))
}

func TestAccessCommandsNoticeALostKeyBeforeConnecting(t *testing.T) {
	for name, run := range map[string]func(*cobra.Command) error{
		"session": func(cmd *cobra.Command) error {
			_, err := openAccessSession(cmd, "apply", false)
			return err
		},
		"show": func(cmd *cobra.Command) error { return runClusterAccessShow(cmd, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			writeDeviceKey(t)
			t.Setenv("KIP_CLUSTER", "")
			cluster := config.Cluster{Name: "prod", Ops: &config.Ops{Device: "laptop", State: config.OpsActive, Address: "10.99.0.129", Via: transport.Tunnel}}
			require.NoError(t, config.Save(&config.Config{CurrentCluster: "prod", Clusters: []config.Cluster{cluster}}))
			path, err := config.OpsKeyPath("prod")
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, []byte("not a key\n"), 0o600))
			cmd := &cobra.Command{}
			cmd.Flags().String("ssh-key", "", "")
			cmd.Flags().Bool("json", false, "")

			err = run(cmd)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "no host recorded", "the key is checked before the first connection")
			cfg, err := config.Load()
			require.NoError(t, err)
			assert.Equal(t, config.OpsStale, cfg.GetCluster("prod").Ops.State)
		})
	}
}

func TestEnrollmentRefusesASecondDeviceForThisWorkstation(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t, "198.51.100.7/32")
	req := laptopRequest()
	req.name = "desktop"

	_, err := f.c.enroll(req)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "already device laptop")
	assert.Empty(t, f.server.writes)
}

func TestAccessReportNoticesAStaleDevice(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 3, Sources: []installer.AccessSource{{Prefix: "198.51.100.0/24"}}})
	f.c.live = func(installer.CommandRunner) (installer.AccessLive, error) { return installer.AccessLive{}, nil }

	f.c.report([]showNode{{rec: f.c.nodes[0].rec, client: f.server}})

	assert.Contains(t, f.out.String(), "no longer lists this workstation's device laptop")
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, config.OpsStale, cfg.GetCluster("prod").Ops.State)
}

func TestRemovingADeviceTheRecordNoLongerListsLoadsTheRecordEverywhere(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.server.store(t, installer.AccessRecord{Version: 1, Generation: 3, Sources: []installer.AccessSource{{Prefix: "198.51.100.7/32"}}})
	f.committed(f.server, 3)
	worker := f.addWorker()
	f.committed(worker, 3)
	f.c.useTunnel = false

	require.NoError(t, f.c.removeDevice("laptop", f.c.device))

	assert.Len(t, f.applied, 2, "a removal that stopped after the record write is finished on every node")
	assert.Contains(t, f.out.String(), "no longer lists device laptop")
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Nil(t, cfg.GetCluster("prod").Ops)
	path, err := config.OpsKeyPath("prod")
	require.NoError(t, err)
	assert.NoFileExists(t, path)
}

func TestRemovingADeviceWithoutARecordForgetsOnlyTheKey(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.c.useTunnel = false

	require.NoError(t, f.c.removeDevice("laptop", f.c.device))

	assert.Empty(t, f.server.writes)
	assert.Empty(t, f.applied)
	path, err := config.OpsKeyPath("prod")
	require.NoError(t, err)
	assert.NoFileExists(t, path)
}

func TestRemovingAnotherDeviceAgainFinishesOnlyARecordedRemoval(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	rec := f.deviceRecord(t, "198.51.100.7/32")
	rec.History = append(rec.History, installer.AccessChange{Action: "device-removed", Device: "desktop", Generation: 3})
	f.server.store(t, rec)

	err := f.c.removeDevice("tablet", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no device called tablet")
	assert.Empty(t, f.applied)

	require.NoError(t, f.c.removeDevice("desktop", nil))
	assert.NotEmpty(t, f.applied)
}

func TestSyncManifestAddsANodeAnotherWorkstationJoined(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	rec := f.deviceRecord(t)
	require.NoError(t, updateLocalOps(f.cluster, func(o *config.Ops) *config.Ops {
		o.Nodes = []config.OpsNode{{MachineID: accessServerID, Name: "server", Host: "192.0.2.10", PublicKey: testNodeKey, SSHPorts: []int{2222}, Server: true}}
		return o
	}))
	f.addWorker()
	rec.Ops.Nodes, rec.Ops.NextNode = append(rec.Ops.Nodes, installer.AccessOpsNode{MachineID: accessWorkerID, Address: "10.99.0.2", PublicKey: testWorkerKey}), 3
	f.server.store(t, rec)

	require.NoError(t, f.c.syncManifest())

	cfg, err := config.Load()
	require.NoError(t, err)
	nodes := cfg.GetCluster("prod").Ops.Nodes
	require.Len(t, nodes, 2)
	assert.Equal(t, []int{2222}, nodes[0].SSHPorts, "a known node keeps its ports")
	assert.Equal(t, "worker-1", nodes[1].Name)
	assert.Equal(t, testWorkerKey, nodes[1].PublicKey)
	assert.Equal(t, []int{22}, nodes[1].SSHPorts)

	rec.Ops.Nodes[0].PublicKey = wordKey("changedchangedchangedchangedchan")
	f.server.store(t, rec)
	err = f.c.syncManifest()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "device repin server")
}

func TestSyncManifestSkipsTheOldPinsOfADeviceTheRecordDropped(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	rec := f.deviceRecord(t, "198.51.100.7/32")
	require.NoError(t, updateLocalOps(f.cluster, func(o *config.Ops) *config.Ops {
		o.Nodes = []config.OpsNode{{MachineID: accessServerID, Name: "server", Host: "192.0.2.10", PublicKey: wordKey("a key from before the reset....."), Server: true}}
		return o
	}))
	rec.Devices = []installer.AccessDevice{{Name: "desktop", PublicKey: wordKey("desktopdesktopdesktopdesktopdesk"), Address: "10.99.0.130", State: installer.DeviceActive}}
	rec.Ops.NextDevice = 131
	f.server.store(t, rec)

	require.NoError(t, f.c.syncManifest(), "another workstation's new enrollment must not lock this one out of 'device add'")

	assert.Nil(t, f.c.device)
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, config.OpsStale, cfg.GetCluster("prod").Ops.State)
}

func TestSyncManifestOverTheDirectPathWarnsAboutAChangedKey(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	rec := f.deviceRecord(t, "198.51.100.7/32")
	old := []config.OpsNode{{MachineID: accessServerID, Name: "server", Host: "192.0.2.10", PublicKey: wordKey("a key from before the reinstall."), Server: true}}
	require.NoError(t, updateLocalOps(f.cluster, func(o *config.Ops) *config.Ops {
		o.Nodes = old
		return o
	}))
	f.server.store(t, rec)
	f.c.useTunnel = false

	require.NoError(t, f.c.syncManifest(), "a direct session can still remove the device or reset")

	assert.Contains(t, f.out.String(), "device repin server")
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, old[0].PublicKey, cfg.GetCluster("prod").Ops.Nodes[0].PublicKey, "the pin is not replaced")
}

func TestDirectRemovalOfAnotherDeviceKeepsAChangedPin(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	rec := f.deviceRecord(t, "198.51.100.7/32")
	rec.Devices = append(rec.Devices, installer.AccessDevice{Name: "desktop", PublicKey: wordKey("desktopdesktopdesktopdesktopdesk"), Address: "10.99.0.130", State: installer.DeviceActive})
	rec.Ops.NextDevice = 131
	f.server.store(t, rec)
	old := wordKey("a key from before the reinstall.")
	require.NoError(t, updateLocalOps(f.cluster, func(o *config.Ops) *config.Ops {
		o.Nodes = []config.OpsNode{{MachineID: accessServerID, Name: "server", Host: "192.0.2.10", PublicKey: old, Server: true}}
		return o
	}))
	f.c.useTunnel = false

	require.NoError(t, f.c.removeDevice("desktop", f.c.device), "a stolen device can be removed over the direct path")

	_, ok := f.server.stored(t).Device("desktop")
	assert.False(t, ok)
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, old, cfg.GetCluster("prod").Ops.Nodes[0].PublicKey, "the changed key is not trusted")
}

func TestRefreshManifestIgnoresPinsOnceTheDeviceLeavesTheRecord(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	rec := f.deviceRecord(t, "198.51.100.7/32")
	require.NoError(t, updateLocalOps(f.cluster, func(o *config.Ops) *config.Ops {
		o.Nodes = []config.OpsNode{{MachineID: accessServerID, Name: "server", Host: "192.0.2.10", PublicKey: wordKey("a key from before the reinstall."), Server: true}}
		return o
	}))
	rec.Devices = nil

	assert.NoError(t, f.c.refreshManifest(rec, nil), "removing this workstation's own device needs no tunnel")
}

func TestRefreshDeviceNodesGivesOtherCommandsANodeAddedElsewhere(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	rec := f.deviceRecord(t)
	require.NoError(t, updateLocalOps(f.cluster, func(o *config.Ops) *config.Ops {
		o.Nodes = []config.OpsNode{{MachineID: accessServerID, Name: "server", Host: "192.0.2.10", PublicKey: testNodeKey, SSHPorts: []int{22}, Server: true}}
		return o
	}))
	f.addWorker()
	rec.Ops.Nodes, rec.Ops.NextNode = append(rec.Ops.Nodes, installer.AccessOpsNode{MachineID: accessWorkerID, Address: "10.99.0.2", PublicKey: testWorkerKey}), 3
	f.server.store(t, rec)

	refreshDeviceNodes(io.Discard, f.cluster, f.server, f.c.records)

	require.Len(t, f.cluster.Ops.Nodes, 2, "the caller's own cluster sees the new node in this run")
	assert.Equal(t, "worker-1", f.cluster.Ops.Nodes[1].Name)
}

// silentRunner fails the test on any command. Other methods panic.
type silentRunner struct {
	installer.FirewallRunner
	t *testing.T
}

func (r silentRunner) Run(command string) (string, error) {
	r.t.Errorf("unexpected command: %s", command)
	return "", errors.New("unexpected")
}

func (r silentRunner) RunStdin(command string, _ io.Reader) (string, error) { return r.Run(command) }

func TestRefreshDeviceNodesWithoutADeviceRunsNothing(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})

	refreshDeviceNodes(io.Discard, &config.Cluster{Name: "prod"}, silentRunner{t: t}, f.c.records)
}

func TestSyncManifestNeedsARecordedServer(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t)
	require.NoError(t, updateLocalOps(f.cluster, func(o *config.Ops) *config.Ops {
		o.Nodes = []config.OpsNode{{MachineID: accessServerID, Name: "server", Host: "192.0.2.10", PublicKey: testNodeKey, Server: true}}
		return o
	}))
	f.c.records = []installer.NodeRecord{{Name: "server", Host: "192.0.2.10", Role: installer.NodeRoleServer}}

	require.NoError(t, f.c.syncManifest())

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Len(t, cfg.GetCluster("prod").Ops.Nodes, 1, "the manifest keeps its server")
}

func TestRepinRecordsTheKeyReadFromTheNode(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t, "198.51.100.7/32")
	key := wordKey("reinstalledreinstalledreinstalle")
	require.NoError(t, updateLocalOps(f.cluster, func(o *config.Ops) *config.Ops {
		o.Nodes = []config.OpsNode{{MachineID: accessServerID, Name: "server", Host: "192.0.2.10", PublicKey: key, Server: true}}
		return o
	}))

	require.NoError(t, f.c.recordNodeKey(accessServerID, "server", key))

	rec := f.server.stored(t)
	on, ok := rec.Ops.Node(accessServerID)
	require.True(t, ok)
	assert.Equal(t, key, on.PublicKey)
	assert.Equal(t, "repin", rec.History[len(rec.History)-1].Action)
	writes := len(f.server.writes)

	require.NoError(t, f.c.recordNodeKey(accessServerID, "server", key))
	assert.Len(t, f.server.writes, writes, "a key the record already holds changes nothing")
}

func TestManifestKeepsTheSSHPortsOfNodesTheChangeDidNotRead(t *testing.T) {
	cluster := &config.Cluster{Ops: &config.Ops{Nodes: []config.OpsNode{{MachineID: accessServerID, PublicKey: testNodeKey, SSHPorts: []int{2222}}}}}
	ops := &installer.AccessOps{Port: 443, Nodes: []installer.AccessOpsNode{{MachineID: accessServerID, Address: "10.99.0.1", PublicKey: testNodeKey}}}
	records := []installer.NodeRecord{{Name: "server", Host: "192.0.2.10", MachineID: accessServerID, Role: installer.NodeRoleServer}}

	nodes, err := manifestFor(cluster, ops, records, nil)

	require.NoError(t, err)
	assert.Equal(t, []int{2222}, nodes[0].SSHPorts)
}

func TestAccessReportSkipsTheDirectAPIWhenOnlyDevicesAreAdmitted(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t)
	f.c.families = func(string) []string { return []string{"4"} }
	f.c.apiFamilies = []string{"4"}
	f.c.live = func(installer.CommandRunner) (installer.AccessLive, error) { return installer.AccessLive{}, nil }
	var direct int
	f.c.readyz = func(family string) error {
		if family != "t" {
			direct++
		}
		return nil
	}

	r := f.c.report([]showNode{{rec: f.c.nodes[0].rec, client: f.server}})

	assert.Zero(t, direct, "the empty list refuses it anyway, and the check would wait out its deadline")
	assert.Contains(t, r.Nodes[0].Admitted, "API through device laptop")
}

func TestNodeAddRefusesWhileADeviceIsRevokedFromTheConsole(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t, "198.51.100.7/32")
	f.revoked = []string{f.public}

	_, _, err := prepareNodeAccess(f.c, f.c.nodes, nil, "node add 192.0.2.11", true)
	require.Error(t, err, "a joining node has no revocation list and would admit the device")
	assert.Contains(t, err.Error(), "revoked on server")

	_, _, err = prepareNodeAccess(f.c, f.c.nodes, nil, "node remove worker-1", false)
	assert.NoError(t, err, "removing a node admits nothing new")
}

func TestRevokingTheLastDirectAddressNeedsTheAPIThroughTheDevice(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t, "198.51.100.7/32")
	f.c.readyz = func(family string) error {
		if family == "t" {
			return errors.New("no answer through the tunnel")
		}
		return nil
	}

	err := f.c.change("revoke", revokeEdit([]string{"198.51.100.7/32"}), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Kubernetes API")
	assert.Empty(t, f.server.writes)
}

func TestResetKeepsTheTunnelWhenANodeIsNotReachableDirectly(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t)
	f.c.direct = func(string) installer.AddressObserver { return unreachableHost{} }

	err := f.c.resetAll(nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "server")
	assert.False(t, f.server.deleted, "the record stays until every node is reachable directly")
	assert.Empty(t, f.opsRemoved, "the tunnel stays the way in")
	path, err := config.OpsKeyPath("prod")
	require.NoError(t, err)
	assert.FileExists(t, path)
}

func TestAccessReportComparesTheOperatorInterface(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t, "198.51.100.0/24")
	f.c.families = func(string) []string { return []string{"4"} }
	f.c.live = func(installer.CommandRunner) (installer.AccessLive, error) { return installer.AccessLive{}, nil }

	r := f.c.report([]showNode{{rec: f.c.nodes[0].rec, client: f.server}})
	assert.NotContains(t, strings.Join(r.Nodes[0].Problems, "\n"), "operator interface")
	require.Len(t, r.Devices, 1)
	var out strings.Builder
	printAccessReport(&out, r)
	assert.Contains(t, out.String(), "one source and one device")
	assert.Contains(t, out.String(), "laptop  10.99.0.129  active")
	assert.Contains(t, out.String(), "ops.sh revoke-device <name>")

	f.c.opsState = func(installer.CommandRunner) (installer.OpsHostState, error) { return installer.OpsHostState{}, nil }
	r = f.c.report([]showNode{{rec: f.c.nodes[0].rec, client: f.server}})
	assert.False(t, r.Match)
	assert.Contains(t, r.Nodes[0].Problems, "the operator interface kipops is down")
}

// unreachableHost refuses every direct connection.
type unreachableHost struct{}

func (unreachableHost) ObservedAddress(string) (string, error) {
	return "", errors.New("connection timed out")
}

// closedRunner is a connection of a session that reset has closed.
type closedRunner struct {
	installer.FirewallRunner
}

func (closedRunner) Run(string) (string, error) { return "", errors.New("session closed") }

func (closedRunner) RunStdin(string, io.Reader) (string, error) {
	return "", errors.New("session closed")
}

func TestResetOverTheTunnelFinishesOnAFreshDirectSession(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t)
	f.addWorker()

	require.NoError(t, f.c.resetAll(nil))

	assert.Equal(t, 1, f.reopened)
	assert.Len(t, f.removed, 4, "the direct session opens every node again under its own lease")
	assert.True(t, f.server.deleted)
	assert.Len(t, f.opsRemoved, 2)
}

func TestResetKeepsTheTunnelWhenNoDirectSessionOpens(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t)
	f.reopenErr = errors.New("connection timed out")

	err := f.c.resetAll(nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--via direct")
	assert.False(t, f.server.deleted)
	assert.Empty(t, f.opsRemoved)
}

func TestResetOverDirectSSHStillNeedsTheAPIDirectly(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t)
	f.c.useTunnel, f.c.direct = false, nil
	f.c.readyz = func(family string) error {
		if family != "t" {
			return errors.New("dial tcp 192.0.2.10:6443: i/o timeout")
		}
		return nil
	}

	err := f.c.resetAll(nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Kubernetes API did not answer this workstation directly")
	assert.Zero(t, f.reopened)
	assert.False(t, f.server.deleted)
	assert.Empty(t, f.opsRemoved)
}

func TestResetKeepsTheTunnelWhenTheAPIDoesNotAnswerDirectly(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t)
	f.c.readyz = func(family string) error {
		if family != "t" {
			return errors.New("dial tcp 192.0.2.10:6443: i/o timeout")
		}
		return nil
	}

	err := f.c.resetAll(nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Kubernetes API did not answer this workstation directly")
	assert.False(t, f.server.deleted)
	assert.Empty(t, f.opsRemoved)
}

func TestApplyGivesANodeMissingFromTheOpsStateItsInterface(t *testing.T) {
	f := newDeviceFixture(t, config.OpsActive, map[string]string{"4": "198.51.100.7"})
	f.deviceRecord(t, "198.51.100.7/32")
	worker := f.addWorker()
	f.committed(worker, 3)
	f.c.opsKey = func(r installer.CommandRunner) (string, error) {
		if r == installer.CommandRunner(worker) {
			return testWorkerKey, nil
		}
		return testNodeKey, nil
	}

	require.NoError(t, f.c.change("apply", nil, false))

	on, ok := f.server.stored(t).Ops.Node(accessWorkerID)
	require.True(t, ok)
	assert.Equal(t, testWorkerKey, on.PublicKey)
	assert.Len(t, f.opsCalls, 2, "both interfaces are loaded")
}
