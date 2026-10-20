package installer

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKey(b byte) string {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return base64.StdEncoding.EncodeToString(k)
}

func sampleDeviceRecord() AccessRecord {
	return AccessRecord{
		Version: 2, Generation: 9,
		Sources: []AccessSource{{Prefix: "198.51.100.7/32"}},
		Ops: &AccessOps{
			Range: "10.99.0.0/24", Port: 443, NextNode: 3, NextDevice: 131,
			Nodes: []AccessOpsNode{
				{MachineID: "m-server", Address: "10.99.0.1", PublicKey: testKey(1)},
				{MachineID: "m-worker", Address: "10.99.0.2", PublicKey: testKey(2)},
			},
		},
		Devices: []AccessDevice{
			{Name: "laptop", PublicKey: testKey(9), Address: "10.99.0.129", State: DeviceActive, AddedBy: "operator@198.51.100.7", AddedAt: "2026-10-21T00:00:00Z"},
			{Name: "desktop", PublicKey: testKey(10), Address: "10.99.0.130", State: DevicePending},
		},
	}
}

func TestReadAccessRecordReadsVersionTwo(t *testing.T) {
	r := &routedRunner{routes: []route{{contains: "get configmap kipper-access", out: accessRecordJSON(t, sampleDeviceRecord())}}}
	rec, present, err := ReadAccessRecord(r)
	require.NoError(t, err)
	assert.True(t, present)
	assert.Equal(t, sampleDeviceRecord(), rec)
}

func TestAnEmptyListIsValidOnlyWithAnActiveDevice(t *testing.T) {
	rec := sampleDeviceRecord()
	rec.Sources = nil
	assert.NoError(t, rec.validate(), "an active device is a way in")

	rec.Devices = rec.Devices[1:]
	err := rec.validate()
	require.Error(t, err, "a pending device never counts as a way in")
	assert.Contains(t, err.Error(), "no operator prefixes and no active device")
}

func TestVersionTwoRecordsAreValidated(t *testing.T) {
	for name, mutate := range map[string]func(*AccessRecord){
		"no ops":                func(r *AccessRecord) { r.Ops = nil },
		"devices in version 1":  func(r *AccessRecord) { r.Version = 1 },
		"range not a /24":       func(r *AccessRecord) { r.Ops.Range = "10.99.0.0/16" },
		"range not canonical":   func(r *AccessRecord) { r.Ops.Range = "10.99.0.5/24" },
		"range IPv6":            func(r *AccessRecord) { r.Ops.Range = "2001:db8::/120" },
		"port zero":             func(r *AccessRecord) { r.Ops.Port = 0 },
		"bad name":              func(r *AccessRecord) { r.Devices[0].Name = "Laptop_1" },
		"duplicate name":        func(r *AccessRecord) { r.Devices[1].Name = "laptop" },
		"bad key":               func(r *AccessRecord) { r.Devices[0].PublicKey = "not-a-key" },
		"short key":             func(r *AccessRecord) { r.Devices[0].PublicKey = base64.StdEncoding.EncodeToString([]byte("short")) },
		"duplicate key":         func(r *AccessRecord) { r.Devices[1].PublicKey = r.Devices[0].PublicKey },
		"node key reused":       func(r *AccessRecord) { r.Devices[0].PublicKey = r.Ops.Nodes[0].PublicKey },
		"device address node":   func(r *AccessRecord) { r.Devices[0].Address = "10.99.0.5" },
		"device address out":    func(r *AccessRecord) { r.Devices[0].Address = "10.98.0.129" },
		"device address twice":  func(r *AccessRecord) { r.Devices[1].Address = "10.99.0.129" },
		"device beyond counter": func(r *AccessRecord) { r.Ops.NextDevice = 130 },
		"bad state":             func(r *AccessRecord) { r.Devices[0].State = "revoked" },
		"two pending":           func(r *AccessRecord) { r.Devices[0].State = DevicePending },
		"node address device":   func(r *AccessRecord) { r.Ops.Nodes[0].Address = "10.99.0.200" },
		"node address twice":    func(r *AccessRecord) { r.Ops.Nodes[1].Address = "10.99.0.1" },
		"node beyond counter":   func(r *AccessRecord) { r.Ops.NextNode = 2 },
		"node twice":            func(r *AccessRecord) { r.Ops.Nodes[1].MachineID = "m-server" },
		"node bad key":          func(r *AccessRecord) { r.Ops.Nodes[1].PublicKey = "x" },
		"key with a newline":    func(r *AccessRecord) { k := r.Devices[1].PublicKey; r.Devices[1].PublicKey = k[:20] + "\n" + k[20:] },
		"key with padding bits": func(r *AccessRecord) { r.Devices[1].PublicKey = paddingAlias(r.Devices[0].PublicKey) },
		"node network address":  func(r *AccessRecord) { r.Ops.Nodes[0].Address = "10.99.0.0" },
		"device half's base":    func(r *AccessRecord) { r.Devices[0].Address = "10.99.0.128" },
		"node past the half": func(r *AccessRecord) {
			r.Ops.NextNode = 128
			r.Ops.Nodes[1].Address = "10.99.0.127"
		},
		"device broadcast": func(r *AccessRecord) {
			r.Ops.NextDevice = 256
			r.Devices[1].Address = "10.99.0.255"
		},
		"node counter past the end":   func(r *AccessRecord) { r.Ops.NextNode = 128 },
		"device counter past the end": func(r *AccessRecord) { r.Ops.NextDevice = 256 },
		"device counter in node half": func(r *AccessRecord) { r.Devices = nil; r.Ops.NextDevice = 5 },
		"node counter negative":       func(r *AccessRecord) { r.Ops.Nodes = nil; r.Ops.NextNode = -1 },
	} {
		rec := sampleDeviceRecord()
		mutate(&rec)
		assert.Error(t, rec.validate(), name)
	}
}

// paddingAlias spells key with its unused low bits set, which a lenient
// base64 decoder reads as the same 32 bytes.
func paddingAlias(key string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	i := strings.IndexByte(alphabet, key[42])
	return key[:42] + string(alphabet[i|3]) + key[43:]
}

func TestTheCounterAtTheEndOfEachHalfIsValid(t *testing.T) {
	rec := sampleDeviceRecord()
	rec.Ops.NextNode = 127
	rec.Ops.NextDevice = 255
	assert.NoError(t, rec.validate(), "a counter one past the last host marks the half as used up")
	rec.Ops.NextNode, rec.Ops.NextDevice, rec.Ops.Nodes, rec.Devices = 0, 0, nil, nil
	assert.NoError(t, rec.validate(), "zero counters are a range nothing was handed out from")
}

func TestVersionThreeIsRefused(t *testing.T) {
	rec := sampleDeviceRecord()
	rec.Version = 3
	r := &routedRunner{routes: []route{{contains: "get configmap kipper-access", out: accessRecordJSON(t, rec)}}}
	_, present, err := ReadAccessRecord(r)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidAccessRecord))
	assert.True(t, present)
}

func TestWriteAccessRecordPicksTheVersionFromTheOpsState(t *testing.T) {
	written := func(rec AccessRecord) AccessRecord {
		t.Helper()
		r := &routedRunner{}
		require.NoError(t, WriteAccessRecord(r, rec))
		var patch struct {
			Data map[string]string `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(r.stdins[0]), &patch))
		var saved AccessRecord
		require.NoError(t, json.Unmarshal([]byte(patch.Data[accessRecordKey]), &saved))
		return saved
	}
	plain := sampleAccessRecord()
	plain.Version = 0
	assert.Equal(t, 1, written(plain).Version, "a cluster without devices keeps a record older kips read")
	assert.NotContains(t, mustJSON(t, written(plain)), "ops", "and carries no ops fields")

	devices := sampleDeviceRecord()
	devices.Version = 0
	assert.Equal(t, 2, written(devices).Version)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func TestAllocationNeverReusesAnAddress(t *testing.T) {
	ops := AccessOps{Range: "10.99.0.0/24", Port: 443, NextNode: 1, NextDevice: 129}
	a, err := ops.AllocateNode("m-1", testKey(1))
	require.NoError(t, err)
	assert.Equal(t, netip.MustParseAddr("10.99.0.1"), a)
	d, err := ops.AllocateDevice()
	require.NoError(t, err)
	assert.Equal(t, netip.MustParseAddr("10.99.0.129"), d)
	d2, err := ops.AllocateDevice()
	require.NoError(t, err)
	assert.Equal(t, netip.MustParseAddr("10.99.0.130"), d2, "a removed device's address stays used")
	assert.Equal(t, 131, ops.NextDevice)

	full := AccessOps{Range: "10.99.0.0/24", Port: 443, NextNode: 127, NextDevice: 255}
	_, err = full.AllocateDevice()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "126 device addresses")
	_, err = full.AllocateNode("m-2", testKey(2))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "126 node addresses")
}

func TestAllocateNodeKeepsAnExistingAddress(t *testing.T) {
	ops := AccessOps{Range: "10.99.0.0/24", Port: 443, NextNode: 2, NextDevice: 129,
		Nodes: []AccessOpsNode{{MachineID: "m-1", Address: "10.99.0.1", PublicKey: testKey(1)}}}
	a, err := ops.AllocateNode("m-1", testKey(7))
	require.NoError(t, err)
	assert.Equal(t, netip.MustParseAddr("10.99.0.1"), a)
	assert.Equal(t, testKey(7), ops.Nodes[0].PublicKey, "the key is updated in place")
	assert.Equal(t, 2, ops.NextNode)
}

func TestParseOpsRangeAcceptsOnlyACanonicalIPv4Slash24(t *testing.T) {
	p, err := ParseOpsRange("10.99.0.0/24")
	require.NoError(t, err)
	assert.Equal(t, "10.99.0.0/24", p.String())
	for _, bad := range []string{"10.99.0.0/25", "10.99.0.1/24", "fd00::/120", "nope", "0.0.0.0/24"} {
		_, err := ParseOpsRange(bad)
		assert.Error(t, err, bad)
	}
}

func TestDeviceRangeHalves(t *testing.T) {
	ops := AccessOps{Range: "10.99.0.0/24"}
	assert.Equal(t, "10.99.0.128/25", ops.DevicePrefix().String())
	assert.Equal(t, "10.99.0.0/25", ops.NodePrefix().String())
}

func TestOpsRangeOverlap(t *testing.T) {
	r := netip.MustParsePrefix("10.99.0.0/24")
	err := OpsRangeOverlap(r, map[string][]netip.Prefix{
		"the pod network": {netip.MustParsePrefix("10.42.0.0/16")},
		"listed prefixes": {netip.MustParsePrefix("10.99.0.128/32")},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "10.99.0.0/24 overlaps listed prefixes (10.99.0.128/32)")
	assert.Contains(t, err.Error(), "--range")
	assert.NoError(t, OpsRangeOverlap(r, map[string][]netip.Prefix{"the pod network": {netip.MustParsePrefix("10.42.0.0/16")}}))
}

func TestValidDeviceName(t *testing.T) {
	for _, ok := range []string{"laptop", "mac-2", "a"} {
		assert.NoError(t, ValidDeviceName(ok), ok)
	}
	for _, bad := range []string{"", "Laptop", "-x", "x-", "a_b", "a b", strings.Repeat("a", 64), "a#b"} {
		assert.Error(t, ValidDeviceName(bad), bad)
	}
}

func TestDeviceLookups(t *testing.T) {
	rec := sampleDeviceRecord()
	d, ok := rec.Device("desktop")
	require.True(t, ok)
	assert.Equal(t, DevicePending, d.State)
	_, ok = rec.Device("phone")
	assert.False(t, ok)
	assert.Equal(t, 1, rec.ActiveDevices())
	p, ok := rec.PendingDevice()
	require.True(t, ok)
	assert.Equal(t, "desktop", p.Name)
	k, ok := rec.DeviceByKey(testKey(9))
	require.True(t, ok)
	assert.Equal(t, "laptop", k.Name)
}
