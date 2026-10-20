package installer

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sort"
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
	if in.Port < 1 || in.Port > 65535 {
		return fmt.Errorf("%d is not a port", in.Port)
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
// remove takes the interface down before the table. Tests use root to
// redirect host paths into a temporary directory.
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
remove)
  flock 9 || exit 1
  systemctl disable ` + opsUnitName + ` >/dev/null 2>&1
  if ip link show ` + opsInterface + ` >/dev/null 2>&1; then ip link del ` + opsInterface + ` || exit 1; fi
  if ! out=$(` + accessTimeout + `nft list table ` + opsTable + ` 2>&1 >/dev/null); then
    case "$out" in *"No such file or directory"*) ;; *) printf '%s\n' "$out" >&2; exit 1 ;; esac
  else
    ` + accessTimeout + `nft delete table ` + opsTable + ` || exit 1
  fi
  rm -f ` + root + opsUnitPath + ` && systemctl daemon-reload || exit 1
  rm -rf $d
  ;;
*)
  echo 'usage: ops.sh load|key|revoke-device <device>|remove' >&2; exit 2
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

// OpsNodeKey installs wireguard-tools when missing, checks kernel WireGuard
// support and creates the node's private key if absent or empty. It returns the
// public key or an error from preparation, key generation or key validation.
// The private key stays on the node. Installation output is written to out.
func OpsNodeKey(client CommandRunner, out io.Writer) (string, error) {
	return opsNodeKey(client, out, "")
}

func opsNodeKey(client CommandRunner, out io.Writer, root string) (string, error) {
	host, err := DetectHostOS(client)
	if err != nil {
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
// enables the boot unit and loads them: first the table, then the interface.
// nft checks the table before anything is written. OpsNodeKey must have run
// on the node first. Validation or remote command failures return an error.
// Failures after the nft check may leave partially applied changes.
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
	b.WriteString(writeIfChanged(d+"/ops.nft", table, "", ""))
	b.WriteString(writeIfChanged(d+"/iface", renderOpsInterface(in), "", ""))
	b.WriteString(writeIfChanged(d+"/peers.conf", peers, "", ""))
	b.WriteString(writeIfChanged(root+opsUnitPath, opsUnit(), "0644", "systemctl daemon-reload || exit 1"))
	fmt.Fprintf(&b, "%ssync %s/ops.nft %s/iface %s/peers.conf %s || exit 1\n", accessTimeout, d, d, d, d)
	fmt.Fprintf(&b, "systemctl enable %s >/dev/null 2>&1; systemctl is-enabled --quiet %s || { echo 'systemd did not enable %s' >&2; exit 1; }\n", opsUnitName, opsUnitName, opsUnitName)
	fmt.Fprintf(&b, "command -v restorecon >/dev/null 2>&1 && restorecon -R %s %s; true) &&\n", d, root+opsUnitPath)
	fmt.Fprintf(&b, "sh %s/ops.sh load", d)
	if _, err := client.Run(b.String()); err != nil {
		return fmt.Errorf("loading the operator interface: %w", err)
	}
	return nil
}

// RemoveOpsNode takes the operator interface down, then removes the
// kipper_ops table, the boot unit and /etc/kipper/ops. It does nothing when
// ops.sh is absent and returns an error if the remote removal command fails.
func RemoveOpsNode(client CommandRunner) error {
	return removeOpsNode(client, "")
}

func removeOpsNode(client CommandRunner, root string) error {
	d := root + opsDir
	cmd := fmt.Sprintf("if [ -e %[1]s/ops.sh ]; then sh %[1]s/ops.sh remove; else exit 0; fi", d)
	if _, err := client.Run(cmd); err != nil {
		return fmt.Errorf("removing the operator interface: %w", err)
	}
	return nil
}
