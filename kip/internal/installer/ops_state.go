package installer

import (
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// OpsHostState describes the node's operator interface using only public key
// material. Peers maps loaded public keys to allowed IPs. Chains maps live
// kipper_ops chains to hooks and rule counts. TableSum fingerprints nft's live
// table listing, and LoadedSum fingerprints the listing saved by ops.sh load.
// UFWRules preserves the order printed by 'ufw status' when UFW is active.
type OpsHostState struct {
	Dir       bool
	Enabled   bool
	Missing   []string
	Table     bool
	Up        bool
	PublicKey string
	Port      int
	Address   string
	MTU       int
	Routes    []string
	Peers     map[string]string
	Chains    map[string]OpsChain
	TableSum  string
	LoadedSum string
	UFW       bool
	UFWRules  []string
	Revoked   []string
}

// OpsChain is one chain of the kipper_ops table.
type OpsChain struct {
	Hook  string
	Rules int
}

// Present reports whether anything of the operator interface is on the node.
func (s OpsHostState) Present() bool {
	return s.Dir || s.Table || s.Up || s.PublicKey != ""
}

func opsStateCommand(root string) string {
	d := root + opsDir
	return `d=` + d + `
[ -d $d ] && echo dir=1
systemctl is-enabled --quiet ` + opsUnitName + ` 2>/dev/null && echo enabled=1
for f in ops.nft iface peers.conf kipops.key ops.sh; do [ -s $d/$f ] || echo "missing=$f"; done
if listed=$(nft list table ` + opsTable + ` 2>/dev/null); then
  echo table=1
  echo "tablesum=$(printf '%s\n' "$listed" | sha256sum | cut -c1-64)"
  echo "loadedsum=$(cat $d/table.sum 2>/dev/null)"
  printf '%s\n' "$listed" | awk '$1 == "chain" {c = $2; print "chain=" c} /hook / {for (i = 1; i < NF; i++) if ($i == "hook") print "hook=" c " " $(i+1)} $1 == "iifname" || $1 == "oifname" || $1 == "fib" {print "rule=" c}'
fi
if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
  echo ufw=1
  ufw status | awk 'p && NF {print "ufwrule=" $0} /^--/ {p = 1}'
fi
if ip link show ` + opsInterface + ` >/dev/null 2>&1; then
  ip -o link show ` + opsInterface + ` | grep -q '[<,]UP[,>]' && echo up=1
  ip -o link show ` + opsInterface + ` | awk '{for (i = 1; i < NF; i++) if ($i == "mtu") print "mtu=" $(i+1)}'
  ip -4 route show dev ` + opsInterface + ` | awk '{print "route=" $1}'
  echo "key=$(wg show ` + opsInterface + ` public-key 2>/dev/null)"
  echo "port=$(wg show ` + opsInterface + ` listen-port 2>/dev/null)"
  ip -o -4 address show dev ` + opsInterface + ` | awk '{print "address=" $4}'
  wg show ` + opsInterface + ` allowed-ips 2>/dev/null | sed 's/^/peer=/'
fi
sed 's/^/revoked=/' $d/revoked-keys 2>/dev/null
true`
}

// ReadOpsState reads the node's operator interface, its table and its
// console revocations. Remote command failures return an error. Individual
// missing or unreadable host values may appear as zero values in the result.
func ReadOpsState(client CommandRunner) (OpsHostState, error) {
	out, err := client.Run(opsStateCommand(""))
	if err != nil {
		return OpsHostState{}, fmt.Errorf("reading the operator interface: %w", err)
	}
	return parseOpsState(out), nil
}

func parseOpsState(out string) OpsHostState {
	st := OpsHostState{Peers: map[string]string{}, Chains: map[string]OpsChain{}}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "dir":
			st.Dir = true
		case "enabled":
			st.Enabled = true
		case "missing":
			st.Missing = append(st.Missing, v)
		case "table":
			st.Table = true
		case "up":
			st.Up = true
		case "key":
			st.PublicKey = v
		case "port":
			st.Port, _ = strconv.Atoi(v)
		case "address":
			st.Address = v
		case "mtu":
			st.MTU, _ = strconv.Atoi(v)
		case "route":
			st.Routes = append(st.Routes, v)
		case "chain":
			if _, ok := st.Chains[v]; !ok {
				st.Chains[v] = OpsChain{}
			}
		case "hook":
			if c, hook, ok := strings.Cut(v, " "); ok {
				ch := st.Chains[c]
				ch.Hook = hook
				st.Chains[c] = ch
			}
		case "rule":
			ch := st.Chains[v]
			ch.Rules++
			st.Chains[v] = ch
		case "ufw":
			st.UFW = true
		case "tablesum":
			st.TableSum = v
		case "loadedsum":
			st.LoadedSum = v
		case "ufwrule":
			st.UFWRules = append(st.UFWRules, v)
		case "peer":
			if f := strings.Fields(v); len(f) > 0 {
				st.Peers[f[0]] = strings.Join(f[1:], " ")
			}
		case "revoked":
			if ValidWireGuardKey(v) == nil {
				st.Revoked = append(st.Revoked, v)
			}
		}
	}
	return st
}

// OpsDrift compares the node machineID's operator interface st with rec and
// returns diagnostic messages, or nil when no differences are found. When
// rec has no operator configuration, it reports only a leftover interface.
func OpsDrift(st OpsHostState, rec AccessRecord, machineID string) []string {
	if rec.Ops == nil {
		if st.Present() {
			return []string{"an operator interface is left on this node although the record has no devices. Run 'kip cluster access reset --yes' to remove it"}
		}
		return nil
	}
	on, ok := rec.Ops.Node(machineID)
	if !ok {
		return []string{"has no operator address in the record. Run 'kip cluster access apply'"}
	}
	var drift []string
	if st.Dir {
		if !st.Enabled {
			drift = append(drift, "kipper-ops.service is not enabled, so the operator interface would not come back after a reboot. Run 'kip cluster access apply'")
		}
		for _, f := range st.Missing {
			drift = append(drift, fmt.Sprintf("%s/%s, which the interface loads at boot, is missing or empty. Run 'kip cluster access apply'", opsDir, f))
		}
	}
	if !st.Table {
		drift = append(drift, "the kipper_ops table is not loaded")
	} else if chains := opsChainDrift(st.Chains); len(chains) > 0 {
		drift = append(drift, chains...)
	} else if st.TableSum == "" || st.TableSum != st.LoadedSum {
		drift = append(drift, "the kipper_ops table differs from the one kip loaded. Run 'kip cluster access apply'")
	}
	if st.UFW {
		if d := ufwDrift(st.UFWRules, rec.Ops.Port); d != "" {
			drift = append(drift, d)
		}
	}
	if !st.Up {
		drift = append(drift, "the operator interface kipops is down")
		return drift
	}
	if st.PublicKey != on.PublicKey {
		drift = append(drift, "the operator interface's key is not the recorded one. If the host was reinstalled, run 'kip cluster access device repin' from each device")
	}
	if st.Port != rec.Ops.Port {
		drift = append(drift, fmt.Sprintf("the operator interface listens on UDP %d, not the recorded %d", st.Port, rec.Ops.Port))
	}
	if st.Address != on.Address+"/32" {
		drift = append(drift, fmt.Sprintf("the operator interface has address %q, not the recorded %s", st.Address, on.Address))
	}
	if st.MTU != opsMTU {
		drift = append(drift, fmt.Sprintf("the operator interface has MTU %d, not %d", st.MTU, opsMTU))
	}
	if route := rec.Ops.DevicePrefix().String(); !slices.Contains(st.Routes, route) {
		drift = append(drift, fmt.Sprintf("the operator interface has no route to the devices' %s", route))
	}
	revoked := map[string]bool{}
	for _, k := range st.Revoked {
		revoked[k] = true
	}
	want := map[string]bool{}
	for _, d := range rec.Devices {
		if revoked[d.PublicKey] {
			drift = append(drift, fmt.Sprintf("device %[1]s was revoked here from the console. Remove it with 'kip cluster access device remove %[1]s'", d.Name))
			continue
		}
		want[d.PublicKey] = true
		label := "device " + d.Name
		if d.State == DevicePending {
			label += " (pending)"
		}
		got, ok := st.Peers[d.PublicKey]
		switch {
		case !ok:
			drift = append(drift, label+" is not loaded")
		case got != d.Address+"/32":
			drift = append(drift, fmt.Sprintf("%s is loaded with allowed IPs %q, not %s/32", label, got, d.Address))
		}
	}
	var extra []string
	for k := range st.Peers {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		drift = append(drift, fmt.Sprintf("peer %s is loaded but not recorded. Run 'kip cluster access apply'", k))
	}
	return drift
}

// ufwDrift checks UFW's inbound IPv4 rules in order, as UFW applies the
// first that matches: kip's rule allowing UDP on port from anywhere must come
// before any deny, reject or limit rule that may match it. Allow rules never
// block, so they are skipped. It returns the difference, or "".
func ufwDrift(rules []string, port int) string {
	want := fmt.Sprintf("%d/udp", port)
	for _, line := range rules {
		to, action, from, ok := parseUFWRule(line)
		if !ok {
			continue
		}
		if action == "ALLOW" {
			if to == want && from == "Anywhere" {
				return ""
			}
			continue
		}
		if !ufwMisses(to, port) {
			return fmt.Sprintf("UFW rule %q comes before the tunnel's allow and may block UDP %d", strings.Join(strings.Fields(line), " "), port)
		}
	}
	return fmt.Sprintf("UFW does not allow the tunnel's UDP port %d from every address", port)
}

// parseUFWRule splits a 'ufw status' row into its destination, action and
// source. It reports false for IPv6, outgoing and forwarded rules and for
// rows it cannot read.
func parseUFWRule(line string) (to, action, from string, ok bool) {
	line, _, _ = strings.Cut(line, " # ")
	f := strings.Fields(line)
	for i, w := range f {
		switch w {
		case "ALLOW", "DENY", "REJECT", "LIMIT":
		default:
			continue
		}
		rest := f[i+1:]
		if len(rest) > 0 && (rest[0] == "IN" || rest[0] == "OUT" || rest[0] == "FWD") {
			if rest[0] != "IN" {
				return "", "", "", false
			}
			rest = rest[1:]
		}
		to, from = strings.Join(f[:i], " "), strings.Join(rest, " ")
		if i == 0 || strings.Contains(to, "(v6)") || strings.Contains(from, "(v6)") {
			return "", "", "", false
		}
		return to, w, from, true
	}
	return "", "", "", false
}

// ufwMisses reports whether a rule's destination to clearly names other
// ports or another protocol than UDP port. Anything else may match it.
func ufwMisses(to string, port int) bool {
	f := strings.Fields(to)
	ports, proto, _ := strings.Cut(f[len(f)-1], "/")
	covers := false
	for _, p := range strings.Split(ports, ",") {
		lo, hi, isRange := strings.Cut(p, ":")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return false
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				return false
			}
		}
		covers = covers || (port >= a && port <= b)
	}
	return !covers || (proto != "" && proto != "udp")
}

// opsChainDrift compares the live kipper_ops chains with the ones
// RenderOpsTable writes: the same hooks and the same number of rules.
func opsChainDrift(live map[string]OpsChain) []string {
	var drift []string
	want := renderedOpsChains()
	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w, got := want[name], live[name]
		switch _, ok := live[name]; {
		case !ok:
			drift = append(drift, fmt.Sprintf("the kipper_ops table has no %s chain", name))
		case got.Hook != w.Hook:
			drift = append(drift, fmt.Sprintf("the kipper_ops %s chain hooks %s, not %s", name, got.Hook, w.Hook))
		case got.Rules != w.Rules:
			drift = append(drift, fmt.Sprintf("the kipper_ops %s chain holds %d rules, not %d", name, got.Rules, w.Rules))
		}
	}
	return drift
}

// renderedOpsChains reads the chains, hooks and rule counts from the table
// RenderOpsTable writes, so the check follows the renderer.
func renderedOpsChains() map[string]OpsChain {
	table, _ := RenderOpsTable(OpsNodeIntent{Address: netip.MustParseAddr("10.99.0.1"), Devices: netip.MustParsePrefix("10.99.0.128/25"), SSHPorts: []int{22}, Port: 443})
	chains := map[string]OpsChain{}
	c := ""
	for _, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "chain":
			c = f[1]
			chains[c] = OpsChain{}
		case "type":
			if i := slices.Index(f, "hook"); i >= 0 && i+1 < len(f) {
				ch := chains[c]
				ch.Hook = f[i+1]
				chains[c] = ch
			}
		case "iifname", "oifname", "fib":
			ch := chains[c]
			ch.Rules++
			chains[c] = ch
		}
	}
	return chains
}
