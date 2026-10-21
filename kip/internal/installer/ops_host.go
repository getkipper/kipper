package installer

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

const (
	opsDir       = "/etc/kipper/ops"
	opsUnitName  = "kipper-ops.service"
	opsUnitPath  = "/etc/systemd/system/" + opsUnitName
	opsInterface = "kipops"
	opsTable     = "inet kipper_ops"
	opsMTU       = 1380
	opsHeader    = "# kipper-ops"
	// opsPeersHeader starts peers.conf with the record generation it was
	// rendered from, so stale-record checks see device removals.
	opsPeersHeader = "# kipper-ops generation "
)

// OpsNodeIntent is everything one node's operator interface and its table
// are rendered from.
type OpsNodeIntent struct {
	Address  netip.Addr
	Devices  netip.Prefix
	SSHPorts []int
	Port     int
}

func (in OpsNodeIntent) validate() error {
	if !in.Address.Is4() {
		return fmt.Errorf("the node's operator address %s is not IPv4", in.Address)
	}
	if !in.Devices.Addr().Is4() || in.Devices.Bits() != 25 {
		return fmt.Errorf("the device range %s is not an IPv4 /25", in.Devices)
	}
	if err := ValidOpsPort(in.Port); err != nil {
		return err
	}
	if len(in.SSHPorts) == 0 {
		return errors.New("no SSH listener ports were found")
	}
	for _, p := range in.SSHPorts {
		if p < 1 || p > 65535 {
			return fmt.Errorf("%d is not a port", p)
		}
	}
	return nil
}

// ValidOpsPort returns an error unless port is between 1 and 65535 and differs from
// the cluster's WireGuard port, 51820.
func ValidOpsPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("%d is not a UDP port", port)
	}
	if strconv.Itoa(port) == wireguardPort {
		return fmt.Errorf("UDP %d carries the cluster's own WireGuard network, so the tunnel needs another port", port)
	}
	return nil
}

// RenderOpsTable renders ops.nft, which replaces the kipper_ops table. Its
// drops remain final even if later chains accept the traffic. From the operator
// interface, it permits traffic from the device range to this node's operator
// address on its SSH ports and TCP 6443, plus ICMP echo requests and destination
// unreachable messages. It blocks forwarding to and from the interface. The raw
// chain excludes the tunnel's locally addressed UDP traffic from connection
// tracking to prevent connection-tracking NAT from redirecting it. Invalid
// addresses, device prefixes and ports return an error.
func RenderOpsTable(in OpsNodeIntent) (string, error) {
	if err := in.validate(); err != nil {
		return "", err
	}
	ports := portList(append(append([]int(nil), in.SSHPorts...), accessAPIPort))
	var b strings.Builder
	fmt.Fprintf(&b, "%s\ntable %s\ndelete table %s\ntable %s {\n", opsHeader, opsTable, opsTable, opsTable)
	b.WriteString("  chain input {\n")
	b.WriteString("    type filter hook input priority filter - 20; policy accept;\n")
	fmt.Fprintf(&b, "    iifname %q meta nfproto != ipv4 drop\n", opsInterface)
	fmt.Fprintf(&b, "    iifname %q ip daddr != %s drop\n", opsInterface, in.Address)
	fmt.Fprintf(&b, "    iifname %q ip saddr != %s drop\n", opsInterface, in.Devices)
	fmt.Fprintf(&b, "    iifname %q tcp dport { %s } return\n", opsInterface, ports)
	fmt.Fprintf(&b, "    iifname %q icmp type { destination-unreachable, echo-request } return\n", opsInterface)
	fmt.Fprintf(&b, "    iifname %q drop\n", opsInterface)
	b.WriteString("  }\n  chain forward {\n")
	b.WriteString("    type filter hook forward priority filter - 20; policy accept;\n")
	fmt.Fprintf(&b, "    iifname %q drop\n    oifname %q drop\n", opsInterface, opsInterface)
	b.WriteString("  }\n  chain raw {\n")
	b.WriteString("    type filter hook prerouting priority raw; policy accept;\n")
	fmt.Fprintf(&b, "    fib daddr type local udp dport %d notrack\n", in.Port)
	b.WriteString("  }\n}\n")
	return b.String(), nil
}

// RenderOpsPeers renders peers.conf: the record generation, the listen port
// and one peer per device, each preceded by a "# device: <name>" comment so a
// console operator can tell keys apart. The loader inserts the private key.
// Devices are sorted by address, including pending devices. Invalid generations,
// ports, device names, keys or IPv4 addresses return an error.
func RenderOpsPeers(generation, port int, devices []AccessDevice) (string, error) {
	if generation < 1 {
		return "", fmt.Errorf("generation %d must be at least 1", generation)
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("%d is not a port", port)
	}
	sorted := append([]AccessDevice(nil), devices...)
	sort.Slice(sorted, func(i, j int) bool {
		a, _ := netip.ParseAddr(sorted[i].Address)
		b, _ := netip.ParseAddr(sorted[j].Address)
		return a.Less(b)
	})
	var b strings.Builder
	fmt.Fprintf(&b, "%s%d\n[Interface]\nListenPort = %d\n", opsPeersHeader, generation, port)
	for _, d := range sorted {
		if err := ValidDeviceName(d.Name); err != nil {
			return "", err
		}
		if err := ValidWireGuardKey(d.PublicKey); err != nil {
			return "", fmt.Errorf("device %s: %w", d.Name, err)
		}
		a, err := netip.ParseAddr(d.Address)
		if err != nil || !a.Is4() {
			return "", fmt.Errorf("device %s has address %q", d.Name, d.Address)
		}
		fmt.Fprintf(&b, "\n# device: %s\n[Peer]\nPublicKey = %s\nAllowedIPs = %s/32\n", d.Name, d.PublicKey, a)
	}
	return b.String(), nil
}

// renderOpsInterface renders the interface settings ops.sh sources.
func renderOpsInterface(in OpsNodeIntent) string {
	return fmt.Sprintf("ADDRESS=%s\nROUTE=%s\nMTU=%d\n", in.Address, in.Devices, opsMTU)
}

// opsComposeAwk prints peers.conf with the node's private key inserted in its
// [Interface] section and every peer whose key is revoked left out. Its
// arguments are the key file, the revoked list and peers.conf, so the key
// never appears on a command line. It fails without a key or without exactly
// one [Interface] section, so the loader only applies a config containing a key
// in a single interface section.
const opsComposeAwk = `BEGIN { RS = ""; FS = "\n" }
FILENAME == ARGV[1] { key = $1; next }
FILENAME == ARGV[2] { for (i = 1; i <= NF; i++) if ($i != "") revoked[$i] = 1; next }
{
  if ($0 ~ /\[Interface\]/) {
    out = ""
    for (i = 1; i <= NF; i++) {
      out = out $i "\n"
      if ($i == "[Interface]") { out = out "PrivateKey = " key "\n"; iface++ }
    }
    printf "%s\n", out
    next
  }
  pk = ""
  for (i = 1; i <= NF; i++) if ($i ~ /^PublicKey = /) pk = substr($i, 13)
  if (pk == "" || (pk in revoked)) next
  printf "%s\n\n", $0
}
END { if (key == "" || iface != 1) exit 1 }`

// opsFindAwk prints the public key of the peer whose "# device:" name or key
// equals n.
const opsFindAwk = `BEGIN { RS = ""; FS = "\n" }
{
  name = ""; pk = ""
  for (i = 1; i <= NF; i++) {
    if ($i ~ /^# device: /) name = substr($i, 11)
    if ($i ~ /^PublicKey = /) pk = substr($i, 13)
  }
  if (pk != "" && (name == n || pk == n)) print pk
}`

// opsScript is ops.sh. load replaces the kipper_ops table before bringing up
// the interface with the composed config. revoke-device lets a
// provider console operator persist a key revocation on this node.
// Tests use root to redirect host paths into a temporary directory.
func opsScript(root string) string {
	d := root + opsDir
	return `export LC_ALL=C LANGUAGE=C
d=` + d + `
umask 077
exec 9>>` + root + accessLock + ` || exit 1
compose() {
  [ -s $d/kipops.key ] || return 1
  r=$d/revoked-keys
  if [ -e $r ]; then [ -r $r ] || return 1; else r=/dev/null; fi
  awk '` + opsComposeAwk + `' $d/kipops.key $r $d/peers.conf
}
load() {
  ` + accessTimeout + `nft -f $d/ops.nft || return 1
  listed=$(nft list table ` + opsTable + `) || return 1
  printf '%s\n' "$listed" | sha256sum | cut -c1-64 > $d/table.sum.kip-new && mv -f $d/table.sum.kip-new $d/table.sum || return 1
  . $d/iface || return 1
  ip link show ` + opsInterface + ` >/dev/null 2>&1 || ip link add ` + opsInterface + ` type wireguard || return 1
  cfg=$(compose) || { echo 'kip could not compose the operator interface config, so its peers are unchanged' >&2; return 1; }
  printf '%s\n' "$cfg" | wg syncconf ` + opsInterface + ` /dev/stdin || return 1
  ip address replace "$ADDRESS/32" dev ` + opsInterface + ` || return 1
  ip link set ` + opsInterface + ` mtu "$MTU" up || return 1
  ip route replace "$ROUTE" dev ` + opsInterface + `
}
case "$1" in
load)
  flock 9 || exit 1
  load || exit 1
  ;;
key)
  flock 9 || exit 1
  if [ ! -s $d/kipops.key ]; then
    wg genkey > $d/kipops.key.kip-new && [ -s $d/kipops.key.kip-new ] && mv -f $d/kipops.key.kip-new $d/kipops.key || exit 1
  fi
  # Sync the key in case a previous run failed before making it durable.
  sync $d/kipops.key $d || exit 1
  wg pubkey < $d/kipops.key
  ;;
revoke-device)
  [ -n "$2" ] || { echo 'usage: ops.sh revoke-device <device name or public key>' >&2; exit 2; }
  flock 9 || exit 1
  key=$(awk -v n="$2" '` + opsFindAwk + `' $d/peers.conf 2>/dev/null | head -n 1)
  if [ -z "$key" ]; then
    case "$2" in *=) key=$2 ;; *) echo "no device called $2 in $d/peers.conf" >&2; exit 1 ;; esac
  fi
  if ! grep -qxF "$key" $d/revoked-keys 2>/dev/null; then
    printf '%s\n' "$key" >> $d/revoked-keys || exit 1
  fi
  # Sync even an existing entry in case a previous run failed before syncing.
  sync $d/revoked-keys $d || exit 1
  if ip link show ` + opsInterface + ` >/dev/null 2>&1; then
    wg set ` + opsInterface + ` peer "$key" remove || exit 1
  fi
  echo "revoked $key on this node. Run this on every node, then 'kip cluster access device remove' from a workstation"
  ;;
*)
  echo 'usage: ops.sh load|key|revoke-device <device>' >&2; exit 2
  ;;
esac
`
}

// opsUnit is the boot unit that loads the kipper_ops table and only then
// brings up the operator interface.
func opsUnit() string {
	return `[Unit]
Description=Kipper operator device interface
DefaultDependencies=no
ConditionPathExists=` + opsDir + `/ops.nft
After=local-fs.target systemd-modules-load.service ` + accessUnitName + `
Before=sysinit.target network-pre.target shutdown.target
Wants=network-pre.target
Conflicts=shutdown.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh ` + opsDir + `/ops.sh load

[Install]
WantedBy=sysinit.target
`
}

// opsPrepareCommand checks kernel WireGuard support and installs ops.sh
// while holding the shared access lock.
func opsPrepareCommand(root string) string {
	d := root + opsDir
	var b strings.Builder
	b.WriteString("(umask 077\n")
	fmt.Fprintf(&b, "mkdir -p %s && mkdir -p -m 0700 %s && chmod 0700 %s || exit 1\n", root+"/etc/kipper", d, d)
	fmt.Fprintf(&b, "exec 9>>%s && flock 9 || exit 1\n", root+accessLock)
	b.WriteString("modprobe wireguard >/dev/null 2>&1\n")
	fmt.Fprintf(&b, "[ -d %s/sys/module/wireguard ] || { echo 'this kernel has no WireGuard module, so the node cannot take part in operator devices' >&2; exit 1; }\n", root)
	b.WriteString(writeIfChanged(d+"/ops.sh", opsScript(root), "", ""))
	b.WriteString(")")
	return b.String()
}

// OpsNodeRoutes returns the IPv4 destinations of the node's routes in every
// table, leaving out default routes and the operator interface's own, so an
// operator range can be checked against the networks the node reaches.
func OpsNodeRoutes(client CommandRunner) ([]netip.Prefix, error) {
	out, err := client.Run("ip -4 route show table all")
	if err != nil {
		return nil, fmt.Errorf("reading the node's routes: %w", err)
	}
	return parseOpsRoutes(out), nil
}

// routeTypes are the route type keywords ip prints before a destination.
var routeTypes = map[string]bool{
	"unicast": true, "local": true, "broadcast": true, "multicast": true, "anycast": true,
	"unreachable": true, "blackhole": true, "prohibit": true, "throw": true, "nat": true,
}

func parseOpsRoutes(out string) []netip.Prefix {
	var nets []netip.Prefix
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		// Indented lines are the nexthops of the route above.
		if len(f) == 0 || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if routeTypes[f[0]] {
			f = f[1:]
		}
		if len(f) == 0 || f[0] == "default" || opsRoute(f) {
			continue
		}
		p, err := netip.ParsePrefix(f[0])
		if err != nil {
			a, aerr := netip.ParseAddr(f[0])
			if aerr != nil {
				continue
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		nets = append(nets, p)
	}
	return nets
}

func opsRoute(fields []string) bool {
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "dev" && fields[i+1] == opsInterface {
			return true
		}
	}
	return false
}

// OpsNodeKey installs nftables and wireguard-tools when missing and checks
// kernel WireGuard support. It rejects nodes with nftables.service enabled,
// active or activating. It creates the node's private key if absent or empty
// and returns the public key. Preparation, key generation and key validation
// failures return an error.
// The private key stays on the node. Installation output is written to out.
func OpsNodeKey(client CommandRunner, out io.Writer) (string, error) {
	return opsNodeKey(client, out, "")
}

func opsNodeKey(client CommandRunner, out io.Writer, root string) (string, error) {
	host, err := DetectHostOS(client)
	if err != nil {
		return "", err
	}
	// Prepare nftables here because a joining node loads its operator table
	// before its access table.
	if err := prepareAccessHost(client, out); err != nil {
		return "", err
	}
	install := host.InstallPackagesCommand(true, "wireguard-tools")
	if install == "" {
		return "", fmt.Errorf("installing wireguard-tools: kip cannot install packages on %s", host.ID)
	}
	if err := ensureInstalled(client, "wg", "wireguard-tools", install, out); err != nil {
		return "", err
	}
	if _, err := client.Run(opsPrepareCommand(root)); err != nil {
		return "", fmt.Errorf("preparing the operator interface: %w", err)
	}
	key, err := client.Run("sh " + root + opsDir + "/ops.sh key")
	if err != nil {
		return "", fmt.Errorf("creating the node's operator key: %w", err)
	}
	key = strings.TrimSpace(key)
	if err := ValidWireGuardKey(key); err != nil {
		return "", fmt.Errorf("the node reported %q as its operator public key: %w", key, err)
	}
	return key, nil
}

// ApplyOpsNode writes the node's ops table, interface settings and peers,
// enables the boot unit and allows the tunnel's UDP port in installed UFW
// or kip-managed firewalld, saving the rule even when the firewall is inactive.
// An active, externally managed firewalld must already allow the port in both
// its running and saved configuration before the node's configuration is written.
// It loads the table before the interface.
//
// OpsNodeKey must have run on the node first. nft checks the table before
// configuration files are written. Validation or remote command failures return
// an error. Failures after the nft check may leave partially applied changes.
func ApplyOpsNode(client CommandRunner, in OpsNodeIntent, peers string) error {
	return applyOpsNode(client, in, peers, "")
}

func applyOpsNode(client CommandRunner, in OpsNodeIntent, peers, root string) error {
	table, err := RenderOpsTable(in)
	if err != nil {
		return err
	}
	if _, err := client.RunStdin(accessTimeout+"nft -c -f -", strings.NewReader(table)); err != nil {
		return fmt.Errorf("nft refused the operator interface table, so nothing changed: %w", err)
	}
	d := root + opsDir
	var b strings.Builder
	b.WriteString("(umask 077\n")
	fmt.Fprintf(&b, "exec 9>>%s && flock 9 || exit 1\n", root+accessLock)
	fmt.Fprintf(&b, "[ -s %s/ops.sh ] && [ -s %s/kipops.key ] || { echo 'the operator interface was not prepared on this node' >&2; exit 1; }\n", d, d)
	b.WriteString(opsFirewalldCheck(root, in.Port))
	b.WriteString(writeIfChanged(d+"/ops.nft", table, "", ""))
	b.WriteString(writeIfChanged(d+"/iface", renderOpsInterface(in), "", ""))
	b.WriteString(writeIfChanged(d+"/peers.conf", peers, "", ""))
	b.WriteString(writeIfChanged(root+opsUnitPath, opsUnit(), "0644", "systemctl daemon-reload || exit 1"))
	fmt.Fprintf(&b, "%ssync %s/ops.nft %s/iface %s/peers.conf %s || exit 1\n", accessTimeout, d, d, d, d)
	fmt.Fprintf(&b, "systemctl enable %s >/dev/null 2>&1; systemctl is-enabled --quiet %s || { echo 'systemd did not enable %s' >&2; exit 1; }\n", opsUnitName, opsUnitName, opsUnitName)
	// Saving the rule while UFW is inactive keeps the port open when it is enabled.
	fmt.Fprintf(&b, "if command -v ufw >/dev/null 2>&1; then ufw allow %d/udp comment kipper-ops >/dev/null || exit 1; fi\n", in.Port)
	b.WriteString(opsFirewalldOpen(root, in.Port))
	fmt.Fprintf(&b, "command -v restorecon >/dev/null 2>&1 && restorecon -R %s %s; true) &&\n", d, root+opsUnitPath)
	fmt.Fprintf(&b, "sh %s/ops.sh load", d)
	if _, err := client.Run(b.String()); err != nil {
		return fmt.Errorf("loading the operator interface: %w", err)
	}
	return nil
}

// RemoveOpsNode disables the boot unit, then removes the operator interface,
// its table, boot unit file, firewall rules and directory, in that order.
// Firewall cleanup removes the tunnel port from UFW and kip-managed firewalld
// zones. A pending firewalld change stops removal before the interface is removed.
// Interface removal must succeed before the table is removed, so a failed removal
// leaves its guard in place. Interface and table inspection failures are treated
// as absence, and firewall rule removal failures are ignored. Other failures
// return an error, and removal can be retried.
// The connection must bypass the tunnel because removing the interface closes it.
// A node with neither the operator directory nor the boot unit file is left alone.
func RemoveOpsNode(client CommandRunner) error {
	return removeOpsNode(client, "")
}

func removeOpsNode(client CommandRunner, root string) error {
	if _, err := client.Run(opsRemoveCommand(root, "sh")); err != nil {
		return fmt.Errorf("removing the operator interface: %w", err)
	}
	return nil
}

// ScheduleOpsRemoval attempts to disable the operator interface's boot unit
// immediately and schedules removal in a transient unit about ten seconds later.
// This lets removal continue after it closes the tunnel carrying the session.
// Uninstall uses it on hosts it wipes. It returns script creation or scheduling
// errors, but the later removal runs independently and can still fail.
func ScheduleOpsRemoval(client CommandRunner) error {
	return scheduleOpsRemoval(client, "")
}

func scheduleOpsRemoval(client CommandRunner, root string) error {
	if _, err := client.Run(opsRemoveCommand(root, "systemctl disable "+opsUnitName+" >/dev/null 2>&1; systemd-run --quiet --collect --on-active=10 /bin/sh")); err != nil {
		return fmt.Errorf("scheduling the removal of the operator interface: %w", err)
	}
	return nil
}

// opsPortCommand returns a command that prints the first ListenPort in
// peers.conf, or nothing if the file is unreadable or has no matching entry.
func opsPortCommand(root string) string {
	return fmt.Sprintf("sed -n 's/^ListenPort = \\([0-9][0-9]*\\)$/\\1/p' %s/peers.conf 2>/dev/null | head -n 1", root+opsDir)
}

// readOpsPort returns the first numeric ListenPort from peers.conf, or 0 if
// the file is unreadable or has no matching entry. It returns an error if the
// remote command fails or its output cannot be represented as an int.
func readOpsPort(client CommandRunner) (int, error) {
	out, err := client.Run(opsPortCommand(""))
	if err != nil {
		return 0, fmt.Errorf("reading the operator tunnel's port: %w", err)
	}
	text := strings.TrimSpace(out)
	if text == "" {
		return 0, nil
	}
	port, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("the operator tunnel's port %q is not a number", text)
	}
	return port, nil
}

// opsUFWRules returns a rule allowing the tunnel's UDP port from peers.conf
// so UFW reloads preserve tunnel access. It returns nil if no port is found.
func opsUFWRules(client CommandRunner) ([]string, error) {
	port, err := readOpsPort(client)
	if err != nil || port == 0 {
		return nil, err
	}
	return []string{fmt.Sprintf("ufw allow %d/udp comment kipper-ops", port)}, nil
}

// opsFirewalldZones includes public and peer zones because source-based peer
// zones take precedence over public, and each must allow the tunnel's port.
const opsFirewalldZones = "public " + firewalldPeerZone + " " + firewalldServerZone

// opsFirewalldWait returns shell code that takes the firewalld lock when
// firewall-cmd is installed. It rejects a reservation less than 15 minutes old
// or a pending rollback because that change relies on settings read earlier.
// Otherwise it removes the reservation, so the previous run fails its ownership
// check if it resumes.
func opsFirewalldWait(root string) string {
	run := root + firewalldStateDir + "/firewalld-run"
	return fmt.Sprintf("if command -v firewall-cmd >/dev/null 2>&1; then\n"+
		"  exec 8>%s && flock 8 || exit 1\n"+
		"  [ -z \"$(find %s -mmin -15 2>/dev/null)$(%s)\" ] || { echo 'a firewall change is in progress on this node. Run the command again once it has finished' >&2; exit 1; }\n"+
		"  rm -f %s || exit 1\n"+
		"fi\n", root+firewalldLock, run, pendingRollbackCommand, run)
}

// opsFirewalldCheck returns shell code with the locking and reservation checks
// from opsFirewalldWait.
// For an active, externally managed firewalld, it requires the port in both the
// running and saved configuration of the default-route interface's zone, falling
// back to the default zone if the lookup fails. On failure, it prints the
// commands needed to open the port.
func opsFirewalldCheck(root string, port int) string {
	var b strings.Builder
	b.WriteString(opsFirewalldWait(root))
	fmt.Fprintf(&b, "if [ ! -s %s ] && systemctl is-active --quiet firewalld; then\n", root+firewalldClaimPath)
	b.WriteString(`  dev=$(ip route show default 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "dev") {print $(i+1); exit}}')` + "\n")
	b.WriteString(`  z=$( { [ -n "$dev" ] && firewall-cmd --get-zone-of-interface="$dev"; } 2>/dev/null || firewall-cmd --get-default-zone)` + "\n")
	fmt.Fprintf(&b, "  { firewall-cmd --zone=\"$z\" --query-port=%[1]d/udp && firewall-cmd --permanent --zone=\"$z\" --query-port=%[1]d/udp; } >/dev/null 2>&1 || "+
		"{ echo \"firewalld on this node is managed outside kip. The operator tunnel port %[1]d/udp must be open in both the running and saved configuration of zone $z. Open it with 'firewall-cmd --zone=$z --add-port=%[1]d/udp && firewall-cmd --permanent --zone=$z --add-port=%[1]d/udp', then run the command again\" >&2; exit 1; }\n", port)
	b.WriteString("fi\n")
	return b.String()
}

// opsFirewalldOpen returns shell code that adds the tunnel's port to existing
// kip-managed zones using firewalld's tools. New ports are appended to match
// firewalldWant.files. For a running firewalld, it updates both the saved and
// running configuration without a reload to preserve k3s's network rules.
// For a stopped firewalld, it updates only the saved configuration.
// The code from opsFirewalldCheck must have run first.
func opsFirewalldOpen(root string, port int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "if [ -s %s ]; then\n", root+firewalldClaimPath)
	b.WriteString("  fw=; fc=firewall-offline-cmd; systemctl is-active --quiet firewalld && { fw=1; fc='firewall-cmd --permanent'; }\n")
	fmt.Fprintf(&b, "  for z in %s; do\n", opsFirewalldZones)
	fmt.Fprintf(&b, "    [ -f %s/zones/$z.xml ] || continue\n", root+firewalldConfigDir)
	fmt.Fprintf(&b, "    $fc --zone=$z --query-port=%[1]d/udp >/dev/null 2>&1 || $fc --zone=$z --add-port=%[1]d/udp >/dev/null || exit 1\n", port)
	fmt.Fprintf(&b, "    [ -z \"$fw\" ] || firewall-cmd --zone=$z --query-port=%[1]d/udp >/dev/null 2>&1 || firewall-cmd --zone=$z --add-port=%[1]d/udp >/dev/null || exit 1\n", port)
	b.WriteString("  done\n")
	b.WriteString("fi\n")
	return b.String()
}

// opsRemoveCommand writes the removal script and runs it with run, passing
// the tunnel port from peers.conf.
func opsRemoveCommand(root, run string) string {
	d := root + opsDir
	script := root + "/run/kipper-ops-remove.sh"
	var b strings.Builder
	fmt.Fprintf(&b, "[ -e %s ] || [ -e %s ] || exit 0\n", d, root+opsUnitPath)
	fmt.Fprintf(&b, "p=$(%s)\n", opsPortCommand(root))
	fmt.Fprintf(&b, "cat > %s <<'KIPPER_OPS_REMOVE' || exit 1\n%sKIPPER_OPS_REMOVE\n", script, opsRemoveScript(root))
	fmt.Fprintf(&b, "%s %s \"$p\"", run, script)
	return b.String()
}

// opsRemoveScript removes the operator interface under the access lock.
// Its argument is the tunnel port whose UFW rule and entries in kip's
// firewalld zones should also be removed, or empty to skip them. Interface
// and table inspection failures are treated as absence, and firewall removal
// failures are ignored. Other failures stop cleanup. The script deletes
// itself on exit.
func opsRemoveScript(root string) string {
	return `export LC_ALL=C LANGUAGE=C
trap 'rm -f "$0"' EXIT
exec 9>>` + root + accessLock + ` && flock 9 || exit 1
` + opsFirewalldWait(root) + `if [ -e ` + root + opsUnitPath + ` ]; then systemctl disable ` + opsUnitName + ` >/dev/null 2>&1 || exit 1; fi
if ip link show ` + opsInterface + ` >/dev/null 2>&1; then ip link del ` + opsInterface + ` || exit 1; fi
if nft list table ` + opsTable + ` >/dev/null 2>&1; then nft delete table ` + opsTable + ` || exit 1; fi
rm -f ` + root + opsUnitPath + ` && systemctl daemon-reload || exit 1
if [ -n "$1" ] && command -v ufw >/dev/null 2>&1; then ufw delete allow "$1/udp" >/dev/null 2>&1; fi
if [ -n "$1" ] && [ -s ` + root + firewalldClaimPath + ` ]; then
  for z in ` + opsFirewalldZones + `; do
    [ -f ` + root + firewalldConfigDir + `/zones/$z.xml ] || continue
    if systemctl is-active --quiet firewalld; then
      firewall-cmd --permanent --zone=$z --remove-port="$1/udp"; firewall-cmd --zone=$z --remove-port="$1/udp"
    else
      firewall-offline-cmd --zone=$z --remove-port="$1/udp"
    fi >/dev/null 2>&1
  done
fi
rm -rf ` + root + opsDir + `
`
}

// ReadOpsRevokedKeys returns the device keys revoked on the node from its
// console with 'ops.sh revoke-device', in file order. Missing or unreadable
// lists produce an empty result, and invalid keys are skipped. Remote command
// failures return an error.
func ReadOpsRevokedKeys(client CommandRunner) ([]string, error) {
	return readOpsRevokedKeys(client, "")
}

func readOpsRevokedKeys(client CommandRunner, root string) ([]string, error) {
	out, err := client.Run(fmt.Sprintf("cat %s/revoked-keys 2>/dev/null; true", root+opsDir))
	if err != nil {
		return nil, fmt.Errorf("reading the node's revoked device keys: %w", err)
	}
	var keys []string
	for _, line := range strings.Split(out, "\n") {
		if k := strings.TrimSpace(line); ValidWireGuardKey(k) == nil {
			keys = append(keys, k)
		}
	}
	return keys, nil
}
