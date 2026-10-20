package installer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// NodeNetwork holds a node's addresses and pod traffic interface.
type NodeNetwork struct {
	NodeIP     string
	ExternalIP string
	Interface  string
	TunnelMTU  int
}

// NetworkPlan describes the network mode and addresses for the server and
// joining host. PodNetwork is the cluster's pod CIDR, set in public mode.
type NetworkPlan struct {
	Mode       string
	Server     NodeNetwork
	Joiner     NodeNetwork
	PodNetwork string
}

// NetworkModeOptions constrains network selection for a joining host.
type NetworkModeOptions struct {
	PrivateNetwork   string
	Public           bool
	ClusterMode      string
	ServerExternalIP string
	JoinerExternalIP string
	SSHPort          string
}

// excludedInterface filters loopback and common pod and container interfaces.
func excludedInterface(name string) bool {
	return name == "lo" || name == "cni0" || name == "docker0" ||
		strings.HasPrefix(name, "flannel") || strings.HasPrefix(name, "veth")
}

type hostAddress struct {
	iface  string
	prefix netip.Prefix
}

// parseIPAddrs parses ip -o -4 addr show output, skipping excluded interfaces.
// It strips the parent suffix from VLAN names such as enp7s0.4000@enp7s0.
func parseIPAddrs(out string) []hostAddress {
	var addrs []hostAddress
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[2] != "inet" {
			continue
		}
		iface, _, _ := strings.Cut(fields[1], "@")
		prefix, err := netip.ParsePrefix(fields[3])
		if err != nil || excludedInterface(iface) || !interfaceNamePattern.MatchString(iface) {
			continue
		}
		addrs = append(addrs, hostAddress{iface: iface, prefix: prefix})
	}
	return addrs
}

type routeLookup struct {
	iface string
	src   netip.Addr
}

// parseRouteGet extracts the interface and source address from ip route output.
// Local routes are excluded because they cannot connect two hosts.
func parseRouteGet(out string) (routeLookup, bool) {
	fields := strings.Fields(out)
	var r routeLookup
	for i := 0; i+1 < len(fields); i++ {
		switch fields[i] {
		case "dev":
			r.iface = fields[i+1]
		case "src":
			if a, err := netip.ParseAddr(fields[i+1]); err == nil {
				r.src = a
			}
		}
	}
	if len(fields) > 0 && fields[0] == "local" {
		return routeLookup{}, false
	}
	return r, r.iface != "" && r.src.IsValid()
}

func routeTo(client CommandRunner, addr netip.Addr) (routeLookup, bool) {
	out, err := client.Run("ip -o route get " + addr.String() + " 2>/dev/null")
	if err != nil {
		return routeLookup{}, false
	}
	return parseRouteGet(out)
}

// defaultRoute returns the interface and source address of the first IPv4 default route.
func defaultRoute(client CommandRunner) (routeLookup, error) {
	out, err := client.Run("ip -o -4 route show default 2>/dev/null | head -n 1")
	if err != nil {
		return routeLookup{}, fmt.Errorf("reading the default route: %w", err)
	}
	r, _ := parseRouteGet(out)
	if r.iface == "" {
		return routeLookup{}, fmt.Errorf("the host has no IPv4 default route")
	}
	// Ask the kernel to select a source address without sending traffic.
	if !r.src.IsValid() {
		if picked, ok := routeTo(client, netip.MustParseAddr("192.0.2.1")); ok && picked.iface == r.iface {
			r.src = picked.src
		} else {
			return routeLookup{}, fmt.Errorf("the default route through %s has no source address", r.iface)
		}
	}
	return r, nil
}

func hostAddresses(client CommandRunner) ([]hostAddress, error) {
	out, err := client.Run("ip -o -4 addr show")
	if err != nil {
		return nil, fmt.Errorf("listing the host's addresses: %w", err)
	}
	return parseIPAddrs(out), nil
}

// tcpReachable probes addr:port from the host using its routing table.
func tcpReachable(from CommandRunner, addr netip.Addr, port string) bool {
	_, err := from.Run(fmt.Sprintf("timeout 5 bash -c '</dev/tcp/%s/%s' 2>/dev/null", addr, port))
	return err == nil
}

// PlanNodeNetwork selects addresses and a mode consistent with opts.ClusterMode.
// Private mode requires local RFC 1918 addresses, SSH access to the joiner,
// and API access to the server. Automatic selection excludes default-route
// interfaces. An explicit private network allows them. With no cluster mode set,
// automatic selection falls back to public mode only when no private route
// qualifies for the TCP checks. Before falling back, it checks for a shared
// default-route network and returns an error asking for an explicit network
// choice if that network passes the private path checks.
// Public mode requires external IPv4 addresses and API access to the server.
// It resolves the cluster pod CIDR and caps each host's backend MTU at 1360 bytes,
// rejecting interface MTUs below 1280 bytes. Invalid options or failed network
// checks return an error before either host is changed.
func PlanNodeNetwork(server, joiner CommandRunner, opts NetworkModeOptions) (NetworkPlan, error) {
	var private netip.Prefix
	if opts.PrivateNetwork != "" {
		p, err := netip.ParsePrefix(opts.PrivateNetwork)
		if err != nil || !p.Addr().Is4() {
			return NetworkPlan{}, fmt.Errorf("--private-network %q is not an IPv4 network in CIDR form", opts.PrivateNetwork)
		}
		private = p.Masked()
	}
	if opts.Public && private.IsValid() {
		return NetworkPlan{}, fmt.Errorf("--public and --private-network cannot be combined")
	}
	if opts.ClusterMode == NetworkModePrivate && opts.Public {
		return NetworkPlan{}, fmt.Errorf("the cluster runs in private mode, so a node cannot join it with --public")
	}
	if opts.ClusterMode == NetworkModePublic && private.IsValid() {
		return NetworkPlan{}, fmt.Errorf("the cluster runs in public mode, so a node cannot join it with --private-network")
	}

	if !opts.Public && opts.ClusterMode != NetworkModePublic {
		plan, found, unreachable, err := privatePath(server, joiner, private, opts)
		if err != nil {
			return NetworkPlan{}, err
		}
		if found {
			return plan, nil
		}
		if opts.ClusterMode == NetworkModePrivate {
			return NetworkPlan{}, fmt.Errorf("no private path found%s between the server and the new host. Because the cluster runs in private mode, every node must join over its private network. Attach the new host to the cluster's private network, then check that the server can reach the new host over SSH and the new host can reach the server's API on port 6443 over that network%s", inNetwork(private), failedLeg(unreachable))
		}
		if private.IsValid() {
			return NetworkPlan{}, fmt.Errorf("no private path found%s between the server and the new host. Check private network connectivity, SSH access to the new host, and API access to the server on port 6443%s", inNetwork(private), failedLeg(unreachable))
		}
		// Later joins reuse this mode, so switching to public addresses requires
		// an explicit choice when a private route fails its TCP checks.
		if unreachable != "" {
			return NetworkPlan{}, fmt.Errorf("kip found a private route between the hosts, but %s. Open that port on the private network and re-run, or add --public to join over public addresses instead; later nodes will also use public addresses", unreachable)
		}
		if shared, ok := sharedDefaultNetwork(server, joiner, opts); ok {
			return NetworkPlan{}, fmt.Errorf("the hosts reach each other over the private network %s on their default network interfaces. Using this network requires an explicit choice. Re-run with --private-network %s to join over it, or with --public to join over public addresses. Later nodes will use the same mode", shared, shared)
		}
	}
	return publicPlan(server, joiner, opts)
}

// sharedDefaultNetwork checks the smallest /24, /16 or /8 prefix containing both
// hosts' private IPv4 default-route source addresses. It returns that prefix only
// if privatePath finds a usable path within it. Lookup or path-check failures
// return false so automatic selection can continue to public mode.
func sharedDefaultNetwork(server, joiner CommandRunner, opts NetworkModeOptions) (netip.Prefix, bool) {
	serverDefault, err := defaultRoute(server)
	if err != nil {
		return netip.Prefix{}, false
	}
	joinerDefault, err := defaultRoute(joiner)
	if err != nil {
		return netip.Prefix{}, false
	}
	a, b := serverDefault.src, joinerDefault.src
	if !a.Is4() || !b.Is4() || !a.IsPrivate() || !b.IsPrivate() {
		return netip.Prefix{}, false
	}
	for _, bits := range []int{24, 16, 8} {
		p := netip.PrefixFrom(a, bits).Masked()
		if !p.Contains(b) {
			continue
		}
		if _, found, _, err := privatePath(server, joiner, p, opts); err == nil && found {
			return p, true
		}
		return netip.Prefix{}, false
	}
	return netip.Prefix{}, false
}

func failedLeg(unreachable string) string {
	if unreachable == "" {
		return ""
	}
	return " (" + unreachable + ")"
}

func inNetwork(p netip.Prefix) string {
	if !p.IsValid() {
		return ""
	}
	return " in " + p.String()
}

// privatePath returns the first qualifying private path. If none passes the TCP
// checks, the third result describes the first failed connection, when present.
func privatePath(server, joiner CommandRunner, private netip.Prefix, opts NetworkModeOptions) (NetworkPlan, bool, string, error) {
	unreachable := ""
	joinerAddrs, err := hostAddresses(joiner)
	if err != nil {
		return NetworkPlan{}, false, "", err
	}
	serverDefault, err := defaultRoute(server)
	if err != nil {
		return NetworkPlan{}, false, "", fmt.Errorf("on the server: %w", err)
	}
	joinerDefault, err := defaultRoute(joiner)
	if err != nil {
		return NetworkPlan{}, false, "", fmt.Errorf("on the new host: %w", err)
	}
	serverAddrs, err := hostAddresses(server)
	if err != nil {
		return NetworkPlan{}, false, "", err
	}
	sshPort := opts.SSHPort
	if sshPort == "" {
		sshPort = "22"
	}
	if n, err := strconv.Atoi(sshPort); err != nil || !sshPortPattern.MatchString(sshPort) || n > 65535 {
		return NetworkPlan{}, false, "", fmt.Errorf("invalid SSH port %q; use a number from 1 to 65535", sshPort)
	}

	usable := func(addr netip.Addr, dev string, defaultDev string, own []hostAddress) bool {
		if !addr.IsPrivate() || !addr.Is4() || excludedInterface(dev) {
			return false
		}
		if private.IsValid() {
			if !private.Contains(addr) {
				return false
			}
		} else if dev == defaultDev {
			return false
		}
		for _, a := range own {
			if a.iface == dev && a.prefix.Addr() == addr {
				return true
			}
		}
		return false
	}

	for _, ja := range joinerAddrs {
		addr := ja.prefix.Addr()
		if !addr.IsPrivate() || (private.IsValid() && !private.Contains(addr)) {
			continue
		}
		toJoiner, ok := routeTo(server, addr)
		if !ok || !usable(toJoiner.src, toJoiner.iface, serverDefault.iface, serverAddrs) {
			continue
		}
		toServer, ok := routeTo(joiner, toJoiner.src)
		if !ok || toServer.src != addr || !usable(addr, toServer.iface, joinerDefault.iface, joinerAddrs) {
			continue
		}
		if !tcpReachable(server, addr, sshPort) {
			if unreachable == "" {
				unreachable = fmt.Sprintf("the server cannot reach the new host's SSH port at %s:%s", addr, sshPort)
			}
			continue
		}
		if !tcpReachable(joiner, toJoiner.src, "6443") {
			if unreachable == "" {
				unreachable = fmt.Sprintf("the new host cannot reach the server's API at %s:6443", toJoiner.src)
			}
			continue
		}
		plan := NetworkPlan{
			Mode:   NetworkModePrivate,
			Server: NodeNetwork{NodeIP: toJoiner.src.String(), ExternalIP: opts.ServerExternalIP, Interface: toJoiner.iface},
			Joiner: NodeNetwork{NodeIP: addr.String(), ExternalIP: opts.JoinerExternalIP, Interface: toServer.iface},
		}
		if err := refuseUnderlayOverlap(server, joiner, plan); err != nil {
			return NetworkPlan{}, false, "", err
		}
		return plan, true, "", nil
	}
	return NetworkPlan{}, false, unreachable, nil
}

func publicPlan(server, joiner CommandRunner, opts NetworkModeOptions) (NetworkPlan, error) {
	serverDefault, err := defaultRoute(server)
	if err != nil {
		return NetworkPlan{}, fmt.Errorf("on the server: %w", err)
	}
	joinerDefault, err := defaultRoute(joiner)
	if err != nil {
		return NetworkPlan{}, fmt.Errorf("on the new host: %w", err)
	}
	for _, ip := range []string{opts.ServerExternalIP, opts.JoinerExternalIP} {
		if a, err := netip.ParseAddr(ip); err != nil || !a.Is4() {
			return NetworkPlan{}, fmt.Errorf("public mode requires an external IPv4 address for each host; got %q", ip)
		}
	}
	if !tcpReachable(joiner, netip.MustParseAddr(opts.ServerExternalIP), "6443") {
		return NetworkPlan{}, fmt.Errorf("the new host cannot reach the server's API at %s:6443; allow it in the provider's firewall", opts.ServerExternalIP)
	}
	serverMTU, err := tunnelMTU(server, serverDefault.iface)
	if err != nil {
		return NetworkPlan{}, fmt.Errorf("on the server: %w", err)
	}
	joinerMTU, err := tunnelMTU(joiner, joinerDefault.iface)
	if err != nil {
		return NetworkPlan{}, fmt.Errorf("on the new host: %w", err)
	}
	clusterNets, err := clusterNetworks(server)
	if err != nil {
		return NetworkPlan{}, err
	}
	return NetworkPlan{
		Mode:       NetworkModePublic,
		Server:     NodeNetwork{NodeIP: serverDefault.src.String(), ExternalIP: opts.ServerExternalIP, TunnelMTU: serverMTU},
		Joiner:     NodeNetwork{NodeIP: joinerDefault.src.String(), ExternalIP: opts.JoinerExternalIP, TunnelMTU: joinerMTU},
		PodNetwork: clusterNets["pod"].String(),
	}, nil
}

// publicTunnelMTU is the largest MTU flannel's WireGuard backend gets in public
// mode, in bytes. At this cap, flannel subtracts 80 bytes of overhead to leave
// a 1280-byte tunnel MTU, matching the IPv6 minimum and allowing room for
// internet paths with an MTU below 1500 bytes.
const publicTunnelMTU = 1360

// minTunnelMTU is the smallest backend MTU kip accepts in public mode, in bytes.
const minTunnelMTU = 1280

// tunnelMTU reads iface's MTU in bytes and caps it at publicTunnelMTU.
// It rejects values below minTunnelMTU. Flannel otherwise derives the WireGuard
// MTU from the interface, so a 9001-byte cloud interface can produce packets
// that require fragmentation on paths between providers.
func tunnelMTU(client CommandRunner, iface string) (int, error) {
	if !interfaceNamePattern.MatchString(iface) {
		return 0, fmt.Errorf("the default route uses interface %q, which kip cannot read", iface)
	}
	out, err := client.Run("cat /sys/class/net/" + iface + "/mtu")
	if err != nil {
		return 0, fmt.Errorf("reading the MTU of %s: %w", iface, err)
	}
	mtu, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || mtu <= 0 {
		return 0, fmt.Errorf("reading the MTU of %s: got %q", iface, strings.TrimSpace(out))
	}
	if mtu < minTunnelMTU {
		return 0, fmt.Errorf("%s has an MTU of %d, below the %d bytes public mode needs for the WireGuard tunnel", iface, mtu, minTunnelMTU)
	}
	return min(mtu, publicTunnelMTU), nil
}

// refuseUnderlayOverlap checks both private interfaces' address networks and
// non-default route destinations against the cluster's pod and Service networks.
func refuseUnderlayOverlap(server, joiner CommandRunner, plan NetworkPlan) error {
	clusterNets, err := clusterNetworks(server)
	if err != nil {
		return err
	}
	var underlay []netip.Prefix
	for _, side := range []struct {
		client CommandRunner
		iface  string
	}{{server, plan.Server.Interface}, {joiner, plan.Joiner.Interface}} {
		nets, err := interfaceNetworks(side.client, side.iface)
		if err != nil {
			return err
		}
		underlay = append(underlay, nets...)
	}
	for _, u := range underlay {
		for name, c := range clusterNets {
			if u.Overlaps(c) {
				return fmt.Errorf("the private network %s overlaps the cluster's %s network %s, so traffic between nodes would be misrouted; use another private network or join with --public", u, name, c)
			}
		}
	}
	return nil
}

// interfaceNetworks lists address networks and non-default route destinations on iface.
func interfaceNetworks(client CommandRunner, iface string) ([]netip.Prefix, error) {
	addrs, err := hostAddresses(client)
	if err != nil {
		return nil, err
	}
	var nets []netip.Prefix
	for _, a := range addrs {
		if a.iface == iface {
			nets = append(nets, a.prefix.Masked())
		}
	}
	out, err := client.Run("ip -o -4 route show dev " + iface)
	if err != nil {
		return nil, fmt.Errorf("listing the routes on %s: %w", iface, err)
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "default" {
			continue
		}
		dest := fields[0]
		if !strings.Contains(dest, "/") {
			dest += "/32"
		}
		if p, err := netip.ParsePrefix(dest); err == nil {
			nets = append(nets, p.Masked())
		}
	}
	return nets, nil
}

// clusterNetworks resolves pod and Service CIDRs from k3s configuration and
// defaults, then checks that they contain the server's reported pod ranges
// and the kubernetes Service address.
func clusterNetworks(server CommandRunner) (map[string]netip.Prefix, error) {
	// Omit Run errors from config and unit reads because either may include
	// captured output containing a token.
	files, err := server.Run(configFilesCommand("/etc/rancher/k3s/config.yaml /etc/rancher/k3s/config.yaml.d/*.yaml", ""))
	if err != nil {
		return nil, errors.New("reading the server's k3s configuration failed")
	}
	unit, err := server.Run(k3sUnitSettingsCommand)
	if err != nil {
		return nil, errors.New("reading the server's k3s unit failed")
	}
	settings, err := cidrSettings(files, unit)
	if err != nil {
		return nil, err
	}
	pod, err := settingOrDefault(settings, "cluster-cidr", k3sPodCIDR)
	if err != nil {
		return nil, err
	}
	service, err := settingOrDefault(settings, "service-cidr", k3sServiceCIDR)
	if err != nil {
		return nil, err
	}

	out, err := server.Run(`kubectl get nodes -l ` + ServerNodeLabel + `=true -o jsonpath='{.items[*].spec.podCIDR}' 2>/dev/null`)
	if err != nil {
		return nil, fmt.Errorf("reading the cluster's pod network: %w", err)
	}
	ranges := strings.Fields(out)
	if len(ranges) == 0 {
		return nil, fmt.Errorf("the server reports no pod range, so kip cannot determine the cluster networks")
	}
	for _, f := range ranges {
		p, err := netip.ParsePrefix(f)
		if err != nil || p.Bits() < pod.Bits() || !pod.Contains(p.Addr()) {
			return nil, fmt.Errorf("the server's pod range %q is not inside %s, so kip cannot determine the cluster networks", f, pod)
		}
	}
	out, err = server.Run(`kubectl get service kubernetes -n default -o jsonpath='{.spec.clusterIP}' 2>/dev/null`)
	if err != nil {
		return nil, fmt.Errorf("reading the cluster's Service network: %w", err)
	}
	if a, err := netip.ParseAddr(strings.TrimSpace(out)); err != nil || !service.Contains(a) {
		return nil, fmt.Errorf("the kubernetes Service address %q is outside %s, so kip cannot determine the cluster networks", strings.TrimSpace(out), service)
	}
	return map[string]netip.Prefix{"pod": pod, "Service": service}, nil
}

// k3sUnitSettingsCommand prints ExecStart, Environment, and lines containing
// K3S_CONFIG_FILE from existing environment files. Filtering file contents
// avoids exposing unrelated entries such as tokens; read failures are errors.
const k3sUnitSettingsCommand = `set -e
systemctl show k3s -p ExecStart -p Environment --value
files=$(systemctl show k3s -p EnvironmentFiles --value)
printf '%s\n' "$files" | awk '{gsub(/ \(ignore_errors=[a-z]+\)/, "\n"); printf "%s", $0}' | while IFS= read -r f; do
  f=${f# }
  [ -n "$f" ] || continue
  if [ -e "$f" ]; then rc=0; grep -h K3S_CONFIG_FILE "$f" || rc=$?; [ "$rc" -le 1 ] || exit 1; fi
done`

type configFile struct {
	path string
	doc  map[string]any
}

// configFilesCommand builds a shell loop over paths, a trusted shell glob list.
// Each existing file except skip is prefixed with "==> path"; read failures abort.
func configFilesCommand(paths, skip string) string {
	return fmt.Sprintf(`for f in %s; do if [ -e "$f" ] && [ "$f" != %s ]; then echo "==> $f"; cat "$f" || exit 1; fi; done`, paths, shellQuote(skip))
}

// parseConfigFiles decodes the delimited YAML documents from configFilesCommand.
func parseConfigFiles(out string) ([]configFile, error) {
	var files []configFile
	for _, part := range strings.Split(out, "==> ")[1:] {
		path, body, _ := strings.Cut(part, "\n")
		path = strings.TrimSpace(path)
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
			return nil, fmt.Errorf("%s is not a YAML mapping: %w", path, err)
		}
		files = append(files, configFile{path: path, doc: doc})
	}
	return files, nil
}

// cidrSettings collects cluster-cidr and service-cidr values from YAML files
// delimited by "==> path" lines and from unit arguments. It rejects custom
// configuration paths and unrecognized CIDR settings to avoid assuming defaults.
func cidrSettings(files, unit string) (map[string][]string, error) {
	settings := map[string][]string{}
	docs, err := parseConfigFiles(files)
	if err != nil {
		return nil, fmt.Errorf("%w, so kip cannot determine the cluster networks", err)
	}
	for _, d := range docs {
		path, doc := d.path, d.doc
		for key, value := range doc {
			name := strings.TrimSuffix(key, "+")
			if name != "cluster-cidr" && name != "service-cidr" {
				continue
			}
			switch v := value.(type) {
			case string:
				settings[name] = append(settings[name], v)
			case []any:
				for _, item := range v {
					settings[name] = append(settings[name], fmt.Sprint(item))
				}
			default:
				return nil, fmt.Errorf("%s sets %s to %v, which kip cannot parse", path, key, value)
			}
		}
	}
	args := strings.Fields(unit)
	for i, arg := range args {
		if arg == "-c" || arg == "--config" || strings.HasPrefix(arg, "--config=") || strings.Contains(arg, "K3S_CONFIG_FILE") {
			return nil, fmt.Errorf("the k3s unit selects its own configuration file (%s), so kip cannot determine the cluster networks", arg)
		}
		for _, name := range []string{"cluster-cidr", "service-cidr"} {
			switch {
			case arg == "--"+name && i+1 < len(args):
				settings[name] = append(settings[name], args[i+1])
			case strings.HasPrefix(arg, "--"+name+"="):
				settings[name] = append(settings[name], strings.TrimPrefix(arg, "--"+name+"="))
			case strings.Contains(arg, name):
				return nil, fmt.Errorf("the k3s unit passes %q, which kip cannot parse; unable to determine the cluster networks", arg)
			}
		}
	}
	return settings, nil
}

// settingOrDefault requires a single distinct IPv4 CIDR, using def when unset.
func settingOrDefault(settings map[string][]string, name, def string) (netip.Prefix, error) {
	values := settings[name]
	if len(values) == 0 {
		values = []string{def}
	}
	var nets []string
	for _, v := range values {
		for _, n := range strings.Split(v, ",") {
			if n = strings.TrimSpace(n); n != "" && !containsString(nets, n) {
				nets = append(nets, n)
			}
		}
	}
	if len(nets) != 1 {
		return netip.Prefix{}, fmt.Errorf("the server's k3s configuration sets %s to %s, which is not one network", name, strings.Join(nets, ", "))
	}
	p, err := netip.ParsePrefix(nets[0])
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("the server's %s %q is not an IPv4 network", name, nets[0])
	}
	return p.Masked(), nil
}

// networkDropInKeys are the settings kip owns in kipper-network.yaml.
var networkDropInKeys = []string{"node-ip", "node-external-ip", "flannel-iface", "advertise-address", "flannel-external-ip", "flannel-conf", "tls-san+"}

// RenderNetworkDropIn replaces kip-owned settings for the node's role and mode,
// preserving other keys from existing. Both node addresses must be IPv4, and
// private mode requires a valid interface name. Servers advertise the API on
// NodeIP in private mode and ExternalIP in public mode. In public mode, a positive
// TunnelMTU selects the file written by WriteFlannelConf. Callers must write
// that file separately. Invalid YAML, addresses, interfaces or modes return an error.
func RenderNetworkDropIn(existing string, role HostRole, mode string, n NodeNetwork) (string, error) {
	var doc map[string]any
	if strings.TrimSpace(existing) != "" {
		if err := yaml.Unmarshal([]byte(existing), &doc); err != nil {
			return "", fmt.Errorf("%s is not a YAML mapping, unable to render network settings: %w", k3sNetworkConfigPath, err)
		}
	}
	// Empty and null YAML documents decode to a nil map.
	if doc == nil {
		doc = map[string]any{}
	}
	for _, k := range networkDropInKeys {
		delete(doc, k)
	}
	for _, ip := range []string{n.NodeIP, n.ExternalIP} {
		if a, err := netip.ParseAddr(ip); err != nil || !a.Is4() {
			return "", fmt.Errorf("node address %q is not an IPv4 address", ip)
		}
	}
	doc["node-ip"] = n.NodeIP
	doc["node-external-ip"] = n.ExternalIP
	switch mode {
	case NetworkModePrivate:
		if !interfaceNamePattern.MatchString(n.Interface) {
			return "", fmt.Errorf("private mode requires a valid interface name for the node's private address; got %q", n.Interface)
		}
		doc["flannel-iface"] = n.Interface
		if role == RoleServer {
			doc["advertise-address"] = n.NodeIP
			doc["tls-san+"] = []string{n.NodeIP}
		}
	case NetworkModePublic:
		if role == RoleServer {
			// Workers in other networks need the external API address, overriding
			// any private address written to the base config by kip install.
			doc["advertise-address"] = n.ExternalIP
			doc["flannel-external-ip"] = true
		}
		if n.TunnelMTU > 0 {
			doc["flannel-conf"] = flannelConfPath
		}
	default:
		return "", fmt.Errorf("invalid network mode %q; expected private or public", mode)
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("rendering %s: %w", k3sNetworkConfigPath, err)
	}
	return string(out), nil
}

// flannelConfPath names the flannel configuration kip writes in public mode.
// k3s uses it instead of generating its own, which has no MTU setting.
const flannelConfPath = "/etc/rancher/k3s/kipper-flannel.json"

// flannelConf renders k3s's WireGuard configuration for an IPv4 pod network
// with the backend MTU set. Flannel subtracts WireGuard's overhead from it.
func flannelConf(podNetwork string, mtu int) (string, error) {
	p, err := netip.ParsePrefix(podNetwork)
	if err != nil || !p.Addr().Is4() {
		return "", fmt.Errorf("the pod network %q is not an IPv4 network", podNetwork)
	}
	if mtu < minTunnelMTU || mtu > publicTunnelMTU {
		return "", fmt.Errorf("tunnel MTU %d is outside the allowed range of %d to %d bytes", mtu, minTunnelMTU, publicTunnelMTU)
	}
	out, err := json.Marshal(map[string]any{
		"Network":    p.Masked().String(),
		"EnableIPv4": true,
		"EnableIPv6": false,
		"Backend":    map[string]any{"Type": "wireguard", "PersistentKeepaliveInterval": 25, "MTU": mtu},
	})
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
}

// RenderFlannelConf returns WireGuard JSON for an IPv4 pod network, with its
// CIDR masked and the backend MTU set in bytes before flannel subtracts overhead.
// It returns an error for an invalid IPv4 CIDR or an MTU outside 1280 to 1360.
// A zero MTU returns "" without validating podNetwork, as used in private mode
// to keep flannel's default MTU.
func RenderFlannelConf(podNetwork string, mtu int) (string, error) {
	if mtu == 0 {
		return "", nil
	}
	return flannelConf(podNetwork, mtu)
}

// WriteFlannelConf atomically replaces the public-mode flannel configuration
// with conf and file permissions 0600. An empty conf leaves the file unchanged.
// It returns write errors and leaves restarting k3s to the caller.
func WriteFlannelConf(client CommandRunner, conf string) error {
	if conf == "" {
		return nil
	}
	if _, err := client.RunStdin(writeDropInScript(flannelConfPath), strings.NewReader(conf)); err != nil {
		return fmt.Errorf("writing %s: %v", flannelConfPath, withoutOutput(err))
	}
	return nil
}
