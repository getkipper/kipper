package cmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/user"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/localrun"
	"github.com/getkipper/kipper/kip/internal/ssh"
	"github.com/getkipper/kipper/kip/internal/transport"
)

var clusterAccessCmd = &cobra.Command{
	Use:   "access",
	Short: "Restrict SSH and the Kubernetes API to operator addresses",
	Long: `Restrict new connections to every node's SSH ports and to the Kubernetes API
(6443) to a list of operator prefixes. Ports 80 and 443 stay public, and the
nodes, the cluster's pods and a host that is joining keep reaching the API.

The list is opt-in: a cluster without one is open as before. 'allow --here'
starts it with the address your SSH connection comes from. Every change is
loaded on each node, checked with a new SSH connection (and on the server
with the API), and rolled back at once when the check fails, or about 180
seconds after kip's last step if kip stops. Open sessions are never cut, so
revoking an address only refuses its new connections.`,
}

var clusterAccessShowCmd = &cobra.Command{
	Use:          "show",
	Short:        "Show the operator access list and check every node against it",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runClusterAccessShow,
}

var clusterAccessAllowCmd = &cobra.Command{
	Use:   "allow [prefix]...",
	Short: "Admit operator prefixes on SSH and the Kubernetes API",
	Long: `Admit operator prefixes (for example 203.0.113.0/24 or 2001:db8::/48) on every
node's SSH ports and on the Kubernetes API. --here adds the address this
workstation's SSH connection comes from: an IPv4 address as a /32 and an IPv6
address as the /64 around it, because workstations rotate temporary IPv6
addresses inside it. Pass a /128 to admit one IPv6 address.

The list restricts every issued kubeconfig, not only kip: CI pipelines and
anyone else using kubectl need their addresses listed too.`,
	SilenceUsage: true,
	RunE:         runClusterAccessAllow,
}

var clusterAccessRevokeCmd = &cobra.Command{
	Use:          "revoke <prefix>...",
	Short:        "Remove operator prefixes from the access list",
	Args:         cobra.MinimumNArgs(1),
	SilenceUsage: true,
	RunE:         runClusterAccessRevoke,
}

var clusterAccessApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Load the recorded access list on every node again",
	Long: `Load the recorded access list on every node, for example after a node booted
closed, after a restore, or after the table was removed by hand.

When a node holds a newer list than the record, as after restoring an older
backup, apply refuses. --accept-older-record prints the prefixes the record
would admit again on each node and then applies the record's list.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runClusterAccessApply,
}

var clusterAccessResetCmd = &cobra.Command{
	Use:          "reset",
	Short:        "Remove the access list and open SSH and the API on every node",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runClusterAccessReset,
}

func init() {
	for _, c := range []*cobra.Command{clusterAccessShowCmd, clusterAccessAllowCmd, clusterAccessRevokeCmd, clusterAccessApplyCmd, clusterAccessResetCmd} {
		c.Flags().String("ssh-key", "", "path to SSH private key")
		clusterAccessCmd.AddCommand(c)
	}
	clusterAccessShowCmd.Flags().Bool("json", false, "print the result as JSON")
	clusterAccessAllowCmd.Flags().Bool("here", false, "add the address this workstation connects from")
	clusterAccessAllowCmd.Flags().String("note", "", "a note stored with the new prefixes, such as 'office'")
	clusterAccessApplyCmd.Flags().Bool("accept-older-record", false, "apply the record's list even though a node holds a newer one")
	clusterAccessResetCmd.Flags().Bool("yes", false, "confirm opening SSH and the API to every address")
	clusterCmd.AddCommand(clusterAccessCmd)
}

// accessNode is one recorded node an access verb reaches. node is
// lease-fenced. base is the plain connection used for baseline checks and
// rollback. tunnel is set when that connection goes through the device tunnel.
type accessNode struct {
	rec    installer.NodeRecord
	node   installer.FirewallRunner
	base   installer.FirewallRunner
	tunnel bool
}

// accessCluster is everything an access verb needs. The function fields
// default to the installer's and are replaced in tests.
type accessCluster struct {
	out         io.Writer
	server      installer.FirewallRunner
	records     []installer.NodeRecord
	nodes       []accessNode
	families    func(host string) []string
	readyz      func(family string) error
	apiFamilies []string
	apiErr      string
	who         string
	now         func() time.Time

	// device is this workstation's device for the cluster, or nil. With
	// useTunnel its path is checked beside the direct one, and with noDirect
	// instead of it. verifyTunnel checks a pending device after the change.
	device       *localDevice
	cluster      *config.Cluster
	useTunnel    bool
	noDirect     bool
	verifyTunnel bool
	tunnel       func(accessNode) (installer.AddressObserver, error)
	direct       func(host string) installer.AddressObserver
	reopen       func() (*accessCluster, []installer.NodeRecord, error)
	// throughTunnel replaces the session with one whose connections use the
	// tunnel, before a change that closes the direct path.
	throughTunnel func() (*accessCluster, error)
	tunnels       map[string]installer.AddressObserver
	lastBaselines map[string]accessBaseline

	pod       func() (netip.Prefix, error)
	service   func() (netip.Prefix, error)
	routes    func(installer.CommandRunner) ([]netip.Prefix, error)
	ports     func(installer.CommandRunner) ([]int, error)
	state     func(installer.CommandRunner) (installer.AccessHostState, error)
	live      func(installer.CommandRunner) (installer.AccessLive, error)
	apply     func(installer.AccessApply) (string, error)
	remove    func(installer.CommandRunner) error
	removeOps func(installer.CommandRunner) error
	opsState  func(installer.CommandRunner) (installer.OpsHostState, error)
	opsKey    func(installer.CommandRunner) (string, error)
	applyOps  func(installer.CommandRunner, installer.OpsNodeIntent, string) error
	revoked   func(installer.CommandRunner) ([]string, error)
}

func (c *accessCluster) defaults() {
	if c.now == nil {
		c.now = time.Now
	}
	if c.pod == nil {
		c.pod = func() (netip.Prefix, error) { return installer.AccessPodNetwork(c.server) }
	}
	if c.service == nil {
		c.service = func() (netip.Prefix, error) { return installer.AccessServiceNetwork(c.server) }
	}
	if c.routes == nil {
		c.routes = installer.OpsNodeRoutes
	}
	if c.ports == nil {
		c.ports = installer.AccessListenerPorts
	}
	if c.state == nil {
		c.state = installer.ReadAccessGuardState
	}
	if c.live == nil {
		c.live = installer.ReadAccessLive
	}
	if c.apply == nil {
		c.apply = installer.ApplyAccessGuard
	}
	if c.remove == nil {
		c.remove = installer.RemoveAccessGuard
	}
	if c.removeOps == nil {
		c.removeOps = installer.RemoveOpsNode
	}
	if c.opsState == nil {
		c.opsState = installer.ReadOpsState
	}
	if c.opsKey == nil {
		c.opsKey = func(r installer.CommandRunner) (string, error) { return installer.OpsNodeKey(r, c.out) }
	}
	if c.applyOps == nil {
		c.applyOps = installer.ApplyOpsNode
	}
	if c.revoked == nil {
		c.revoked = installer.ReadOpsRevokedKeys
	}
}

// accessBaseline holds the client address a node saw over each address family
// that connected successfully before the change, and whether a connection
// through this workstation's device tunnel got in.
type accessBaseline struct {
	observed map[string]netip.Addr
	tunnel   bool
}

// admitted returns the families of b whose address admin admits.
func (b accessBaseline) admitted(admin []netip.Prefix) []string {
	var out []string
	for _, f := range b.families() {
		if prefixesAdmit(admin, b.observed[f]) {
			out = append(out, f)
		}
	}
	return out
}

func (b accessBaseline) families() []string {
	var out []string
	for _, f := range []string{"4", "6"} {
		if _, ok := b.observed[f]; ok {
			out = append(out, f)
		}
	}
	return out
}

// baseline probes a fresh SSH connection over each resolved address family
// unless noDirect is set, and through the device tunnel when useTunnel is set.
// It records successful paths for verification after the change and reports
// failed paths. If none connects, it returns an error before any writes.
func (c *accessCluster) baseline(n accessNode) (accessBaseline, error) {
	b := accessBaseline{observed: map[string]netip.Addr{}}
	if c.useTunnel {
		b.tunnel = c.tunnelBaseline(n)
	}
	for _, family := range c.families(n.rec.Host) {
		if c.noDirect {
			break
		}
		got, err := c.observer(n).ObservedAddress(family)
		if errors.Is(err, localrun.ErrNoConnection) {
			return b, errors.New("kip runs these commands on the cluster's own host, so it cannot see the address your workstation connects from. Run 'kip cluster access' from your workstation")
		}
		if err != nil {
			say(c.out, "  ℹ  %s: no IPv%s session (%v). IPv%s access is not verified\n", n.rec.Name, family, firstErrLine(err), family)
			continue
		}
		addr, perr := netip.ParseAddr(got)
		if perr != nil {
			return b, fmt.Errorf("%s reported %q as this workstation's address, which is not an address", n.rec.Name, got)
		}
		b.observed[family] = addr.Unmap()
	}
	if len(b.observed) == 0 && !b.tunnel {
		return b, fmt.Errorf("kip could not open a new SSH connection to %s (%s) over IPv4 or IPv6, so it changed nothing", n.rec.Name, n.rec.Host)
	}
	return b, nil
}

// accessOutcome formats a failed change for the node's history entry using
// the first line of err, which must be non-nil. An empty outcome means the live
// access table was left unchanged. The node's name is already the entry's key,
// so its prefix is removed from the error.
func accessOutcome(name, outcome string, err error) string {
	if outcome == "" {
		outcome = "not changed"
	}
	return outcome + ": " + strings.TrimPrefix(firstErrLine(err), name+": ")
}

func firstErrLine(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}

// apiBaseline probes the API before the change using the server's successful
// SSH paths in b. It checks direct families that the API host resolves to and
// the tunnel ("t") when b includes it. It returns the paths that answered for
// verification after the change, or an error if none can be verified.
func (c *accessCluster) apiBaseline(b accessBaseline) ([]string, error) {
	if c.readyz == nil {
		return nil, fmt.Errorf("kip cannot check the Kubernetes API from this workstation (%s), so it changed nothing", c.apiErr)
	}
	verified := b.families()
	var out []string
	if b.tunnel {
		if err := c.readyz("t"); err != nil {
			say(c.out, "  ℹ  The Kubernetes API did not answer through device %s (%v). API access through the tunnel is not verified\n", c.device.name, firstErrLine(err))
		} else {
			out = append(out, "t")
		}
	}
	for _, f := range verified {
		if !containsString(c.apiFamilies, f) {
			continue
		}
		if err := c.readyz(f); err != nil {
			say(c.out, "  ℹ  The Kubernetes API did not answer over IPv%s (%v). IPv%s API access is not verified. Add the prefix with 'kip cluster access allow' if kubectl should use it\n", f, firstErrLine(err), f)
			continue
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, errors.New("the Kubernetes API did not answer from this workstation over a family its SSH connection uses, so kip could not check it after the change and changed nothing. Check that 'kubectl get --raw /readyz' works, then run the command again")
	}
	return out, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// accessEdit turns the current record into the next one. here holds the
// addresses the server saw for this workstation per family.
type accessEdit func(rec installer.AccessRecord, here map[string]netip.Addr) (installer.AccessRecord, []string, error)

// change reads every node, checks for stale records and orphaned tables,
// verifies current connectivity and guards against lockout, writes the record,
// then applies on every node, workers first and the server last.
func (c *accessCluster) change(action string, edit accessEdit, acceptOlder bool) error {
	c.defaults()
	for _, r := range c.records {
		if r.State == installer.NodeStateWiping {
			return fmt.Errorf("the removal of %[1]s has not finished, and its host may still enforce the current list. Finish it with 'kip node remove %[1]s' (add --force if the host is gone), then run this command again", r.Name)
		}
	}
	states := map[string]installer.AccessHostState{}
	var headers []installer.AccessNodeHeader
	for _, n := range c.nodes {
		st, err := c.state(n.node)
		if err != nil {
			return fmt.Errorf("%s: %w", n.rec.Name, err)
		}
		states[n.rec.MachineID] = st
		headers = append(headers, st.Header(n.rec.Name))
	}
	rec, present, err := installer.ReadAccessRecord(c.server)
	if err != nil {
		return err
	}
	switch {
	case !present:
		if err := installer.OrphanedAccessTable(headers); err != nil {
			return err
		}
		if edit == nil {
			return errors.New("this cluster has no operator access list. Start one with 'kip cluster access allow --here'")
		}
		rec = installer.AccessRecord{Version: 1}
	case acceptOlder:
		if err := c.reportWidening(rec); err != nil {
			return err
		}
	default:
		if err := installer.StaleAccessRecord(rec.Generation, headers); err != nil {
			return err
		}
	}
	c.checkStaleDevice(rec, present)

	baselines := map[string]accessBaseline{}
	for _, n := range c.nodes {
		b, err := c.baseline(n)
		if err != nil {
			return err
		}
		baselines[n.rec.MachineID] = b
	}
	serverRec, _ := installer.ServerRecord(c.records)
	c.lastBaselines = baselines
	apiOn, err := c.apiBaseline(baselines[serverRec.MachineID])
	if err != nil {
		return err
	}
	next := rec
	var changed []string
	if edit != nil {
		if next, changed, err = edit(rec, baselines[serverRec.MachineID].observed); err != nil {
			return err
		}
	}
	admin, err := next.Prefixes()
	if err != nil {
		return err
	}
	if err := c.lockoutGuard(admin, baselines, c.nodes, next); err != nil {
		return err
	}
	if err := c.sessionGuard(admin, baselines); err != nil {
		return err
	}
	if err := c.apiGuard(admin, baselines[serverRec.MachineID], apiOn, next); err != nil {
		return err
	}
	if err := c.refuseRevokedDevices(c.nodes, next, action); err != nil {
		return err
	}
	if next, err = c.completeOps(next); err != nil {
		return err
	}

	generation := installer.NextAccessGeneration(rec.Generation, headers)
	pod, err := c.pod()
	if err != nil {
		return err
	}
	intents := map[string]installer.AccessIntent{}
	ports := map[string][]int{}
	for _, n := range c.nodes {
		p, err := c.ports(n.node)
		if err != nil {
			return fmt.Errorf("%s: %w", n.rec.Name, err)
		}
		ports[n.rec.MachineID] = p
		in, err := installer.AccessIntentFor(generation, admin, p, pod, c.records, n.rec.MachineID, "")
		if err != nil {
			return fmt.Errorf("%s: %w", n.rec.Name, err)
		}
		in.Devices = len(next.Devices) > 0
		if _, err := installer.RenderAccessTable(in); err != nil {
			return fmt.Errorf("%s: %w", n.rec.Name, err)
		}
		intents[n.rec.MachineID] = in
	}
	if err := c.refreshManifest(next, ports); err != nil {
		return err
	}

	next.Generation = generation
	entry := installer.AccessChange{At: c.now().UTC().Format(time.RFC3339), By: c.actor(baselines[serverRec.MachineID]), Action: action, Generation: generation}
	if strings.HasPrefix(action, "device-") {
		entry.Device = strings.Join(changed, ", ")
	} else {
		entry.Prefixes = changed
	}
	next.History = append(next.History, entry)
	if err := installer.WriteAccessRecord(c.server, next); err != nil {
		return err
	}
	if !present {
		say(c.out, "  ℹ  The list restricts every issued kubeconfig, not only kip. CI pipelines and other kubectl users need their addresses listed too\n")
	}

	outcomes := map[string]string{}
	var failure error
	for _, n := range c.applyOrder() {
		if failure != nil {
			outcomes[n.rec.Name] = "not changed"
			continue
		}
		outcome, err := c.applyOn(n, baselines[n.rec.MachineID], intents[n.rec.MachineID], apiOn, next, ports[n.rec.MachineID])
		if err != nil {
			outcomes[n.rec.Name] = accessOutcome(n.rec.Name, outcome, err)
			failure = err
			continue
		}
		outcomes[n.rec.Name] = outcome
	}
	next.History[len(next.History)-1].Nodes = outcomes
	if err := installer.WriteAccessRecord(c.server, next); err != nil {
		say(c.out, "  ⚠  could not record the outcome of the change: %v\n", err)
	}
	if failure != nil {
		var unchanged []string
		for name, o := range outcomes {
			if o == "not changed" {
				unchanged = append(unchanged, name)
			}
		}
		sort.Strings(unchanged)
		if len(unchanged) > 0 {
			return fmt.Errorf("%w. %s still %s the previous list. Run 'kip cluster access apply' after fixing the cause", failure, strings.Join(unchanged, ", "), plural(len(unchanged), "holds", "hold"))
		}
		return failure
	}
	if len(admin) == 1 && next.ActiveDevices() == 0 && action != "device-added" {
		say(c.out, "  ⚠  The list has one entry. A single address changes with DHCP, your ISP or a corporate NAT with several outgoing addresses. Add a stable second prefix so a change cannot lock you out\n")
	}
	say(c.out, "\n  Revoking an address refuses its new connections. Sessions already open stay open.\n")
	say(c.out, "  If kip stops during a change, each node restores its committed list about 180 seconds after kip's last step.\n\n")
	return nil
}

// applyOn loads next's devices before applying access intent in. It verifies
// direct families from b that in still admits, and the tunnel when the change
// retains this device and the tunnel worked before or is being registered.
// On the server, it also verifies retained API paths from apiOn and checks
// the tunnel API during device registration.
func (c *accessCluster) applyOn(n accessNode, b accessBaseline, in installer.AccessIntent, apiOn []string, next installer.AccessRecord, ports []int) (string, error) {
	if err := c.applyOpsOn(n, next, ports); err != nil {
		return "", err
	}
	families := b.admitted(in.Admin)
	a := installer.AccessApply{Node: n.node, Base: n.base, Direct: c.observer(n), Label: n.rec.Name, Intent: in, Families: families, Out: c.out}
	// Under auto a node reached only directly is checked directly, unless
	// the change is adding this device.
	tunnel := c.tunnelVerified(next) && (c.verifyTunnel || b.tunnel)
	if tunnel {
		obs, err := c.tunnelRunner(n)
		if err != nil {
			return "", fmt.Errorf("%s: %w", n.rec.Name, err)
		}
		a.Tunnel, a.Device = obs, c.device.address
	}
	if n.rec.Role == installer.NodeRoleServer {
		a.Readyz = c.readyz
		for _, f := range apiOn {
			if f != "t" && containsString(families, f) {
				a.ReadyzOn = append(a.ReadyzOn, f)
			}
		}
		if tunnel && (c.verifyTunnel || containsString(apiOn, "t")) {
			a.ReadyzOn = append(a.ReadyzOn, "t")
		}
	}
	say(c.out, "  ...  Loading the access list on %s\n", n.rec.Name)
	outcome, err := c.apply(a)
	if err != nil {
		return outcome, err
	}
	admits := plural(len(in.Admin), "one operator prefix", fmt.Sprintf("%d operator prefixes", len(in.Admin)))
	if len(in.Admin) == 0 {
		admits = "only registered devices"
	}
	say(c.out, "  ✔  %s admits %s on SSH and the API (generation %d)\n", n.rec.Name, admits, in.Generation)
	return outcome, nil
}

// applyOrder is workers first, the server last.
func (c *accessCluster) applyOrder() []accessNode {
	var workers, servers []accessNode
	for _, n := range c.nodes {
		if n.rec.Role == installer.NodeRoleServer {
			servers = append(servers, n)
		} else {
			workers = append(workers, n)
		}
	}
	return append(workers, servers...)
}

// lockoutGuard refuses a list that does not cover the address each of
// nodes saw for this workstation, in every family that connected. A node
// this workstation reached through its device tunnel passes while next
// still lists the device as active.
func (c *accessCluster) lockoutGuard(admin []netip.Prefix, baselines map[string]accessBaseline, nodes []accessNode, next installer.AccessRecord) error {
	for _, n := range nodes {
		b := baselines[n.rec.MachineID]
		if b.tunnel && c.deviceActiveIn(next) {
			continue
		}
		if b.tunnel && len(b.families()) == 0 {
			return fmt.Errorf("kip reached %s only through device %s, which the change stops admitting, so it would lock you out", n.rec.Name, c.device.name)
		}
		for _, f := range b.families() {
			addr := baselines[n.rec.MachineID].observed[f]
			if !prefixesAdmit(admin, addr) {
				return fmt.Errorf("the new list does not admit %s, the IPv%s address %s sees for this workstation, so kip would lock you out. Add it with 'kip cluster access allow %s' or use --here", addr, f, n.rec.Name, addr)
			}
		}
	}
	return nil
}

// sessionGuard protects the command's direct connections when a change removes
// access for any address family observed in the baseline. The connection's
// address family is unknown, so each observed family must remain allowed.
// It runs after lockoutGuard, which verifies the tunnel as an alternative.
func (c *accessCluster) sessionGuard(admin []netip.Prefix, baselines map[string]accessBaseline) error {
	for _, n := range c.nodes {
		b, ok := baselines[n.rec.MachineID]
		if !ok || n.tunnel || len(b.admitted(admin)) == len(b.families()) {
			continue
		}
		return fmt.Errorf("kip connected to %s directly when this command started. The change could close that connection, so kip changed nothing. Run the command again to connect through the tunnel", n.rec.Name)
	}
	return nil
}

// apiGuard refuses a change after which the API keeps none of the paths
// apiOn found answering from this workstation: a direct family whose
// address the list still admits, or the device tunnel while next keeps the
// device active.
func (c *accessCluster) apiGuard(admin []netip.Prefix, b accessBaseline, apiOn []string, next installer.AccessRecord) error {
	for _, f := range apiOn {
		if f == "t" {
			if c.deviceActiveIn(next) {
				return nil
			}
			continue
		}
		if prefixesAdmit(admin, b.observed[f]) {
			return nil
		}
	}
	return errors.New("this change would remove every working path from this workstation to the Kubernetes API, so kip changed nothing. Check that the API answers through the device with 'kip cluster access show', then run the command again")
}

func prefixesAdmit(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (c *accessCluster) actor(b accessBaseline) string {
	who := c.who
	if who == "" {
		who = "kip"
	}
	for _, f := range []string{"4", "6"} {
		if a, ok := b.observed[f]; ok {
			return who + "@" + a.String()
		}
	}
	if b.tunnel && c.device != nil {
		return who + "@device:" + c.device.name
	}
	return who
}

// reportWidening prints, per node, the record's prefixes that node's live
// operator sets do not admit, before apply --accept-older-record restores
// the record's list.
func (c *accessCluster) reportWidening(rec installer.AccessRecord) error {
	admin, err := rec.Prefixes()
	if err != nil {
		return err
	}
	live := map[string][]netip.Prefix{}
	for _, n := range c.nodes {
		l, err := c.live(n.node)
		if err != nil {
			return fmt.Errorf("%s: %w", n.rec.Name, err)
		}
		live[n.rec.Name] = append(append([]netip.Prefix(nil), l.Sets["admin4"]...), l.Sets["admin6"]...)
	}
	widening := installer.AccessWidening(admin, live)
	if len(widening) == 0 {
		say(c.out, "  ✔  Every node already admits all prefixes in the record\n")
		return nil
	}
	names := make([]string, 0, len(widening))
	for name := range widening {
		names = append(names, name)
	}
	sort.Strings(names)
	say(c.out, "  ⚠  Applying the record (generation %d) admits again:\n", rec.Generation)
	for _, name := range names {
		say(c.out, "       %s: %s\n", name, strings.Join(widening[name], ", "))
	}
	return nil
}

// allowEdit adds prefixes, and with here the addresses the server saw.
// New sources name the workstation the server saw as their author.
func (c *accessCluster) allowEdit(args []string, here bool, note string) accessEdit {
	return func(rec installer.AccessRecord, seen map[string]netip.Addr) (installer.AccessRecord, []string, error) {
		actor := c.actor(accessBaseline{observed: seen})
		now := c.now()
		var add []netip.Prefix
		for _, a := range args {
			p, err := installer.ParseAccessPrefix(a)
			if err != nil {
				return rec, nil, err
			}
			add = append(add, p)
		}
		if here {
			if a, ok := seen["4"]; ok {
				add = append(add, netip.PrefixFrom(a, 32))
			}
			if a, ok := seen["6"]; ok {
				p, _ := a.Prefix(64)
				add = append(add, p)
				say(c.out, "  ℹ  Admitting the /64 around %s because workstations rotate temporary addresses inside it. Pass a /128 to 'allow' to admit one address\n", a)
			}
		}
		if len(add) == 0 {
			if here && c.device != nil {
				return rec, nil, fmt.Errorf("kip reached the server only through device %s, so it cannot see this workstation's direct address. Name the prefix to admit, for example 'kip cluster access allow 203.0.113.5/32'", c.device.name)
			}
			return rec, nil, errors.New("name the prefixes to admit, or pass --here")
		}
		next := rec
		next.Sources = append([]installer.AccessSource(nil), rec.Sources...)
		var changed []string
		for _, p := range add {
			if hasSource(next.Sources, p.String()) {
				continue
			}
			next.Sources = append(next.Sources, installer.AccessSource{Prefix: p.String(), Note: note, AddedBy: actor, AddedAt: now.UTC().Format(time.RFC3339)})
			changed = append(changed, p.String())
		}
		return next, changed, nil
	}
}

func hasSource(sources []installer.AccessSource, prefix string) bool {
	for _, s := range sources {
		if s.Prefix == prefix {
			return true
		}
	}
	return false
}

// revokeEdit removes prefixes and rejects removal of the last entry.
// Use reset to open the ports.
func revokeEdit(args []string) accessEdit {
	return func(rec installer.AccessRecord, _ map[string]netip.Addr) (installer.AccessRecord, []string, error) {
		drop := map[string]bool{}
		for _, a := range args {
			p, err := installer.ParseAccessPrefix(a)
			if err != nil {
				return rec, nil, err
			}
			if !hasSource(rec.Sources, p.String()) {
				return rec, nil, fmt.Errorf("%s is not in the access list", p)
			}
			drop[p.String()] = true
		}
		next := rec
		next.Sources = nil
		var changed []string
		for _, s := range rec.Sources {
			if drop[s.Prefix] {
				changed = append(changed, s.Prefix)
				continue
			}
			next.Sources = append(next.Sources, s)
		}
		if len(next.Sources) == 0 && rec.ActiveDevices() == 0 {
			return rec, nil, errors.New("that would empty the access list. 'kip cluster access reset --yes' removes the list and opens SSH and the API to every address")
		}
		return next, changed, nil
	}
}

// resetAll removes the guard from every reachable node, then finishes the
// reset on a direct session (see finishReset). A session that uses the
// tunnel first proves that every node answers SSH directly, then releases its
// lease and opens a fresh direct session through reopen, because removing the
// interfaces closes the tunnel carrying the current session. The new session
// removes the guards again under its own lease in case another run changed them.
func (c *accessCluster) resetAll(unreachable []installer.NodeRecord) error {
	c.defaults()
	for _, n := range c.nodes {
		say(c.out, "  ...  Opening SSH and the API on %s\n", n.rec.Name)
		if err := c.remove(n.node); err != nil {
			return fmt.Errorf("%s: %w. Run the command again", n.rec.Name, err)
		}
		say(c.out, "  ✔  %s no longer restricts SSH and the API\n", n.rec.Name)
	}
	if len(unreachable) > 0 {
		var names []string
		for _, r := range unreachable {
			names = append(names, r.Name)
		}
		return fmt.Errorf("reset incomplete: %s %s unreachable and still %s SSH and 6443. Run reset again when %s reachable, or remove %s with 'kip node remove'",
			strings.Join(names, ", "), plural(len(names), "is", "are"), plural(len(names), "restricts", "restrict"), plural(len(names), "it is", "they are"), plural(len(names), "it", "them"))
	}
	if !c.useTunnel {
		return c.finishReset()
	}
	if err := c.proveDirectSSH(); err != nil {
		return err
	}
	say(c.out, "  ...  Every node answers directly. Continuing without the tunnel\n")
	d, unreachable, err := c.reopen()
	if err != nil {
		return fmt.Errorf("reset incomplete: SSH and the API are open on every node, but kip could not open a direct session (%w), %s. Run 'kip cluster access reset --yes --via direct'", err, keptForReset)
	}
	return d.resetAll(unreachable)
}

const keptForReset = "so kip kept the operator interfaces, the access record and this device as the way in"

// finishReset runs on a session whose connections bypass the tunnel, opened
// by this command. When a device is active, the API must answer directly
// too. It then removes each node's operator interface, server last, deletes
// the record and forgets this workstation's device. An interface-removal failure
// keeps the record so a rerun can finish the reset. Failure to clear the local
// device state is reported as a warning after the record has been deleted.
func (c *accessCluster) finishReset() error {
	rec, present, err := installer.ReadAccessRecord(c.server)
	if err != nil {
		return err
	}
	local := c.cluster != nil && c.cluster.Ops != nil && c.cluster.Ops.State == config.OpsActive
	if ((present && rec.ActiveDevices() > 0) || local) && !c.directAPI() {
		return fmt.Errorf("reset incomplete: SSH and the API are open on every node, but the Kubernetes API did not answer this workstation directly, %s. Run 'kip cluster access reset --yes' again once 'kubectl get --raw /readyz' works without the tunnel", keptForReset)
	}
	var failed []string
	for _, n := range c.applyOrder() {
		say(c.out, "  ...  Removing the operator interface on %s\n", n.rec.Name)
		if err := c.removeOps(n.node); err != nil {
			say(c.out, "  ⚠  %s: %v\n", n.rec.Name, err)
			failed = append(failed, n.rec.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("reset incomplete: kip could not remove the operator interface on %s, so it kept the access record. Run 'kip cluster access reset --yes --via direct' again", strings.Join(failed, ", "))
	}
	if err := installer.DeleteAccessRecord(c.server); err != nil {
		return fmt.Errorf("%w. Run 'kip cluster access reset --yes --via direct' again", err)
	}
	if c.cluster != nil && c.cluster.Ops != nil {
		if err := forgetLocalDevice(c.cluster); err != nil {
			say(c.out, "  ⚠  could not forget this workstation's device: %v\n", err)
		}
	}
	say(c.out, "\n  ✔  The operator access list is gone. SSH and the API accept every address again\n")
	say(c.out, "  The operator interfaces are gone too, and with them every device.\n\n")
	return nil
}

// proveDirectSSH requires every node to answer a fresh direct SSH connection
// before reset removes the tunnel interfaces. It returns an error naming
// unreachable nodes so their tunnels remain available for recovery.
func (c *accessCluster) proveDirectSSH() error {
	var names []string
	for _, n := range c.nodes {
		reached := false
		if c.direct != nil {
			if o := c.direct(n.rec.Host); o != nil {
				for _, f := range c.families(n.rec.Host) {
					if _, err := o.ObservedAddress(f); err == nil {
						reached = true
						break
					}
				}
			}
		}
		if !reached {
			names = append(names, n.rec.Name)
		}
	}
	if len(names) > 0 {
		return notDirect(names)
	}
	return nil
}

func notDirect(names []string) error {
	return fmt.Errorf("reset incomplete: SSH and the API are open on every node, but %s did not answer a direct SSH connection from this workstation, %s. Run 'kip cluster access reset --yes' again once SSH reaches %s directly",
		strings.Join(names, ", "), keptForReset, plural(len(names), "it", "them"))
}

// directAPI reports whether the API answers this workstation directly in
// one of its families.
func (c *accessCluster) directAPI() bool {
	if c.readyz == nil {
		return false
	}
	for _, f := range c.apiFamilies {
		if f != "t" && c.readyz(f) == nil {
			return true
		}
	}
	return false
}

// useDevice configures this workstation's device and path observers for cluster.
// Tunnel commands get separate direct observers because their existing
// connections may use the tunnel. An unreadable or invalid device key
// returns an error.
func (c *accessCluster) useDevice(cluster *config.Cluster, keyFlag string) error {
	device, err := loadLocalDevice(cluster)
	if err != nil {
		return err
	}
	mode := transport.Mode(cluster)
	c.device, c.cluster = device, cluster
	c.useTunnel = device != nil && device.state == config.OpsActive && mode != transport.Direct
	c.noDirect = c.useTunnel && mode == transport.Tunnel
	c.tunnel = func(n accessNode) (installer.AddressObserver, error) {
		cfg, err := nodeSSHVia(n.rec.Host, keyFlag, cluster, transport.Tunnel)
		if err != nil {
			return nil, err
		}
		return ssh.Unshared(cfg), nil
	}
	if c.useTunnel {
		c.direct = func(host string) installer.AddressObserver {
			cfg, err := nodeSSHVia(host, keyFlag, cluster, transport.Direct)
			if err != nil {
				return nil
			}
			return ssh.Unshared(cfg)
		}
	}
	return nil
}

// observer returns the direct-path observer for n, falling back to its base
// connection when no separate observer is available.
func (c *accessCluster) observer(n accessNode) installer.AddressObserver {
	if c.direct != nil {
		if o := c.direct(n.rec.Host); o != nil {
			return o
		}
	}
	return n.base
}

// useAPI sets up the API check from the cluster's kubeconfig. A setup
// failure is kept and refuses a change that needs the check.
func (c *accessCluster) useAPI(cluster *config.Cluster) {
	families, readyz, err := apiReadyz(cluster)
	if err != nil {
		c.apiErr = err.Error()
		return
	}
	c.apiFamilies, c.readyz = families, readyz
}

// hostFamilies returns the address families an SSH host resolves to.
func hostFamilies(host string) []string {
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Unmap().Is4() {
			return []string{"4"}
		}
		return []string{"6"}
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		// Let ssh resolve it so failures can be reported per family.
		return []string{"4", "6"}
	}
	var has4, has6 bool
	for _, ip := range ips {
		if ip.To4() != nil {
			has4 = true
		} else {
			has6 = true
		}
	}
	var out []string
	if has4 {
		out = append(out, "4")
	}
	if has6 {
		out = append(out, "6")
	}
	return out
}

// apiReadyz returns the families the kubeconfig's API host resolves to and
// a check that sends GET /readyz over one family, or through the device
// tunnel for "t". Any HTTP answer counts: the check proves that the access
// table admits the connection, and sends no credentials.
func apiReadyz(cluster *config.Cluster) ([]string, func(family string) error, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", cluster.Kubeconfig)
	if err != nil {
		return nil, nil, fmt.Errorf("loading kubeconfig %s: %w", cluster.Kubeconfig, err)
	}
	u, err := url.Parse(cfg.Host)
	if err != nil || u.Hostname() == "" {
		return nil, nil, fmt.Errorf("the kubeconfig's server %q is not a URL", cfg.Host)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname(), InsecureSkipVerify: cfg.Insecure} //nolint:gosec // follows the operator's own kubeconfig, and sends no credentials
	if cfg.ServerName != "" {
		tlsConfig.ServerName = cfg.ServerName
	}
	ca := cfg.CAData
	if len(ca) == 0 && cfg.CAFile != "" {
		if ca, err = os.ReadFile(cfg.CAFile); err != nil {
			return nil, nil, fmt.Errorf("reading the kubeconfig's CA: %w", err)
		}
	}
	if len(ca) > 0 {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(ca)
		tlsConfig.RootCAs = pool
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	check := func(family string) error {
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp"+family, net.JoinHostPort(u.Hostname(), port))
		}
		if family == "t" {
			tunnelDial, err := transport.ClusterAPIDialer(cluster)
			if err != nil {
				return err
			}
			dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
				return tunnelDial(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
			}
		}
		rt := &http.Transport{TLSClientConfig: tlsConfig, DialContext: dial}
		defer rt.CloseIdleConnections()
		client := &http.Client{Transport: rt, Timeout: 10 * time.Second}
		deadline := time.Now().Add(30 * time.Second)
		for {
			resp, err := client.Get(strings.TrimSuffix(cfg.Host, "/") + "/readyz")
			if err == nil {
				_ = resp.Body.Close()
				return nil
			}
			if time.Now().After(deadline) {
				return err
			}
			time.Sleep(2 * time.Second)
		}
	}
	return hostFamilies(u.Hostname()), check, nil
}

// accessSession holds the connections and lease of one writing verb.
type accessSession struct {
	cluster     *config.Cluster
	lease       *installer.NodeLease
	conns       []*ssh.Client
	c           *accessCluster
	unreachable []installer.NodeRecord
}

func (s *accessSession) close() {
	if s.lease != nil {
		s.lease.Release()
	}
	for _, c := range s.conns {
		_ = c.Close()
	}
}

// openAccessSession takes the node lease and connects to every recorded
// node. With forReset it also connects to nodes whose removal has not
// finished. Unreachable workers are collected for reporting after the reset,
// while an unreachable server still returns an error.
func openAccessSession(cmd *cobra.Command, verb string, forReset bool) (*accessSession, error) {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return nil, err
	}
	// A lost device key moves the session to the direct path before it connects.
	if _, err := loadLocalDevice(cluster); err != nil {
		return nil, err
	}
	if cluster.Host == "" {
		return nil, fmt.Errorf("cluster %s has no host recorded, so kip cannot reach its nodes", cluster.Name)
	}
	keyFlag, _ := cmd.Flags().GetString("ssh-key")
	s := &accessSession{cluster: cluster}
	serverConn, serverTunnel, err := dialNode(cluster.Host, keyFlag, cluster)
	if err != nil {
		return nil, fmt.Errorf("connecting to the server: %w", err)
	}
	s.conns = append(s.conns, serverConn)
	lease, err := installer.TakeNodeLease(serverConn, "kip cluster access "+verb)
	if err != nil {
		s.close()
		return nil, err
	}
	s.lease = lease
	server := lease.Server(serverConn)
	records, err := recordNodes(server, cluster)
	if err != nil {
		s.close()
		return nil, err
	}
	who := ""
	if u, err := user.Current(); err == nil {
		who = u.Username
	}
	c := &accessCluster{out: os.Stdout, server: server, records: records, families: hostFamilies, who: who}
	if err := c.useDevice(cluster, keyFlag); err != nil {
		s.close()
		return nil, err
	}
	// Repin trusts a changed key, so the old pin must not stop it.
	if verb != "device repin" {
		if err := c.syncManifest(); err != nil {
			s.close()
			return nil, err
		}
	}
	for _, r := range records {
		if r.State == installer.NodeStateWiping && !forReset {
			continue
		}
		if r.Role == installer.NodeRoleServer {
			c.nodes = append(c.nodes, accessNode{rec: r, node: server, base: serverConn, tunnel: serverTunnel})
			continue
		}
		conn, tunnel, err := dialNode(r.Host, keyFlag, cluster)
		if err == nil {
			s.conns = append(s.conns, conn)
			var id string
			if id, err = installer.ReadMachineID(conn); err == nil && id != r.MachineID {
				err = fmt.Errorf("it answers as machine %s, not the recorded %s", id, r.MachineID)
			}
		}
		if err != nil {
			if forReset {
				s.unreachable = append(s.unreachable, r)
				continue
			}
			s.close()
			return nil, fmt.Errorf("%s is unreachable (%v). If it is gone, run 'kip node remove %s --force'. If it refuses this workstation, add your address from the provider console first (see 'kip cluster access show')", r.Name, firstErrLine(err), r.Name)
		}
		c.nodes = append(c.nodes, accessNode{rec: r, node: lease.Host(conn), base: conn, tunnel: tunnel})
	}
	c.useAPI(cluster)
	s.c = c
	return s, nil
}

func runClusterAccessAllow(cmd *cobra.Command, args []string) error {
	here, _ := cmd.Flags().GetBool("here")
	note, _ := cmd.Flags().GetString("note")
	if !here && len(args) == 0 {
		return errors.New("name the prefixes to admit, or pass --here")
	}
	s, err := openAccessSession(cmd, "allow", false)
	if err != nil {
		return err
	}
	defer s.close()
	fmt.Println()
	return s.c.change("allow", s.c.allowEdit(args, here, note), false)
}

func runClusterAccessRevoke(cmd *cobra.Command, args []string) error {
	s, err := openAccessSession(cmd, "revoke", false)
	if err != nil {
		return err
	}
	defer s.close()
	fmt.Println()
	return s.c.change("revoke", revokeEdit(args), false)
}

func runClusterAccessApply(cmd *cobra.Command, _ []string) error {
	acceptOlder, _ := cmd.Flags().GetBool("accept-older-record")
	s, err := openAccessSession(cmd, "apply", false)
	if err != nil {
		return err
	}
	defer s.close()
	fmt.Println()
	return s.c.change("apply", nil, acceptOlder)
}

func runClusterAccessReset(cmd *cobra.Command, _ []string) error {
	if yes, _ := cmd.Flags().GetBool("yes"); !yes {
		return errors.New("reset removes the access list and opens SSH and the Kubernetes API on every node to every address. Pass --yes to do it")
	}
	s, err := openAccessSession(cmd, "reset", true)
	if err != nil {
		return err
	}
	defer func() { s.close() }()
	s.c.reopen = func() (*accessCluster, []installer.NodeRecord, error) {
		s.close()
		transport.Override = transport.Direct
		d, err := openAccessSession(cmd, "reset", true)
		if err != nil {
			return nil, nil, err
		}
		s = d
		return d.c, d.unreachable, nil
	}
	fmt.Println()
	return s.c.resetAll(s.unreachable)
}

// accessNodeReport is one node in show's output.
type accessNodeReport struct {
	Name       string   `json:"name"`
	Host       string   `json:"host"`
	Verified   bool     `json:"verified"`
	Generation int      `json:"generation,omitempty"`
	Problems   []string `json:"problems,omitempty"`
	Admitted   []string `json:"admitted,omitempty"`
	Notes      []string `json:"notes,omitempty"`
}

// accessReport is show's result.
type accessReport struct {
	Present    bool                     `json:"present"`
	Error      string                   `json:"error,omitempty"`
	Generation int                      `json:"generation,omitempty"`
	Sources    []installer.AccessSource `json:"sources,omitempty"`
	Devices    []installer.AccessDevice `json:"devices,omitempty"`
	History    []installer.AccessChange `json:"history,omitempty"`
	Nodes      []accessNodeReport       `json:"nodes"`
	Match      bool                     `json:"match"`
}

// showNode is one node show reads. client is nil when it was unreachable.
type showNode struct {
	rec    installer.NodeRecord
	client installer.FirewallRunner
	conn   *ssh.Client
	err    error
}

// report reads the record and every reachable node without changing
// anything. Match is true only when every node enforces the record.
func (c *accessCluster) report(nodes []showNode) accessReport {
	c.defaults()
	var r accessReport
	rec, present, err := installer.ReadAccessRecord(c.server)
	r.Present = present
	if err == nil {
		c.checkStaleDevice(rec, present)
	}
	if err != nil {
		r.Error = err.Error()
	} else if present {
		r.Generation, r.Sources, r.Devices = rec.Generation, rec.Sources, rec.Devices
		r.History = rec.History
		if n := len(r.History); n > 5 {
			r.History = r.History[n-5:]
		}
	}
	admin, _ := rec.Prefixes()
	listed := map[string]bool{}
	for _, p := range admin {
		if p.Addr().Is4() {
			listed["4"] = true
		} else {
			listed["6"] = true
		}
	}
	// An address family absent from the access list may be unroutable from
	// this workstation, so connection failures in that family are only notes.
	failed := func(nr *accessNodeReport, family, msg string) {
		if !present || listed[family] {
			nr.Problems = append(nr.Problems, msg)
		} else {
			nr.Notes = append(nr.Notes, msg)
		}
	}
	r.Match = err == nil
	for _, n := range nodes {
		nr := accessNodeReport{Name: n.rec.Name, Host: n.rec.Host}
		if n.rec.State == installer.NodeStateWiping {
			nr.Problems = append(nr.Problems, fmt.Sprintf("its removal has not finished. Run 'kip node remove %s' to finish it", n.rec.Name))
		}
		if n.client == nil {
			nr.Problems = append(nr.Problems, "not verified: "+firstErrLine(n.err))
			r.Match = false
			r.Nodes = append(r.Nodes, nr)
			continue
		}
		st, err := c.state(n.client)
		if err != nil {
			nr.Problems = append(nr.Problems, "not verified: "+firstErrLine(err))
			r.Match = false
			r.Nodes = append(r.Nodes, nr)
			continue
		}
		nr.Verified = true
		nr.Generation, _ = st.Generation()
		switch {
		case !present && !st.HasTable():
		case !present:
			nr.Problems = append(nr.Problems, fmt.Sprintf("restricts SSH and 6443 to %s although the cluster has no access record", plural(st.Sources(), "one source", fmt.Sprintf("%d sources", st.Sources()))))
		case !st.HasTable():
			nr.Problems = append(nr.Problems, "no access table. Run 'kip cluster access apply'")
		default:
			nr.Problems = append(nr.Problems, accessRecordMismatches(st, rec)...)
		}
		if ops, err := c.opsState(n.client); err != nil {
			nr.Problems = append(nr.Problems, "the operator interface could not be read: "+firstErrLine(err))
		} else if r.Error == "" {
			nr.Problems = append(nr.Problems, installer.OpsDrift(ops, rec, n.rec.MachineID)...)
		}
		if st.HasTable() {
			live, err := c.live(n.client)
			if err != nil {
				nr.Problems = append(nr.Problems, "the live table could not be read: "+firstErrLine(err))
			} else {
				nr.Problems = append(nr.Problems, installer.AccessDrift(st, live)...)
			}
		}
		for _, f := range c.families(n.rec.Host) {
			addr, err := n.client.ObservedAddress(f)
			if err != nil {
				failed(&nr, f, fmt.Sprintf("a new IPv%s SSH connection failed: %s", f, firstErrLine(err)))
				continue
			}
			nr.Admitted = append(nr.Admitted, "IPv"+f+" SSH from "+addr)
		}
		if n.rec.Role == installer.NodeRoleServer && c.readyz != nil {
			for _, f := range c.apiFamilies {
				// An empty list normally blocks direct workstation access, so
				// skip a probe that would otherwise wait for its deadline.
				if present && len(admin) == 0 {
					continue
				}
				if err := c.readyz(f); err != nil {
					failed(&nr, f, fmt.Sprintf("the Kubernetes API did not answer over IPv%s: %s", f, firstErrLine(err)))
				} else {
					nr.Admitted = append(nr.Admitted, "IPv"+f+" API")
				}
			}
			if c.useTunnel {
				if err := c.readyz("t"); err != nil {
					nr.Problems = append(nr.Problems, fmt.Sprintf("the Kubernetes API did not answer through device %s: %s", c.device.name, firstErrLine(err)))
				} else {
					nr.Admitted = append(nr.Admitted, "API through device "+c.device.name)
				}
			}
		}
		if len(nr.Problems) > 0 {
			r.Match = false
		}
		r.Nodes = append(r.Nodes, nr)
	}
	return r
}

// accessRecordMismatches reports differences in the committed generation and
// the addresses admitted by the operator prefixes. Equivalent prefix ranges match.
func accessRecordMismatches(st installer.AccessHostState, rec installer.AccessRecord) []string {
	var problems []string
	if g, _ := st.Generation(); g != rec.Generation {
		problems = append(problems, fmt.Sprintf("holds generation %d while the record has generation %d", g, rec.Generation))
	}
	admin, _ := rec.Prefixes()
	if !installer.SameAccessAddresses(fileAdmin(st), admin) {
		problems = append(problems, "its committed operator list differs from the record")
	}
	return problems
}

func fileAdmin(st installer.AccessHostState) []netip.Prefix {
	return installer.AccessFilePrefixes(st.Operators)
}

func printAccessReport(out io.Writer, r accessReport) {
	say(out, "\n  Operator access\n\n")
	switch {
	case r.Error != "":
		say(out, "  ⚠  %s\n", r.Error)
	case !r.Present:
		say(out, "  Open to every address. 'kip cluster access allow --here' restricts SSH and the API.\n")
	default:
		counts := plural(len(r.Sources), "one source", fmt.Sprintf("%d sources", len(r.Sources)))
		if len(r.Devices) > 0 {
			counts += " and " + plural(len(r.Devices), "one device", fmt.Sprintf("%d devices", len(r.Devices)))
		}
		say(out, "  Generation %d, %s:\n", r.Generation, counts)
		for _, s := range r.Sources {
			line := "    " + s.Prefix
			if s.Note != "" {
				line += "  (" + s.Note + ")"
			}
			if s.AddedBy != "" {
				line += "  added by " + s.AddedBy + " " + s.AddedAt
			}
			say(out, "%s\n", line)
		}
		for _, d := range r.Devices {
			say(out, "    device %s  %s  %s  added by %s %s\n", d.Name, d.Address, d.State, d.AddedBy, d.AddedAt)
		}
		if len(r.History) > 0 {
			say(out, "\n  Recent changes:\n")
			for _, h := range r.History {
				what := strings.Join(h.Prefixes, ", ")
				if h.Device != "" {
					what = h.Device
				}
				say(out, "    %s  %s %s by %s (generation %d)\n", h.At, h.Action, what, h.By, h.Generation)
				names := make([]string, 0, len(h.Nodes))
				for name := range h.Nodes {
					names = append(names, name)
				}
				sort.Strings(names)
				// Keep history brief because current problems are listed below.
				// Full history details are available with --json.
				for _, name := range names {
					outcome, _, _ := strings.Cut(h.Nodes[name], ": ")
					if outcome == "" {
						outcome = "not changed"
					}
					say(out, "      %s: %s\n", name, outcome)
				}
			}
		}
	}
	say(out, "\n  Nodes:\n")
	for _, n := range r.Nodes {
		if len(n.Problems) == 0 {
			say(out, "    ✔  %s: matches (%s)\n", n.Name, strings.Join(n.Admitted, ", "))
		} else {
			say(out, "    ⚠  %s:\n", n.Name)
			for _, p := range n.Problems {
				say(out, "         %s\n", p)
			}
		}
		for _, note := range n.Notes {
			say(out, "         %s (the list has no prefix for this address family)\n", note)
		}
	}
	say(out, "\n  To check a node yourself:\n")
	say(out, "    nft list table inet kipper_access\n    systemctl status kipper-access\n    cat /etc/kipper/access/table.nft /etc/kipper/access/operators.nft\n")
	if len(r.Devices) > 0 {
		say(out, "    wg show kipops\n    nft list table inet kipper_ops\n")
		say(out, "  From the provider console, cut off a lost device on every node: sh /etc/kipper/ops/ops.sh revoke-device <name>\n")
	}
	say(out, "  From the provider console, admit an address until the next change: nft add element inet kipper_access admin4 { 198.51.100.9 }\n\n")
}

func runClusterAccessShow(cmd *cobra.Command, _ []string) error {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}
	// A lost device key moves show to the direct path before it connects.
	if _, err := loadLocalDevice(cluster); err != nil {
		return err
	}
	if cluster.Host == "" {
		return fmt.Errorf("cluster %s has no host recorded, so kip cannot reach its nodes", cluster.Name)
	}
	keyFlag, _ := cmd.Flags().GetString("ssh-key")
	asJSON, _ := cmd.Flags().GetBool("json")
	cfg, err := hostSSHConfig(cluster, keyFlag)
	if err != nil {
		return err
	}
	cfg.CommandTimeout = 60 * time.Second
	server, err := ssh.Dial(cfg)
	if err != nil {
		return fmt.Errorf("connecting to the server: %w", err)
	}
	defer func() { _ = server.Close() }()
	records, err := installer.ReadNodeRecords(server)
	if err != nil {
		return err
	}
	cluster = withNodes(cluster, records)
	if _, ok := installer.ServerRecord(records); !ok {
		records = append(records, installer.NodeRecord{Name: "server", Host: cluster.Host, Role: installer.NodeRoleServer})
	}
	c := &accessCluster{out: os.Stderr, server: server, records: records, families: hostFamilies, cluster: cluster}
	if families, readyz, err := apiReadyz(cluster); err == nil {
		c.apiFamilies, c.readyz = families, readyz
	}
	if c.device, err = loadLocalDevice(cluster); err != nil {
		return err
	}
	mode := transport.Mode(cluster)
	c.useTunnel = c.device != nil && c.device.state == config.OpsActive && mode != transport.Direct
	if mode == transport.Tunnel {
		c.apiFamilies = nil
	}
	if err := c.syncManifest(); err != nil {
		say(c.out, "  ⚠  %v\n", err)
	}
	var nodes []showNode
	for _, r := range records {
		if r.Role == installer.NodeRoleServer {
			nodes = append(nodes, showNode{rec: r, client: server})
			continue
		}
		wcfg, err := retargetSSH(cfg, cluster, r.Host)
		if err != nil {
			nodes = append(nodes, showNode{rec: r, err: err})
			continue
		}
		conn, err := ssh.Dial(wcfg)
		if err != nil {
			nodes = append(nodes, showNode{rec: r, err: err})
			continue
		}
		nodes = append(nodes, showNode{rec: r, client: conn, conn: conn})
	}
	defer func() {
		for _, n := range nodes {
			if n.conn != nil {
				_ = n.conn.Close()
			}
		}
	}()
	report := c.report(nodes)
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		printAccessReport(os.Stdout, report)
	}
	if !report.Match {
		return errors.New("the access list is not enforced as recorded on every node")
	}
	return nil
}
