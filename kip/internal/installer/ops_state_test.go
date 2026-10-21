package installer

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
)

var (
	stateNodeKey   = wordKey("nodenodenodenodenodenodenodenode")
	stateDeviceKey = wordKey("devicedevicedevicedevicedevice12")
	stateOtherKey  = wordKey("otherotherotherotherotherother12")
)

// wordKey is a WireGuard key whose 32 bytes spell word, so it is plainly
// synthetic.
func wordKey(word string) string {
	return base64.StdEncoding.EncodeToString([]byte(word))
}

func stateRecord() AccessRecord {
	return AccessRecord{
		Version: 2, Generation: 3,
		Ops: &AccessOps{Range: DefaultOpsRange, Port: 443, NextNode: 2, NextDevice: 131,
			Nodes: []AccessOpsNode{{MachineID: "m-1", Address: "10.99.0.1", PublicKey: stateNodeKey}}},
		Devices: []AccessDevice{
			{Name: "laptop", PublicKey: stateDeviceKey, Address: "10.99.0.129", State: DeviceActive},
			{Name: "desktop", PublicKey: stateOtherKey, Address: "10.99.0.130", State: DevicePending},
		},
	}
}

// matchingState is a node whose interface matches stateRecord.
func matchingState() OpsHostState {
	return OpsHostState{Dir: true, Table: true, Up: true, PublicKey: stateNodeKey, Port: 443, Address: "10.99.0.1/32",
		MTU: opsMTU, Routes: []string{"10.99.0.128/25"}, Chains: renderedOpsChains(), TableSum: "abc", LoadedSum: "abc",
		Peers: map[string]string{stateDeviceKey: "10.99.0.129/32", stateOtherKey: "10.99.0.130/32"}}
}

func TestParseOpsStateReadsTheInterfaceWithoutItsPrivateKey(t *testing.T) {
	out := "dir=1\ntable=1\nchain=input\nhook=input input\nrule=input\nrule=input\nchain=raw\nhook=raw prerouting\nrule=raw\ntablesum=abc\nloadedsum=abd\n" +
		"ufw=1\nufwrule=443/udp                    ALLOW       Anywhere                   # kipper-ops\nup=1\nmtu=1380\nroute=10.99.0.128/25\nkey=" + stateNodeKey + "\nport=443\naddress=10.99.0.1/32\n" +
		"peer=" + stateDeviceKey + "\t10.99.0.129/32\nrevoked=" + stateOtherKey + "\nrevoked=junk\n"

	assert.Equal(t, OpsHostState{
		Dir: true, Table: true, Up: true, PublicKey: stateNodeKey, Port: 443, Address: "10.99.0.1/32", MTU: 1380,
		Routes: []string{"10.99.0.128/25"}, UFW: true, UFWRules: []string{"443/udp                    ALLOW       Anywhere                   # kipper-ops"},
		TableSum: "abc", LoadedSum: "abd",
		Chains: map[string]OpsChain{"input": {Hook: "input", Rules: 2}, "raw": {Hook: "prerouting", Rules: 1}},
		Peers:  map[string]string{stateDeviceKey: "10.99.0.129/32"}, Revoked: []string{stateOtherKey},
	}, parseOpsState(out))
	assert.NotContains(t, opsStateCommand(""), "private", "show never moves the node's private key")
	assert.NotContains(t, opsStateCommand(""), "dump")
}

func TestRenderedOpsChainsFollowTheRenderer(t *testing.T) {
	assert.Equal(t, map[string]OpsChain{
		"input": {Hook: "input", Rules: 6}, "forward": {Hook: "forward", Rules: 2}, "raw": {Hook: "prerouting", Rules: 1},
	}, renderedOpsChains())
}

func TestOpsDriftMatchesARecordedInterface(t *testing.T) {
	assert.Empty(t, OpsDrift(matchingState(), stateRecord(), "m-1"))
}

func TestOpsDriftNamesEachDifference(t *testing.T) {
	st := matchingState()
	st.PublicKey, st.Port, st.Address, st.MTU, st.Routes = stateOtherKey, 51820, "10.99.0.9/32", 1420, nil
	st.Chains = map[string]OpsChain{"input": {Hook: "input"}, "raw": {Hook: "prerouting", Rules: 1}}
	st.UFW, st.UFWRules = true, []string{"51820/udp                  ALLOW       Anywhere"}
	st.Peers = map[string]string{stateDeviceKey: "10.99.0.0/24", "dW5yZWNvcmRlZHVucmVjb3JkZWR1bnJlY29yZGVkMTI=": "10.99.0.200/32"}

	assert.Equal(t, []string{
		"the kipper_ops table has no forward chain",
		"the kipper_ops input chain holds 0 rules, not 6",
		"UFW does not allow the tunnel's UDP port 443 from every address",
		"the operator interface's key is not the recorded one. If the host was reinstalled, run 'kip cluster access device repin' from each device",
		"the operator interface listens on UDP 51820, not the recorded 443",
		`the operator interface has address "10.99.0.9/32", not the recorded 10.99.0.1`,
		"the operator interface has MTU 1420, not 1380",
		"the operator interface has no route to the devices' 10.99.0.128/25",
		`device laptop is loaded with allowed IPs "10.99.0.0/24", not 10.99.0.129/32`,
		"device desktop (pending) is not loaded",
		"peer dW5yZWNvcmRlZHVucmVjb3JkZWR1bnJlY29yZGVkMTI= is loaded but not recorded. Run 'kip cluster access apply'",
	}, OpsDrift(st, stateRecord(), "m-1"))
}

func TestOpsDriftNoticesATableThatChangedAfterTheLoad(t *testing.T) {
	st := matchingState()
	st.TableSum = "abd"
	assert.Equal(t, []string{"the kipper_ops table differs from the one kip loaded. Run 'kip cluster access apply'"}, OpsDrift(st, stateRecord(), "m-1"))
	st.TableSum, st.LoadedSum = "", ""
	assert.Len(t, OpsDrift(st, stateRecord(), "m-1"), 1, "an unreadable listing never matches")
}

func TestOpsDriftNeedsUFWToAllowTheTunnelPortFromEveryAddress(t *testing.T) {
	allow := "443/udp                    ALLOW       Anywhere                   # kipper-ops"
	for name, tc := range map[string]struct {
		rules []string
		ok    bool
	}{
		"allowed":                       {[]string{"22/tcp                     LIMIT       Anywhere", "53/udp                     DENY        Anywhere", allow}, true},
		"allowed after narrower allows": {[]string{"443/udp                    ALLOW       198.51.100.7", allow}, true},
		"allowed before a deny":         {[]string{allow, "Anywhere                   DENY        198.51.100.0/24"}, true},
		"denied for the port":           {[]string{"443/udp                    DENY        Anywhere", allow}, false},
		"denied for a source":           {[]string{"Anywhere                   DENY        198.51.100.0/24", allow}, false},
		"denied on every protocol":      {[]string{"443                        REJECT      Anywhere", allow}, false},
		"denied for a port range":       {[]string{"400:500/udp                DENY        Anywhere", allow}, false},
		"denied for an app":             {[]string{"OpenSSH                    DENY        Anywhere", allow}, false},
		"denied to a network":           {[]string{"192.0.2.0/24               DENY        198.51.100.0/24", allow}, false},
		"denied to an address and port": {[]string{"192.0.2.10 443/udp         REJECT IN   Anywhere", allow}, false},
		"denied for TCP only":           {[]string{"443/tcp                    DENY        Anywhere", allow}, true},
		"denied only over IPv6":         {[]string{"443/udp (v6)               DENY        Anywhere (v6)", allow}, true},
		"denied only outgoing":          {[]string{"443/udp                    DENY OUT    Anywhere", allow}, true},
		"allowed from one address only": {[]string{"443/udp                    ALLOW       198.51.100.7"}, false},
		"missing":                       {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			st := matchingState()
			st.UFW, st.UFWRules = true, tc.rules
			assert.Equal(t, tc.ok, len(OpsDrift(st, stateRecord(), "m-1")) == 0, OpsDrift(st, stateRecord(), "m-1"))
		})
	}
}

func TestOpsDriftReportsConsoleRevocationsAndLeftovers(t *testing.T) {
	st := matchingState()
	st.Peers, st.Revoked = map[string]string{stateOtherKey: "10.99.0.130/32"}, []string{stateDeviceKey}

	assert.Equal(t, []string{"device laptop was revoked here from the console. Remove it with 'kip cluster access device remove laptop'"}, OpsDrift(st, stateRecord(), "m-1"))

	assert.Contains(t, OpsDrift(OpsHostState{Dir: true}, AccessRecord{Version: 1}, "m-1")[0], "left on this node")
	assert.Empty(t, OpsDrift(OpsHostState{}, AccessRecord{Version: 1}, "m-1"))
	assert.Equal(t, []string{"the kipper_ops table is not loaded", "the operator interface kipops is down"}, OpsDrift(OpsHostState{}, stateRecord(), "m-1"))
}
