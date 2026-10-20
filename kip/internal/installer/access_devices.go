package installer

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

// DevicePending and DeviceActive describe registration states. Pending peers
// are included in the node configuration during enrollment. Only active devices
// satisfy the access record's requirement for an operator access method.
const (
	DevicePending = "pending"
	DeviceActive  = "active"
)

const (
	// DefaultOpsRange is the default operator address range.
	DefaultOpsRange = "10.99.0.0/24"
	// DefaultOpsPort is the UDP port of the operator interface.
	DefaultOpsPort = 443
	// firstNodeHost and firstDeviceHost start the two halves of the range:
	// nodes take .1 to .126, devices .129 to .254.
	firstNodeHost   = 1
	lastNodeHost    = 126
	firstDeviceHost = 129
	lastDeviceHost  = 254
)

// AccessOps is the operator interface's shared state: the range, the UDP
// port, the address counters and each node's address and public key.
// Counters only grow, so an address is never handed out twice.
type AccessOps struct {
	Range      string          `json:"range"`
	Port       int             `json:"port"`
	NextNode   int             `json:"nextNode"`
	NextDevice int             `json:"nextDevice"`
	Nodes      []AccessOpsNode `json:"nodes,omitempty"`
}

// AccessOpsNode is one node's operator address and WireGuard public key.
type AccessOpsNode struct {
	MachineID string `json:"machineID"`
	Address   string `json:"address"`
	PublicKey string `json:"publicKey"`
}

// AccessDevice is one registered operator workstation.
type AccessDevice struct {
	Name      string `json:"name"`
	PublicKey string `json:"publicKey"`
	Address   string `json:"address"`
	State     string `json:"state"`
	AddedBy   string `json:"addedBy,omitempty"`
	AddedAt   string `json:"addedAt,omitempty"`
}

// ParseOpsRange reads an operator range, which must be a canonical IPv4 /24
// other than 0.0.0.0/24.
func ParseOpsRange(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() || p.Bits() != 24 || p.Masked() != p || p.Addr().IsUnspecified() {
		return netip.Prefix{}, fmt.Errorf("%q is not an IPv4 /24 such as %s", s, DefaultOpsRange)
	}
	return p, nil
}

func (o AccessOps) prefix() netip.Prefix {
	p, _ := ParseOpsRange(o.Range)
	return p
}

// host returns the nth address of the range, or the zero Addr outside it.
func (o AccessOps) host(n int) netip.Addr {
	if n < 0 || n > 255 {
		return netip.Addr{}
	}
	b := o.prefix().Addr().As4()
	b[3] = byte(n)
	return netip.AddrFrom4(b)
}

// NodePrefix returns the lower /25 for node addresses. Range must pass ParseOpsRange.
func (o AccessOps) NodePrefix() netip.Prefix {
	return netip.PrefixFrom(o.prefix().Addr(), 25)
}

// DevicePrefix returns the upper /25 for device addresses. Range must pass ParseOpsRange.
func (o AccessOps) DevicePrefix() netip.Prefix {
	return netip.PrefixFrom(o.host(128), 25)
}

// AllocateNode returns the node's operator address. A node already present
// keeps its address and receives the supplied public key. A new node takes the next
// unused address. Exhaustion is an error. Range must pass ParseOpsRange.
func (o *AccessOps) AllocateNode(machineID, key string) (netip.Addr, error) {
	for i, n := range o.Nodes {
		if n.MachineID == machineID {
			o.Nodes[i].PublicKey = key
			return netip.ParseAddr(n.Address)
		}
	}
	if o.NextNode < firstNodeHost {
		o.NextNode = firstNodeHost
	}
	if o.NextNode > lastNodeHost {
		return netip.Addr{}, fmt.Errorf("the operator range %s has handed out all %d node addresses. Pick a new range with 'kip cluster access reset' and 'device add --range'", o.Range, lastNodeHost-firstNodeHost+1)
	}
	a := o.host(o.NextNode)
	o.NextNode++
	o.Nodes = append(o.Nodes, AccessOpsNode{MachineID: machineID, Address: a.String(), PublicKey: key})
	return a, nil
}

// AllocateDevice returns the next unused device address and advances the counter.
// Exhaustion is an error. Range must pass ParseOpsRange.
func (o *AccessOps) AllocateDevice() (netip.Addr, error) {
	if o.NextDevice < firstDeviceHost {
		o.NextDevice = firstDeviceHost
	}
	if o.NextDevice > lastDeviceHost {
		return netip.Addr{}, fmt.Errorf("the operator range %s has handed out all %d device addresses. Pick a new range with 'kip cluster access reset' and 'device add --range'", o.Range, lastDeviceHost-firstDeviceHost+1)
	}
	a := o.host(o.NextDevice)
	o.NextDevice++
	return a, nil
}

// Node returns the ops state of the node with machineID.
func (o AccessOps) Node(machineID string) (AccessOpsNode, bool) {
	for _, n := range o.Nodes {
		if n.MachineID == machineID {
			return n, true
		}
	}
	return AccessOpsNode{}, false
}

// OpsRangeOverlap refuses a range that overlaps any of the named networks,
// naming the first one it overlaps in name order.
func OpsRangeOverlap(r netip.Prefix, nets map[string][]netip.Prefix) error {
	names := make([]string, 0, len(nets))
	for name := range nets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var hits []string
		for _, p := range nets[name] {
			if p.Overlaps(r) {
				hits = append(hits, p.String())
			}
		}
		if len(hits) > 0 {
			return fmt.Errorf("the operator range %s overlaps %s (%s). Pick another /24 with --range", r, name, strings.Join(hits, ", "))
		}
	}
	return nil
}

var deviceNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidDeviceName accepts a DNS label: lower-case letters, digits and inner
// hyphens, at most 63 characters.
func ValidDeviceName(name string) error {
	if !deviceNamePattern.MatchString(name) {
		return fmt.Errorf("%q is not a valid device name. Use lower-case letters, digits and hyphens, such as laptop or mac-2", name)
	}
	return nil
}

// ValidWireGuardKey accepts a 32-byte key in canonical base64, as wg prints
// it, so string equality reliably compares keys.
func ValidWireGuardKey(key string) error {
	b, err := base64.StdEncoding.Strict().DecodeString(key)
	if err != nil || len(b) != 32 || base64.StdEncoding.EncodeToString(b) != key {
		return errors.New("not a WireGuard key")
	}
	return nil
}

// Device returns the device called name.
func (r AccessRecord) Device(name string) (AccessDevice, bool) {
	for _, d := range r.Devices {
		if d.Name == name {
			return d, true
		}
	}
	return AccessDevice{}, false
}

// DeviceByKey returns the device with public key key.
func (r AccessRecord) DeviceByKey(key string) (AccessDevice, bool) {
	for _, d := range r.Devices {
		if d.PublicKey == key {
			return d, true
		}
	}
	return AccessDevice{}, false
}

// PendingDevice returns the first pending device, or a zero value and false.
// A valid access record contains at most one pending device.
func (r AccessRecord) PendingDevice() (AccessDevice, bool) {
	for _, d := range r.Devices {
		if d.State == DevicePending {
			return d, true
		}
	}
	return AccessDevice{}, false
}

// ActiveDevices counts registered devices that satisfy the access record's
// requirement for an operator access method.
func (r AccessRecord) ActiveDevices() int {
	n := 0
	for _, d := range r.Devices {
		if d.State == DeviceActive {
			n++
		}
	}
	return n
}

func (r AccessRecord) validateOps() error {
	o := r.Ops
	rng, err := ParseOpsRange(o.Range)
	if err != nil {
		return fmt.Errorf("its operator range: %w", err)
	}
	if o.Port < 1 || o.Port > 65535 {
		return fmt.Errorf("its operator port %d must be between 1 and 65535", o.Port)
	}
	// Zero marks an unused half of the range. One past the last host marks
	// an exhausted half.
	if o.NextNode != 0 && (o.NextNode < firstNodeHost || o.NextNode > lastNodeHost+1) {
		return fmt.Errorf("its node counter %d is outside the node half", o.NextNode)
	}
	if o.NextDevice != 0 && (o.NextDevice < firstDeviceHost || o.NextDevice > lastDeviceHost+1) {
		return fmt.Errorf("its device counter %d is outside the device half", o.NextDevice)
	}
	keys := map[string]string{}
	useKey := func(key, owner string) error {
		if err := ValidWireGuardKey(key); err != nil {
			return fmt.Errorf("%s has an invalid public key", owner)
		}
		if other, ok := keys[key]; ok {
			return fmt.Errorf("%s and %s share a public key", other, owner)
		}
		keys[key] = owner
		return nil
	}
	addrs := map[netip.Addr]string{}
	useAddr := func(s, owner string, first, last, next int) error {
		a, err := netip.ParseAddr(s)
		if err != nil || !rng.Contains(a) || int(a.As4()[3]) < first || int(a.As4()[3]) > last {
			return fmt.Errorf("%s has address %q outside %s to %s", owner, s, o.host(first), o.host(last))
		}
		if int(a.As4()[3]) >= next {
			return fmt.Errorf("%s has address %s, which the record has not handed out yet", owner, s)
		}
		if other, ok := addrs[a]; ok {
			return fmt.Errorf("%s and %s share address %s", other, owner, s)
		}
		addrs[a] = owner
		return nil
	}
	machines := map[string]bool{}
	for _, n := range o.Nodes {
		owner := "node " + n.MachineID
		if n.MachineID == "" || machines[n.MachineID] {
			return fmt.Errorf("it lists node %q twice or without a machine ID", n.MachineID)
		}
		machines[n.MachineID] = true
		if err := useKey(n.PublicKey, owner); err != nil {
			return err
		}
		if err := useAddr(n.Address, owner, firstNodeHost, lastNodeHost, o.NextNode); err != nil {
			return err
		}
	}
	names := map[string]bool{}
	pending := 0
	for _, d := range r.Devices {
		if err := ValidDeviceName(d.Name); err != nil {
			return err
		}
		if names[d.Name] {
			return fmt.Errorf("it lists device %s twice", d.Name)
		}
		names[d.Name] = true
		owner := "device " + d.Name
		if err := useKey(d.PublicKey, owner); err != nil {
			return err
		}
		if err := useAddr(d.Address, owner, firstDeviceHost, lastDeviceHost, o.NextDevice); err != nil {
			return err
		}
		switch d.State {
		case DevicePending:
			pending++
		case DeviceActive:
		default:
			return fmt.Errorf("device %s has state %q", d.Name, d.State)
		}
	}
	if pending > 1 {
		return errors.New("more than one device is pending")
	}
	return nil
}
