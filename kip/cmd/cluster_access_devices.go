package cmd

import (
	"bufio"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/opstunnel"
	"github.com/getkipper/kipper/kip/internal/ssh"
	"github.com/getkipper/kipper/kip/internal/transport"
)

var clusterAccessDeviceCmd = &cobra.Command{
	Use:   "device",
	Short: "Register this workstation as a device that reaches the nodes through WireGuard",
	Long: `A device is an operator workstation with its own WireGuard key. kip carries
SSH and the Kubernetes API to every node through an encrypted tunnel on one
UDP port, so the access list can stay short, or empty: with a device and an
empty list, SSH and the API accept only registered devices.

The tunnel runs inside kip, leaving this workstation's network interfaces
and routes unchanged.`,
}

var clusterAccessDeviceAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Register this workstation as a device and verify it on every node",
	Long: `Register this workstation as a device called <name> (lower-case letters,
digits and hyphens). The first device sets up the operator interface on every
node, so no separate enable step is needed.

kip loads the device on every node, checks a new SSH connection through the
tunnel on each node and the API through it on the server, and only then marks
the device active. When the access list then holds only this workstation's own
address, kip offers to remove it, so that SSH and the API accept only
registered devices. --device-only does that without asking.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runClusterAccessDeviceAdd,
}

var clusterAccessDeviceListCmd = &cobra.Command{
	Use:          "list",
	Short:        "List the registered devices",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runClusterAccessDeviceList,
}

var clusterAccessDeviceRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a device from every node",
	Long: `Remove a device: kip writes the record first, then removes its peer from every
node. Removing the device this workstation uses runs over the direct path, so
pass --via direct from a listed address.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runClusterAccessDeviceRemove,
}

var clusterAccessDeviceRepinCmd = &cobra.Command{
	Use:   "repin <node>",
	Short: "Trust a node's new WireGuard key after its host was reinstalled",
	Long: `kip pins each node's WireGuard key like an SSH host key and refuses a node
whose key changed. After a legitimate reinstall, repin reads the node's key
again over a direct SSH connection and trusts it.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runClusterAccessDeviceRepin,
}

func init() {
	for _, c := range []*cobra.Command{clusterAccessDeviceAddCmd, clusterAccessDeviceListCmd, clusterAccessDeviceRemoveCmd, clusterAccessDeviceRepinCmd} {
		c.Flags().String("ssh-key", "", "path to SSH private key")
		clusterAccessDeviceCmd.AddCommand(c)
	}
	clusterAccessDeviceAddCmd.Flags().String("range", installer.DefaultOpsRange, "the operator range, an IPv4 /24 (first device only)")
	clusterAccessDeviceAddCmd.Flags().Int("port", installer.DefaultOpsPort, "the UDP port of the tunnel on every node (first device only)")
	clusterAccessDeviceAddCmd.Flags().Bool("device-only", false, "remove this workstation's own address from the list once the device works")
	clusterAccessCmd.AddCommand(clusterAccessDeviceCmd)
}

type localDevice struct {
	name    string
	address netip.Addr
	public  string
	state   string
}

// loadLocalDevice returns the cluster's pending or active local device, or nil
// for any other state. An unreadable or invalid key file returns an error.
func loadLocalDevice(cluster *config.Cluster) (*localDevice, error) {
	ops := cluster.Ops
	if ops == nil || (ops.State != config.OpsActive && ops.State != config.OpsPending) {
		return nil, nil
	}
	path, err := config.OpsKeyPath(cluster.Name)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // kip's own key file for this cluster
	if err != nil {
		return nil, fmt.Errorf("reading the device key: %w", err)
	}
	pub, err := devicePublicKey(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("the device key in %s: %w", path, err)
	}
	addr, _ := netip.ParseAddr(ops.Address)
	return &localDevice{name: ops.Device, address: addr, public: pub, state: ops.State}, nil
}

// newDeviceKey returns a fresh WireGuard private key, clamped as wg genkey
// does, in base64.
func newDeviceKey() (string, error) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return "", err
	}
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	return base64.StdEncoding.EncodeToString(k), nil
}

func devicePublicKey(private string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(private)
	if err != nil || len(b) != 32 {
		return "", errors.New("not a WireGuard key")
	}
	k, err := ecdh.X25519().NewPrivateKey(b)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()), nil
}

// saveDeviceKey writes the private key owner-only, through a file beside it
// that is renamed into place.
func saveDeviceKey(cluster, private string) error {
	path, err := config.OpsKeyPath(cluster)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".kip-new"
	if err := os.WriteFile(tmp, []byte(private+"\n"), 0o600); err != nil {
		return err
	}
	if err := opstunnel.SecureFile(tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func removeDeviceKey(cluster string) {
	if path, err := config.OpsKeyPath(cluster); err == nil {
		_ = os.Remove(path)
	}
}

// updateLocalOps changes the cluster's local device state under the config
// lock and mirrors the change into cluster.
func updateLocalOps(cluster *config.Cluster, mutate func(*config.Ops) *config.Ops) error {
	err := config.Update(func(cfg *config.Config) error {
		c := cfg.GetCluster(cluster.Name)
		if c == nil {
			return fmt.Errorf("no cluster named %q in the local config", cluster.Name)
		}
		c.Ops = mutate(c.Ops)
		cluster.Ops = c.Ops
		return nil
	})
	return err
}

// apiHost returns the host of the kubeconfig's API server.
func apiHost(cluster *config.Cluster) (string, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", cluster.Kubeconfig)
	if err != nil {
		return "", fmt.Errorf("loading kubeconfig %s: %w", cluster.Kubeconfig, err)
	}
	u, err := url.Parse(cfg.Host)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("the kubeconfig's server %q is not a URL", cfg.Host)
	}
	return u.Hostname(), nil
}

// manifestFor builds the local node manifest from the record's ops state and
// the node records, pinning each node's key. A node whose key differs from
// the pinned one is refused.
func manifestFor(cluster *config.Cluster, ops *installer.AccessOps, records []installer.NodeRecord, ports map[string][]int) ([]config.OpsNode, error) {
	known := map[string]config.OpsNode{}
	if cluster.Ops != nil {
		for _, n := range cluster.Ops.Nodes {
			known[n.MachineID] = n
		}
	}
	var out []config.OpsNode
	for _, r := range records {
		if r.State == installer.NodeStateWiping {
			continue
		}
		on, ok := ops.Node(r.MachineID)
		if !ok {
			continue
		}
		k, ok := known[r.MachineID]
		if ok && k.PublicKey != on.PublicKey {
			return nil, fmt.Errorf("the WireGuard key of %s changed. If its host was reinstalled, trust the new key with 'kip cluster access device repin %s'", r.Name, r.Name)
		}
		out = append(out, config.OpsNode{
			MachineID: r.MachineID, Name: r.Name, Host: r.Host,
			Endpoint:  net.JoinHostPort(strings.Trim(r.Host, "[]"), strconv.Itoa(ops.Port)),
			PublicKey: on.PublicKey, Address: on.Address, SSHPorts: k.SSHPorts,
			Server: r.Role == installer.NodeRoleServer,
		})
		if p, ok := ports[r.MachineID]; ok {
			out[len(out)-1].SSHPorts = p
		}
	}
	return out, nil
}

// refreshManifest writes the local node manifest for next, when this
// workstation holds a device.
func (c *accessCluster) refreshManifest(next installer.AccessRecord, ports map[string][]int) error {
	if c.device == nil || next.Ops == nil || c.cluster == nil {
		return nil
	}
	nodes, err := manifestFor(c.cluster, next.Ops, c.records, ports)
	if err != nil {
		return err
	}
	return updateLocalOps(c.cluster, func(o *config.Ops) *config.Ops {
		if o == nil {
			return nil
		}
		o.Nodes = nodes
		return o
	})
}

// checkStaleDevice marks the local device stale when a successfully read
// record no longer lists its key, after a reset or a removal run elsewhere.
// kip then stops using it in this session. Saving the stale state also prevents
// later sessions from using it or repeating the notice.
func (c *accessCluster) checkStaleDevice(rec installer.AccessRecord, present bool) {
	if c.device == nil || c.device.state != config.OpsActive || c.cluster == nil {
		return
	}
	if present {
		if _, ok := rec.DeviceByKey(c.device.public); ok {
			return
		}
	}
	say(c.out, "  ℹ  The access record no longer lists this workstation's device %s, so kip stops using its key. Add it again with 'kip cluster access device add'\n", c.device.name)
	c.useTunnel, c.noDirect = false, false
	_ = updateLocalOps(c.cluster, func(o *config.Ops) *config.Ops {
		if o != nil {
			o.State = config.OpsStale
		}
		return o
	})
	c.device = nil
}

// tunnelRunner returns a cached observer for n. Each observation opens a new
// SSH connection through the device tunnel.
func (c *accessCluster) tunnelRunner(n accessNode) (installer.AddressObserver, error) {
	if r, ok := c.tunnels[n.rec.MachineID]; ok {
		return r, nil
	}
	if c.tunnel == nil {
		return nil, errors.New("this workstation has no device tunnel")
	}
	r, err := c.tunnel(n)
	if err != nil {
		return nil, err
	}
	if c.tunnels == nil {
		c.tunnels = map[string]installer.AddressObserver{}
	}
	c.tunnels[n.rec.MachineID] = r
	return r, nil
}

// tunnelBaseline reports whether a new SSH connection through the tunnel
// reaches n and arrives from this device's address.
func (c *accessCluster) tunnelBaseline(n accessNode) bool {
	r, err := c.tunnelRunner(n)
	if err == nil {
		var got string
		if got, err = r.ObservedAddress("4"); err == nil {
			if a, perr := netip.ParseAddr(got); perr != nil || a != c.device.address {
				err = fmt.Errorf("it saw the connection come from %s, not the device address %s", got, c.device.address)
			}
		}
	}
	if err != nil {
		say(c.out, "  ℹ  %s: no session through device %s (%v)\n", n.rec.Name, c.device.name, firstErrLine(err))
		return false
	}
	say(c.out, "  ✔  %s: reached through device %s\n", n.rec.Name, c.device.name)
	return true
}

// deviceActiveIn reports whether next lists this workstation's device as
// active, so that it counts as a way in.
func (c *accessCluster) deviceActiveIn(next installer.AccessRecord) bool {
	if c.device == nil {
		return false
	}
	d, ok := next.DeviceByKey(c.device.public)
	return ok && d.State == installer.DeviceActive
}

// tunnelVerified reports whether a change to next is checked through the
// device tunnel: when the command uses the tunnel and next keeps the device,
// and when the device is being added.
func (c *accessCluster) tunnelVerified(next installer.AccessRecord) bool {
	if c.device == nil || (!c.useTunnel && !c.verifyTunnel) {
		return false
	}
	_, ok := next.DeviceByKey(c.device.public)
	return ok
}

// refuseRevokedDevices rejects changes while a node has a console revocation
// for a device that next still lists. Device removal and address additions
// remain available so the operator can recover access and remove the device.
func (c *accessCluster) refuseRevokedDevices(nodes []accessNode, next installer.AccessRecord, action string) error {
	if len(next.Devices) == 0 || action == "device-removed" || action == "allow" {
		return nil
	}
	for _, n := range nodes {
		keys, err := c.revoked(n.node)
		if err != nil {
			return fmt.Errorf("%s: %w", n.rec.Name, err)
		}
		for _, k := range keys {
			if d, ok := next.DeviceByKey(k); ok {
				return fmt.Errorf("device %[1]s was revoked on %[2]s from the console, but the record still lists it. Remove it with 'kip cluster access device remove %[1]s' first", d.Name, n.rec.Name)
			}
		}
	}
	return nil
}

// addOpsNode prepares n's operator interface and records its address and
// key in ops. A route on n into the operator range is refused, because the
// interface would cut that route off.
func (c *accessCluster) addOpsNode(ops *installer.AccessOps, n accessNode) error {
	c.defaults()
	rng, err := installer.ParseOpsRange(ops.Range)
	if err != nil {
		return err
	}
	routes, err := c.routes(n.node)
	if err != nil {
		return fmt.Errorf("%s: %w", n.rec.Name, err)
	}
	for _, p := range routes {
		if p.Overlaps(rng) {
			return fmt.Errorf("%s has a route to %s, inside the cluster's operator range %s, so its operator interface would cut that route off. Remove the route, or start the devices again on another range with 'kip cluster access reset' and 'device add --range'", n.rec.Name, p, rng)
		}
	}
	say(c.out, "  ...  Preparing the operator interface on %s\n", n.rec.Name)
	key, err := c.opsKey(n.node)
	if err != nil {
		return fmt.Errorf("%s: %w", n.rec.Name, err)
	}
	_, err = ops.AllocateNode(n.rec.MachineID, key)
	return err
}

// completeOps returns next with an operator address and key for each node
// the operator state lacks, such as one restored from an older record.
func (c *accessCluster) completeOps(next installer.AccessRecord) (installer.AccessRecord, error) {
	if next.Ops == nil {
		return next, nil
	}
	ops := *next.Ops
	ops.Nodes = append([]installer.AccessOpsNode(nil), next.Ops.Nodes...)
	for _, n := range c.nodes {
		if _, ok := ops.Node(n.rec.MachineID); ok {
			continue
		}
		if err := c.addOpsNode(&ops, n); err != nil {
			return next, err
		}
	}
	next.Ops = &ops
	return next, nil
}

// applyOpsOn loads the operator interface with next's devices on n, before
// its access table changes.
func (c *accessCluster) applyOpsOn(n accessNode, next installer.AccessRecord, ports []int) error {
	if next.Ops == nil {
		return nil
	}
	c.defaults()
	on, ok := next.Ops.Node(n.rec.MachineID)
	if !ok {
		return fmt.Errorf("%s has no operator address in the record. Run 'kip cluster access device add' again", n.rec.Name)
	}
	addr, err := netip.ParseAddr(on.Address)
	if err != nil {
		return err
	}
	peers, err := installer.RenderOpsPeers(next.Generation, next.Ops.Port, next.Devices)
	if err != nil {
		return err
	}
	in := installer.OpsNodeIntent{Address: addr, Devices: next.Ops.DevicePrefix(), SSHPorts: ports, Port: next.Ops.Port}
	say(c.out, "  ...  Loading %s on %s's operator interface\n", plural(len(next.Devices), "one device", fmt.Sprintf("%d devices", len(next.Devices))), n.rec.Name)
	if err := c.applyOps(n.node, in, peers); err != nil {
		return fmt.Errorf("%s: %w", n.rec.Name, err)
	}
	return nil
}

func runClusterAccessDeviceAdd(cmd *cobra.Command, args []string) error {
	req := deviceRequest{name: args[0]}
	if err := installer.ValidDeviceName(req.name); err != nil {
		return err
	}
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}
	if cluster.Ops != nil && cluster.Ops.State == config.OpsPending && cluster.Ops.Device != req.name {
		return fmt.Errorf("this workstation is still adding device %s. Finish it with 'kip cluster access device add %s', or abandon it with 'kip cluster access device remove %s'", cluster.Ops.Device, cluster.Ops.Device, cluster.Ops.Device)
	}
	req.rng, _ = cmd.Flags().GetString("range")
	req.port, _ = cmd.Flags().GetInt("port")
	req.rangeSet, req.portSet = cmd.Flags().Changed("range"), cmd.Flags().Changed("port")
	deviceOnly, _ := cmd.Flags().GetBool("device-only")
	if _, err := installer.ParseOpsRange(req.rng); err != nil {
		return err
	}
	if req.port < 1 || req.port > 65535 {
		return fmt.Errorf("%d is not a UDP port", req.port)
	}
	if req.apiHost, err = apiHost(cluster); err != nil {
		return err
	}

	s, err := openAccessSession(cmd, "device add", false)
	if err != nil {
		return err
	}
	defer s.close()
	rec, err := s.c.enroll(req)
	if err != nil {
		return err
	}
	interactive := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) //nolint:gosec // file descriptors fit an int
	return s.c.offerDeviceOnly(rec, deviceOnly, os.Stdin, interactive)
}

// deviceRequest is what 'device add' asks for: the device name, the API
// host the tunnel carries, and the operator range and tunnel port, with
// whether each was given.
type deviceRequest struct {
	name, apiHost, rng string
	port               int
	rangeSet, portSet  bool
}

// enroll adds this workstation as the device req asks for and activates it
// once every node has verified it through the tunnel. Initial name and range
// checks precede node preparation, which can install tools and create keys.
// Preparation precedes local device and access-record writes. Without an
// access list, it lists this workstation's address first. On success, it returns
// the record with the device active. Later failures can leave a pending device.
func (c *accessCluster) enroll(req deviceRequest) (installer.AccessRecord, error) {
	cluster := c.cluster
	rec, present, err := installer.ReadAccessRecord(c.server)
	if err != nil {
		return rec, err
	}
	c.checkStaleDevice(rec, present)
	if c.device != nil && c.device.state == config.OpsActive {
		return rec, fmt.Errorf("this workstation is already device %s of cluster %s. Remove it with 'kip cluster access device remove %s' first", c.device.name, cluster.Name, c.device.name)
	}
	if p, ok := rec.PendingDevice(); ok && p.Name != req.name {
		return rec, fmt.Errorf("device %s is being added. Finish it from its workstation, or abandon it with 'kip cluster access device remove %s'", p.Name, p.Name)
	}
	resumeKey := ""
	if cluster.Ops != nil && cluster.Ops.State == config.OpsPending {
		path, err := config.OpsKeyPath(cluster.Name)
		if err != nil {
			return rec, err
		}
		if raw, err := os.ReadFile(path); err == nil { //nolint:gosec // kip's own key file
			resumeKey = strings.TrimSpace(string(raw))
		}
	}
	if d, ok := rec.Device(req.name); ok {
		pub := ""
		if resumeKey != "" {
			pub, _ = devicePublicKey(resumeKey)
		}
		if pub == "" || d.PublicKey != pub {
			return rec, fmt.Errorf("the cluster already has a device called %s. Pick another name, or remove it with 'kip cluster access device remove %s'", req.name, req.name)
		}
	}
	if rec.Ops != nil {
		if req.rangeSet && req.rng != rec.Ops.Range {
			return rec, fmt.Errorf("the cluster's operator range is %s. --range only applies to the first device", rec.Ops.Range)
		}
		if req.portSet && req.port != rec.Ops.Port {
			return rec, fmt.Errorf("the cluster's tunnel port is %d. --port only applies to the first device", rec.Ops.Port)
		}
	} else if err := c.checkOpsRange(req.rng, rec); err != nil {
		return rec, err
	}
	keys, err := c.prepareOps()
	if err != nil {
		return rec, err
	}

	if !present {
		say(c.out, "\n  No access list yet, listing this workstation's address first so you keep a way in.\n\n")
		if err := c.change("allow", c.allowEdit(nil, true, ""), false); err != nil {
			return rec, err
		}
	}
	private := resumeKey
	if private == "" {
		if private, err = newDeviceKey(); err != nil {
			return rec, err
		}
		if err := saveDeviceKey(cluster.Name, private); err != nil {
			return rec, fmt.Errorf("saving the device key: %w", err)
		}
	}
	public, err := devicePublicKey(private)
	if err != nil {
		return rec, err
	}
	if err := updateLocalOps(cluster, func(o *config.Ops) *config.Ops {
		keep := []config.OpsNode(nil)
		if o != nil {
			keep = o.Nodes
		}
		return &config.Ops{Device: req.name, State: config.OpsPending, APIHost: req.apiHost, Nodes: keep}
	}); err != nil {
		return rec, err
	}
	c.device = &localDevice{name: req.name, public: public, state: config.OpsPending}
	c.verifyTunnel = true

	say(c.out, "\n  Adding device %s\n\n", req.name)
	if err := c.change("device-added", c.addDeviceEdit(req.name, public, req.rng, req.port, keys), false); err != nil {
		return rec, err
	}
	if rec, err = c.activateDevice(req.name, public); err != nil {
		return rec, err
	}
	if err := updateLocalOps(cluster, func(o *config.Ops) *config.Ops {
		o.State = config.OpsActive
		return o
	}); err != nil {
		return rec, err
	}
	c.device.state = config.OpsActive
	c.verifyTunnel = false
	mode := transport.Mode(cluster)
	c.useTunnel, c.noDirect = mode != transport.Direct, mode == transport.Tunnel
	say(c.out, "\n  ✔  Device %s is active. kip now reaches SSH and the API through the tunnel on UDP %d\n", req.name, rec.Ops.Port)
	return rec, nil
}

// prepareOps installs WireGuard and creates the operator key on every node,
// refusing nodes kip cannot set up. It returns each node's public key by
// machine ID.
func (c *accessCluster) prepareOps() (map[string]string, error) {
	c.defaults()
	keys := map[string]string{}
	for _, n := range c.nodes {
		say(c.out, "  ...  Preparing the operator interface on %s\n", n.rec.Name)
		key, err := c.opsKey(n.node)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n.rec.Name, err)
		}
		keys[n.rec.MachineID] = key
	}
	return keys, nil
}

// offerDeviceOnly empties the address list only when all its entries are
// this workstation's 'allow --here' prefixes. It requires deviceOnly or an
// affirmative answer read from in when interactive is true.
func (c *accessCluster) offerDeviceOnly(rec installer.AccessRecord, deviceOnly bool, in io.Reader, interactive bool) error {
	if len(rec.Sources) == 0 {
		say(c.out, "  ✔  The access list is empty, so SSH and the API already accept only registered devices\n\n")
		return nil
	}
	here := map[string]bool{}
	for _, b := range c.lastBaselines {
		for f, a := range b.observed {
			if f == "4" {
				here[netip.PrefixFrom(a, 32).String()] = true
			} else {
				p, _ := a.Prefix(64)
				here[p.String()] = true
			}
		}
	}
	var own, other []string
	for _, s := range rec.Sources {
		if here[s.Prefix] {
			own = append(own, s.Prefix)
		} else {
			other = append(other, s.Prefix)
		}
	}
	if len(other) > 0 {
		say(c.out, "  ℹ  The access list keeps direct access for %s. To accept only registered devices, run 'kip cluster access revoke %s'\n\n", strings.Join(other, ", "), strings.Join(append(own, other...), " "))
		if deviceOnly {
			return fmt.Errorf("--device-only: the access list still holds %s, which is not this workstation's own address, so kip left it as it is", strings.Join(other, ", "))
		}
		return nil
	}
	if !deviceOnly {
		if !interactive {
			say(c.out, "  ℹ  To accept only registered devices, run 'kip cluster access revoke %s'\n\n", strings.Join(own, " "))
			return nil
		}
		say(c.out, "  Remove %s from the access list, so that SSH and the API accept only registered devices? (y/N) ", strings.Join(own, " and "))
		answer, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			say(c.out, "  The access list stays as it is.\n\n")
			return nil
		}
	}
	if err := c.change("revoke", revokeEdit(own), false); err != nil {
		return err
	}
	say(c.out, "  ✔  SSH and the API now accept only registered devices. From unlisted networks the tunnel on UDP %d is the only way in\n\n", rec.Ops.Port)
	return nil
}

// checkOpsRange refuses an operator range that overlaps the pod or Service
// network, a node address, a route on a node, or a listed operator prefix.
func (c *accessCluster) checkOpsRange(r string, rec installer.AccessRecord) error {
	rng, err := installer.ParseOpsRange(r)
	if err != nil {
		return err
	}
	nets := map[string][]netip.Prefix{}
	c.defaults()
	if pod, err := c.pod(); err == nil {
		nets["the pod network"] = []netip.Prefix{pod}
	}
	service, err := c.service()
	if err != nil {
		return err
	}
	nets["the Service network"] = []netip.Prefix{service}
	for _, n := range c.records {
		if a, err := netip.ParseAddr(strings.Trim(n.Host, "[]")); err == nil {
			nets["node "+n.Name] = append(nets["node "+n.Name], netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
		}
		if a, err := netip.ParseAddr(n.PeerAddress); err == nil {
			nets["node "+n.Name] = append(nets["node "+n.Name], netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
		}
	}
	for _, n := range c.nodes {
		routes, err := c.routes(n.node)
		if err != nil {
			return fmt.Errorf("%s: %w", n.rec.Name, err)
		}
		nets["a route on "+n.rec.Name] = routes
	}
	if admin, err := rec.Prefixes(); err == nil && len(admin) > 0 {
		nets["the access list"] = admin
	}
	return installer.OpsRangeOverlap(rng, nets)
}

func runClusterAccessDeviceList(cmd *cobra.Command, _ []string) error {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}
	keyFlag, _ := cmd.Flags().GetString("ssh-key")
	server, err := dialNodeSSH(cluster.Host, keyFlag, cluster)
	if err != nil {
		return fmt.Errorf("connecting to the server: %w", err)
	}
	defer func() { _ = server.Close() }()
	rec, present, err := installer.ReadAccessRecord(server)
	if err != nil {
		return err
	}
	local, _ := loadLocalDevice(cluster)
	out := cmd.OutOrStdout()
	if !present || len(rec.Devices) == 0 {
		_, _ = fmt.Fprintln(out, "No devices. Add this workstation with 'kip cluster access device add <name>'.")
		return nil
	}
	devices := append([]installer.AccessDevice(nil), rec.Devices...)
	sort.Slice(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })
	_, _ = fmt.Fprintf(out, "%-20s %-8s %-14s %-24s %s\n", "NAME", "STATE", "ADDRESS", "ADDED", "BY")
	for _, d := range devices {
		mark := ""
		if local != nil && d.PublicKey == local.public {
			mark = "  (this workstation)"
		}
		_, _ = fmt.Fprintf(out, "%-20s %-8s %-14s %-24s %s%s\n", d.Name, d.State, d.Address, d.AddedAt, d.AddedBy, mark)
	}
	_, _ = fmt.Fprintf(out, "\nTunnel: UDP %d, operator range %s\n", rec.Ops.Port, rec.Ops.Range)
	return nil
}

func runClusterAccessDeviceRemove(cmd *cobra.Command, args []string) error {
	name := args[0]
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}
	local, err := loadLocalDevice(cluster)
	if err != nil {
		return err
	}
	if local != nil && local.name == name && local.state == config.OpsActive && transport.Mode(cluster) != transport.Direct {
		return fmt.Errorf("device %s is the one this workstation reaches the nodes through. Remove it over the direct path from a listed address with 'kip cluster access device remove %s --via direct'", name, name)
	}
	s, err := openAccessSession(cmd, "device remove", false)
	if err != nil {
		return err
	}
	defer s.close()
	return s.c.removeDevice(name, local)
}

// removeDevice removes device name from every node, and forgets this
// workstation's key when local is that device. A local device the record no
// longer lists is only forgotten.
func (c *accessCluster) removeDevice(name string, local *localDevice) error {
	own := local != nil && local.name == name
	if own {
		rec, _, err := installer.ReadAccessRecord(c.server)
		if err != nil {
			return err
		}
		if _, ok := rec.DeviceByKey(local.public); !ok {
			if err := forgetLocalDevice(c.cluster); err != nil {
				return err
			}
			say(c.out, "  ✔  The cluster no longer lists device %s, so kip removed only this workstation's key\n\n", name)
			return nil
		}
	}
	if err := c.change("device-removed", removeDeviceEdit(name), false); err != nil {
		return err
	}
	if own {
		if err := forgetLocalDevice(c.cluster); err != nil {
			return err
		}
		say(c.out, "  ✔  Removed this workstation's device key\n")
	}
	say(c.out, "  ✔  Device %s is removed from every node\n\n", name)
	return nil
}

// forgetLocalDevice attempts to delete this workstation's device key, then
// clears its operator state for cluster. Key removal errors are ignored.
func forgetLocalDevice(cluster *config.Cluster) error {
	removeDeviceKey(cluster.Name)
	return updateLocalOps(cluster, func(*config.Ops) *config.Ops { return nil })
}

func runClusterAccessDeviceRepin(cmd *cobra.Command, args []string) error {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}
	if cluster.Ops == nil {
		return errNoDevice(cluster.Name)
	}
	var node *config.OpsNode
	for i := range cluster.Ops.Nodes {
		if cluster.Ops.Nodes[i].Name == args[0] || strings.EqualFold(cluster.Ops.Nodes[i].Host, args[0]) {
			node = &cluster.Ops.Nodes[i]
		}
	}
	if node == nil {
		return fmt.Errorf("this workstation's device knows no node called %s", args[0])
	}
	keyFlag, _ := cmd.Flags().GetString("ssh-key")
	cfg, err := nodeSSHVia(node.Host, keyFlag, cluster, transport.Direct)
	if err != nil {
		return err
	}
	conn, err := ssh.Dial(cfg)
	if err != nil {
		return fmt.Errorf("connecting to %s directly: %w", node.Host, err)
	}
	defer func() { _ = conn.Close() }()
	key, err := installer.OpsNodeKey(conn, cmd.OutOrStdout())
	if err != nil {
		return err
	}
	name, changed := node.Name, node.PublicKey != key
	if err := updateLocalOps(cluster, func(o *config.Ops) *config.Ops {
		for i := range o.Nodes {
			if o.Nodes[i].MachineID == node.MachineID {
				o.Nodes[i].PublicKey = key
			}
		}
		return o
	}); err != nil {
		return err
	}
	if changed {
		say(cmd.OutOrStdout(), "  ✔  Trusting the new WireGuard key of %s\n", name)
	} else {
		say(cmd.OutOrStdout(), "  ✔  %s's WireGuard key is the pinned one\n", name)
	}
	return nil
}

// addDeviceEdit records each node's operator address and its key from keys,
// by machine ID, and adds the device name with public key as pending,
// starting the operator state with rangeFlag and port when the record has
// none. A pending device of the same key is kept, so a failed add resumes.
// It stores the device's address in the local config.
func (c *accessCluster) addDeviceEdit(name, public, rangeFlag string, port int, keys map[string]string) accessEdit {
	return func(rec installer.AccessRecord, seen map[string]netip.Addr) (installer.AccessRecord, []string, error) {
		next := rec
		next.Version = 2
		if next.Ops == nil {
			next.Ops = &installer.AccessOps{Range: rangeFlag, Port: port}
		} else {
			ops := *next.Ops
			ops.Nodes = append([]installer.AccessOpsNode(nil), rec.Ops.Nodes...)
			next.Ops = &ops
		}
		for _, n := range c.nodes {
			key, ok := keys[n.rec.MachineID]
			if !ok {
				return rec, nil, fmt.Errorf("%s has no operator key yet. Run 'kip cluster access device add' again", n.rec.Name)
			}
			if _, err := next.Ops.AllocateNode(n.rec.MachineID, key); err != nil {
				return rec, nil, err
			}
		}
		next.Devices = append([]installer.AccessDevice(nil), rec.Devices...)
		d, ok := next.Device(name)
		if !ok {
			addr, err := next.Ops.AllocateDevice()
			if err != nil {
				return rec, nil, err
			}
			d = installer.AccessDevice{Name: name, PublicKey: public, Address: addr.String(), State: installer.DevicePending, AddedBy: c.actor(accessBaseline{observed: seen}), AddedAt: c.now().UTC().Format(time.RFC3339)}
			next.Devices = append(next.Devices, d)
		}
		addr, err := netip.ParseAddr(d.Address)
		if err != nil {
			return rec, nil, err
		}
		c.device.address = addr
		if err := updateLocalOps(c.cluster, func(o *config.Ops) *config.Ops {
			o.Address = d.Address
			return o
		}); err != nil {
			return rec, nil, err
		}
		return next, []string{name}, nil
	}
}

// activateDevice marks the pending device name with public key active once
// every node has verified it. The generation stays unchanged because this
// updates only the record and its history.
func (c *accessCluster) activateDevice(name, public string) (installer.AccessRecord, error) {
	rec, _, err := installer.ReadAccessRecord(c.server)
	if err != nil {
		return rec, err
	}
	found := false
	for i := range rec.Devices {
		if rec.Devices[i].Name == name && rec.Devices[i].PublicKey == public {
			rec.Devices[i].State = installer.DeviceActive
			found = true
		}
	}
	if !found {
		return rec, fmt.Errorf("the access record no longer lists device %s, so kip did not activate it. Run 'kip cluster access device add %s' again", name, name)
	}
	rec.History = append(rec.History, installer.AccessChange{
		At: c.now().UTC().Format(time.RFC3339), By: c.actor(accessBaseline{tunnel: true}), Action: "device-activated",
		Device: name, Generation: rec.Generation,
	})
	if err := installer.WriteAccessRecord(c.server, rec); err != nil {
		return rec, fmt.Errorf("every node verified the device, but kip could not mark it active (%w). Run 'kip cluster access device add %s' again", err, name)
	}
	return rec, nil
}

// removeDeviceEdit removes the device name, and refuses to remove the last
// active device while the address list is empty.
func removeDeviceEdit(name string) accessEdit {
	return func(rec installer.AccessRecord, _ map[string]netip.Addr) (installer.AccessRecord, []string, error) {
		if _, ok := rec.Device(name); !ok {
			return rec, nil, fmt.Errorf("the cluster has no device called %s. 'kip cluster access device list' shows them", name)
		}
		next := rec
		next.Devices = nil
		for _, x := range rec.Devices {
			if x.Name != name {
				next.Devices = append(next.Devices, x)
			}
		}
		if len(next.Sources) == 0 && next.ActiveDevices() == 0 {
			return rec, nil, fmt.Errorf("%s is the last active device and the access list is empty, so removing it would leave no way in. Add an address with 'kip cluster access allow' first", name)
		}
		return next, []string{name}, nil
	}
}
