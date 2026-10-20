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
// lease-fenced. base is the plain connection used for baseline checks and rollback.
type accessNode struct {
	rec  installer.NodeRecord
	node installer.FirewallRunner
	base installer.FirewallRunner
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

	pod    func() (netip.Prefix, error)
	ports  func(installer.CommandRunner) ([]int, error)
	state  func(installer.CommandRunner) (installer.AccessHostState, error)
	live   func(installer.CommandRunner) (installer.AccessLive, error)
	apply  func(installer.AccessApply) (string, error)
	remove func(installer.CommandRunner) error
}

func (c *accessCluster) defaults() {
	if c.now == nil {
		c.now = time.Now
	}
	if c.pod == nil {
		c.pod = func() (netip.Prefix, error) { return installer.AccessPodNetwork(c.server) }
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
}

// accessBaseline holds the client address a node saw over each address family
// that connected successfully before the change.
type accessBaseline struct {
	observed map[string]netip.Addr
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

// baseline opens one fresh connection per family the node's SSH host
// resolves to. A family that does not connect is reported and never
// verified. If neither family connects, it returns an error before any writes.
func (c *accessCluster) baseline(n accessNode) (accessBaseline, error) {
	b := accessBaseline{observed: map[string]netip.Addr{}}
	for _, family := range c.families(n.rec.Host) {
		got, err := n.base.ObservedAddress(family)
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
	if len(b.observed) == 0 {
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

// apiBaseline probes the API once per family of verified that the API host
// resolves to, before anything changes, and returns the families that
// answered. The server's change is checked in those families. It refuses
// when the API cannot be checked or answers in none of them.
func (c *accessCluster) apiBaseline(verified []string) ([]string, error) {
	if c.readyz == nil {
		return nil, fmt.Errorf("kip cannot check the Kubernetes API from this workstation (%s), so it changed nothing", c.apiErr)
	}
	var out []string
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

	baselines := map[string]accessBaseline{}
	for _, n := range c.nodes {
		b, err := c.baseline(n)
		if err != nil {
			return err
		}
		baselines[n.rec.MachineID] = b
	}
	serverRec, _ := installer.ServerRecord(c.records)
	apiOn, err := c.apiBaseline(baselines[serverRec.MachineID].families())
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
	if err := c.lockoutGuard(admin, baselines, c.nodes); err != nil {
		return err
	}

	generation := installer.NextAccessGeneration(rec.Generation, headers)
	pod, err := c.pod()
	if err != nil {
		return err
	}
	intents := map[string]installer.AccessIntent{}
	for _, n := range c.nodes {
		ports, err := c.ports(n.node)
		if err != nil {
			return fmt.Errorf("%s: %w", n.rec.Name, err)
		}
		in, err := installer.AccessIntentFor(generation, admin, ports, pod, c.records, n.rec.MachineID, "")
		if err != nil {
			return fmt.Errorf("%s: %w", n.rec.Name, err)
		}
		if _, err := installer.RenderAccessTable(in); err != nil {
			return fmt.Errorf("%s: %w", n.rec.Name, err)
		}
		intents[n.rec.MachineID] = in
	}

	next.Generation = generation
	next.History = append(next.History, installer.AccessChange{
		At: c.now().UTC().Format(time.RFC3339), By: c.actor(baselines[serverRec.MachineID]), Action: action, Prefixes: changed, Generation: generation,
	})
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
		outcome, err := c.applyOn(n, baselines[n.rec.MachineID], intents[n.rec.MachineID], apiOn)
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
	if len(admin) == 1 {
		say(c.out, "  ⚠  The list has one entry. A single address changes with DHCP, your ISP or a corporate NAT with several outgoing addresses. Add a stable second prefix so a change cannot lock you out\n")
	}
	say(c.out, "\n  Revoking an address refuses its new connections. Sessions already open stay open.\n")
	say(c.out, "  If kip stops during a change, each node restores its committed list about 180 seconds after kip's last step.\n\n")
	return nil
}

// applyOn loads in on one node and verifies it in every family of b, and
// on the server also through the API in apiOn.
func (c *accessCluster) applyOn(n accessNode, b accessBaseline, in installer.AccessIntent, apiOn []string) (string, error) {
	a := installer.AccessApply{Node: n.node, Base: n.base, Label: n.rec.Name, Intent: in, Families: b.families(), Out: c.out}
	if n.rec.Role == installer.NodeRoleServer {
		a.Readyz = c.readyz
		a.ReadyzOn = apiOn
	}
	say(c.out, "  ...  Loading the access list on %s\n", n.rec.Name)
	outcome, err := c.apply(a)
	if err != nil {
		return outcome, err
	}
	say(c.out, "  ✔  %s admits %s on SSH and the API (generation %d)\n", n.rec.Name, plural(len(in.Admin), "one operator prefix", fmt.Sprintf("%d operator prefixes", len(in.Admin))), in.Generation)
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
// nodes saw for this workstation, in every family that connected.
func (c *accessCluster) lockoutGuard(admin []netip.Prefix, baselines map[string]accessBaseline, nodes []accessNode) error {
	for _, n := range nodes {
		for _, f := range baselines[n.rec.MachineID].families() {
			addr := baselines[n.rec.MachineID].observed[f]
			if !prefixesAdmit(admin, addr) {
				return fmt.Errorf("the new list does not admit %s, the IPv%s address %s sees for this workstation, so kip would lock you out. Add it with 'kip cluster access allow %s' or use --here", addr, f, n.rec.Name, addr)
			}
		}
	}
	return nil
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
		if len(next.Sources) == 0 {
			return rec, nil, errors.New("that would empty the access list. 'kip cluster access reset --yes' removes the list and opens SSH and the API to every address")
		}
		return next, changed, nil
	}
}

// resetAll removes the guard from every reachable node and deletes the
// record only when every node was reset.
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
	if err := installer.DeleteAccessRecord(c.server); err != nil {
		return err
	}
	say(c.out, "\n  ✔  The operator access list is gone. SSH and the API accept every address again\n\n")
	return nil
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
// a check that sends GET /readyz over one family. Any HTTP answer counts:
// the check proves that the access table admits the connection, and sends
// no credentials.
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
		transport := &http.Transport{
			TLSClientConfig: tlsConfig,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp"+family, net.JoinHostPort(u.Hostname(), port))
			},
		}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
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
	if cluster.Host == "" {
		return nil, fmt.Errorf("cluster %s has no host recorded, so kip cannot reach its nodes", cluster.Name)
	}
	keyFlag, _ := cmd.Flags().GetString("ssh-key")
	s := &accessSession{cluster: cluster}
	serverConn, err := ssh.Dial(nodeSSH(cluster.Host, keyFlag, cluster))
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
	records, err := recordNodes(server, cluster.Name, cluster.Host)
	if err != nil {
		s.close()
		return nil, err
	}
	who := ""
	if u, err := user.Current(); err == nil {
		who = u.Username
	}
	c := &accessCluster{out: os.Stdout, server: server, records: records, families: hostFamilies, who: who}
	for _, r := range records {
		if r.State == installer.NodeStateWiping && !forReset {
			continue
		}
		if r.Role == installer.NodeRoleServer {
			c.nodes = append(c.nodes, accessNode{rec: r, node: server, base: serverConn})
			continue
		}
		conn, err := ssh.Dial(nodeSSH(r.Host, keyFlag, cluster))
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
		c.nodes = append(c.nodes, accessNode{rec: r, node: lease.Host(conn), base: conn})
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
	defer s.close()
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
}

// accessReport is show's result.
type accessReport struct {
	Present    bool                     `json:"present"`
	Error      string                   `json:"error,omitempty"`
	Generation int                      `json:"generation,omitempty"`
	Sources    []installer.AccessSource `json:"sources,omitempty"`
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
	if err != nil {
		r.Error = err.Error()
	} else if present {
		r.Generation, r.Sources = rec.Generation, rec.Sources
		r.History = rec.History
		if n := len(r.History); n > 5 {
			r.History = r.History[n-5:]
		}
	}
	admin, _ := rec.Prefixes()
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
			if nr.Generation != rec.Generation {
				nr.Problems = append(nr.Problems, fmt.Sprintf("holds generation %d, the record %d", nr.Generation, rec.Generation))
			}
			if !installer.SameAccessAddresses(fileAdmin(st), admin) {
				nr.Problems = append(nr.Problems, "its committed operator list differs from the record")
			}
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
				nr.Problems = append(nr.Problems, fmt.Sprintf("a new IPv%s SSH connection failed: %s", f, firstErrLine(err)))
				continue
			}
			nr.Admitted = append(nr.Admitted, "IPv"+f+" SSH from "+addr)
		}
		if n.rec.Role == installer.NodeRoleServer && c.readyz != nil {
			for _, f := range c.apiFamilies {
				if err := c.readyz(f); err != nil {
					nr.Problems = append(nr.Problems, fmt.Sprintf("the Kubernetes API did not answer over IPv%s: %s", f, firstErrLine(err)))
				} else {
					nr.Admitted = append(nr.Admitted, "IPv"+f+" API")
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
		say(out, "  Generation %d, %s:\n", r.Generation, plural(len(r.Sources), "one source", fmt.Sprintf("%d sources", len(r.Sources))))
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
		if len(r.History) > 0 {
			say(out, "\n  Recent changes:\n")
			for _, h := range r.History {
				say(out, "    %s  %s %s by %s (generation %d)\n", h.At, h.Action, strings.Join(h.Prefixes, ", "), h.By, h.Generation)
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
			continue
		}
		say(out, "    ⚠  %s:\n", n.Name)
		for _, p := range n.Problems {
			say(out, "         %s\n", p)
		}
	}
	say(out, "\n  To check a node yourself:\n")
	say(out, "    nft list table inet kipper_access\n    systemctl status kipper-access\n    cat /etc/kipper/access/table.nft /etc/kipper/access/operators.nft\n")
	say(out, "  From the provider console, admit an address until the next change: nft add element inet kipper_access admin4 { 198.51.100.9 }\n\n")
}

func runClusterAccessShow(cmd *cobra.Command, _ []string) error {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}
	if cluster.Host == "" {
		return fmt.Errorf("cluster %s has no host recorded, so kip cannot reach its nodes", cluster.Name)
	}
	keyFlag, _ := cmd.Flags().GetString("ssh-key")
	asJSON, _ := cmd.Flags().GetBool("json")
	cfg := hostSSHConfig(cluster, keyFlag)
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
	if _, ok := installer.ServerRecord(records); !ok {
		records = append(records, installer.NodeRecord{Name: "server", Host: cluster.Host, Role: installer.NodeRoleServer})
	}
	c := &accessCluster{server: server, records: records, families: hostFamilies}
	if families, readyz, err := apiReadyz(cluster); err == nil {
		c.apiFamilies, c.readyz = families, readyz
	}
	var nodes []showNode
	for _, r := range records {
		if r.Role == installer.NodeRoleServer {
			nodes = append(nodes, showNode{rec: r, client: server})
			continue
		}
		wcfg := cfg
		wcfg.Host = r.Host
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
