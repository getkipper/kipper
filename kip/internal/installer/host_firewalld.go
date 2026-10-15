package installer

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/getkipper/kipper/kip/internal/localrun"
)

const (
	// Separate claims keep ownership specific to each firewall backend.
	firewalldClaimPath = "/etc/kipper/firewalld-managed"
	firewalldPodZone   = "kipper-pods"
	// Keep the service CIDR in sync with the k3s installation defaults.
	k3sServiceCIDR = "10.43.0.0/16"

	firewalldConfigDir = "/etc/firewalld"
	firewalldStateDir  = "/etc/kipper"
	// firewalldLock serialises a run's commit with its rollback.
	firewalldLock         = firewalldStateDir + "/firewalld.lock"
	firewalldRollbackUnit = "kipper-firewalld-rollback"
	// Schedule recovery if the new firewall prevents the installer from reconnecting.
	firewalldRollbackSeconds = 180
)

// firewalldPolicies name the forwarding policies Kipper writes. firewalld
// limits policy names to 17 characters. Pod egress is masqueraded; ingress
// covers traffic the service load balancer DNATs from the public interface to
// pods.
var firewalldPolicies = []struct {
	name, ingress, egress string
	masquerade            bool
}{
	{"kipper-pods-out", firewalldPodZone, "public", true},
	{"kipper-pods-in", "public", firewalldPodZone, false},
}

type firewalldWant struct {
	publicPorts    []string
	publicServices []string
	podPorts       []string
	podSources     []string
	peerZones      []firewalldSourceZone
	role           HostRole
}

// firewalldSourceZone is a source zone for other nodes. It keeps the public
// ports too, because a source zone replaces the public zone for its sources.
type firewalldSourceZone struct {
	name    string
	sources []string
	ports   []string
}

// firewalldPeerZoneFiles includes previously managed zones so they can be removed.
var firewalldPeerZoneFiles = map[string]bool{
	"zones/" + firewalldPeerZone + ".xml":   true,
	"zones/" + firewalldServerZone + ".xml": true,
}

func wantFirewalld(sshPorts []string) firewalldWant {
	return wantFirewalldFor(sshPorts, FirewallMesh{})
}

func wantFirewalldFor(sshPorts []string, mesh FirewallMesh) firewalldWant {
	open := append([]string{}, sshPorts...)
	if mesh.Role != RoleWorker {
		open = append(open, "80", "443", "6443")
	}
	var public []string
	for _, p := range open {
		if !containsString(public, p+"/tcp") {
			public = append(public, p+"/tcp")
		}
	}
	want := firewalldWant{
		publicPorts:    public,
		publicServices: []string{"dhcpv6-client"},
		podPorts:       []string{"6443/tcp", kubeletPort + "/tcp", nodeExporterPort + "/tcp"},
		podSources:     []string{k3sPodCIDR, k3sServiceCIDR},
		role:           mesh.Role,
	}
	zones := map[string]*firewalldSourceZone{}
	for _, p := range mesh.Peers {
		name := firewalldPeerZone
		if p.Server {
			name = firewalldServerZone
		}
		z, ok := zones[name]
		if !ok {
			z = &firewalldSourceZone{name: name, ports: append(append([]string{}, public...), peerPorts(p)...)}
			zones[name] = z
		}
		z.sources = append(z.sources, p.Address)
	}
	for _, name := range []string{firewalldPeerZone, firewalldServerZone} {
		if z, ok := zones[name]; ok {
			want.peerZones = append(want.peerZones, *z)
		}
	}
	return want
}

// zoneNames lists every zone kip expects to be active.
func (w firewalldWant) zoneNames() []string {
	names := []string{"public", firewalldPodZone}
	for _, z := range w.peerZones {
		names = append(names, z.name)
	}
	return names
}

// files returns firewalld's permanent configuration as Kipper writes it,
// keyed by path relative to /etc/firewalld. The public zone file replaces the
// stock one in full.
func (w firewalldWant) files() map[string]string {
	zone := func(short string, services, ports, sources []string) string {
		var b strings.Builder
		b.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<zone>\n")
		fmt.Fprintf(&b, "  <short>%s</short>\n", short)
		b.WriteString("  <description>Managed by Kipper. Delete " + firewalldClaimPath + " before changing it by hand.</description>\n")
		for _, s := range sources {
			fmt.Fprintf(&b, "  <source address=\"%s\"/>\n", s)
		}
		for _, s := range services {
			fmt.Fprintf(&b, "  <service name=\"%s\"/>\n", s)
		}
		for _, p := range ports {
			port, proto, _ := strings.Cut(p, "/")
			fmt.Fprintf(&b, "  <port port=\"%s\" protocol=\"%s\"/>\n", port, proto)
		}
		b.WriteString("  <forward/>\n</zone>\n")
		return b.String()
	}

	files := map[string]string{
		"zones/public.xml":                   zone("Public", w.publicServices, w.publicPorts, nil),
		"zones/" + firewalldPodZone + ".xml": zone("Kipper pods", nil, w.podPorts, w.podSources),
	}
	for _, z := range w.peerZones {
		files["zones/"+z.name+".xml"] = zone("Kipper "+strings.TrimPrefix(z.name, "kipper-"), nil, z.ports, z.sources)
	}
	for _, p := range firewalldPolicies {
		masquerade := ""
		if p.masquerade {
			masquerade = "  <masquerade/>\n"
		}
		files["policies/"+p.name+".xml"] = fmt.Sprintf(
			"<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<policy target=\"ACCEPT\">\n"+
				"  <short>%s</short>\n  <ingress-zone name=\"%s\"/>\n  <egress-zone name=\"%s\"/>\n%s</policy>\n",
			p.name, p.ingress, p.egress, masquerade)
	}
	return files
}

var sshPortPattern = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)

// readSSHPorts reads all configured ports from sshd -T so firewall rules
// preserve access through each SSH port.
func readSSHPorts(client CommandRunner) ([]string, error) {
	out, err := client.Run(`sshd -T 2>/dev/null | awk '$1 == "port" {print $2}'`)
	if err != nil {
		return nil, fmt.Errorf("reading sshd's ports: %w", err)
	}
	var ports []string
	seen := map[string]bool{}
	for _, p := range strings.Fields(out) {
		if !sshPortPattern.MatchString(p) {
			return nil, fmt.Errorf("sshd reported %q, which is not a port", p)
		}
		if !seen[p] {
			seen[p] = true
			ports = append(ports, p)
		}
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("could not read the ports sshd listens on (sshd -T printed none), so kip left the firewall alone")
	}
	return ports, nil
}

// firewalldStockFiles are the files a freshly installed firewalld may carry in
// /etc/firewalld.
var firewalldStockFiles = map[string]bool{
	"firewalld.conf":         true,
	"lockdown-whitelist.xml": true,
	"zones/public.xml":       true,
}

var firewalldStockPublicServices = []string{"ssh", "dhcpv6-client", "cockpit"}

// firewalldInventoryCommand lists non-directory entries with their file types
// so the audit can reject symlinks and other non-regular files.
const firewalldInventoryCommand = "cd " + firewalldConfigDir + " && find . -mindepth 1 ! -type d -printf '%y %P\\n' | LC_ALL=C sort"

type firewalldSavedConfig struct {
	inventory string
	conf      string
	public    string
}

// changes reports saved customizations outside the files kip may replace.
// A claim permits kip's zone and policy files; adoption permits custom public-zone
// contents. Backups ending in .old follow the same filename allowlist.
func (c firewalldSavedConfig) changes(claimed, adopting bool, want firewalldWant) []string {
	ours := want.files()
	var changes []string
	for _, line := range strings.Split(c.inventory, "\n") {
		kind, file, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		base := strings.TrimSuffix(file, ".old")
		_, isOurs := ours[base]
		isOurs = isOurs || firewalldPeerZoneFiles[base]
		switch {
		case kind != "f":
			changes = append(changes, file+" is not a regular file")
		case firewalldStockFiles[base]:
		case claimed && isOurs:
		default:
			changes = append(changes, file)
		}
	}

	for _, line := range strings.Split(c.conf, "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch {
		case key == "DefaultZone" && value != "public":
			changes = append(changes, fmt.Sprintf("default zone %q", value))
		case key == "AllowZoneDrifting" && strings.EqualFold(value, "yes"):
			changes = append(changes, "zone drifting allowed")
		}
	}

	if c.public == "" || claimed || adopting {
		return changes
	}
	var zone struct {
		Target   string `xml:"target,attr"`
		Services []struct {
			Name string `xml:"name,attr"`
		} `xml:"service"`
		Other []struct {
			XMLName xml.Name
		} `xml:",any"`
	}
	if err := xml.Unmarshal([]byte(c.public), &zone); err != nil {
		return append(changes, "zones/public.xml cannot be read")
	}
	if zone.Target != "" && zone.Target != "default" {
		changes = append(changes, "public zone target "+zone.Target)
	}
	var services []string
	for _, svc := range zone.Services {
		services = append(services, svc.Name)
	}
	// Treat removed services as customizations too, so hardening choices are preserved.
	if !sameSet(services, firewalldStockPublicServices) {
		changes = append(changes, "public zone services "+strings.Join(services, " ")+" (stock is ssh dhcpv6-client cockpit)")
	}
	for _, o := range zone.Other {
		switch o.XMLName.Local {
		case "short", "description", "forward", "service":
		default:
			changes = append(changes, "public zone "+o.XMLName.Local+" entry")
		}
	}
	return changes
}

func readFirewalldSaved(client CommandRunner) (firewalldSavedConfig, error) {
	var c firewalldSavedConfig
	var err error
	if c.inventory, err = client.Run(firewalldInventoryCommand); err != nil {
		return c, fmt.Errorf("reading the saved firewalld configuration: %w", err)
	}
	if c.conf, err = client.Run("cat " + firewalldConfigDir + "/firewalld.conf 2>/dev/null || true"); err != nil {
		return c, fmt.Errorf("reading firewalld.conf: %w", err)
	}
	if strings.Contains(c.inventory, "f zones/public.xml\n") {
		if c.public, err = client.Run("cat " + firewalldConfigDir + "/zones/public.xml"); err != nil {
			return c, fmt.Errorf("reading the saved public zone: %w", err)
		}
	}
	return c, nil
}

// firewalldStateCommand groups runtime queries into sections for comparison.
// The q helper marks query failures; stderr is suppressed to keep diagnostics
// out of the parsed state. The panic query returns its yes/no result directly.
func firewalldStateCommand(want firewalldWant) string {
	return `q() { out=$("$@" 2>/dev/null) || { echo "FAILED: $*"; return 0; }; printf '%s\n' "$out"; }; ` +
		`echo "== state"; q firewall-cmd --state; ` +
		`echo "== default"; q firewall-cmd --get-default-zone; ` +
		`echo "== panic"; firewall-cmd --query-panic 2>/dev/null; ` +
		`echo "== direct"; q firewall-cmd --direct --get-all-chains; q firewall-cmd --direct --get-all-rules; q firewall-cmd --direct --get-all-passthroughs; ` +
		`echo "== active-zones"; q firewall-cmd --get-active-zones; ` +
		`echo "== policies"; q firewall-cmd --get-policies; ` +
		`for z in ` + strings.Join(want.zoneNames(), " ") + `; do echo "== zone $z"; q firewall-cmd --info-zone="$z"; done; ` +
		`for p in $(firewall-cmd --get-policies 2>/dev/null); do echo "== policy $p"; q firewall-cmd --info-policy="$p"; done`
}

// firewalldDumpCommand lists zones, policies, direct rules, and ipset names
// from runtime or permanent configuration. It omits zone interface bindings
// and status annotations when comparing the two configurations.
func firewalldDumpCommand(permanent bool) string {
	p := ""
	if permanent {
		p = " --permanent"
	}
	// Exclude stderr so transient diagnostics cannot appear as configuration drift.
	fc := "fc"
	return "set -e; fc() { firewall-cmd" + p + ` "$@" 2>/dev/null; }; ` + "zones=$(" + fc + " --get-zones); policies=$(" + fc + " --get-policies); " +
		`for z in $zones; do echo "== zone $z"; info=$(` + fc + ` --info-zone="$z"); printf '%s
' "$info" | sed -e '1s/ (.*)$//' -e '/^  interfaces:/d'; done; ` +
		`for x in $policies; do echo "== policy $x"; info=$(` + fc + ` --info-policy="$x"); printf '%s
' "$info" | sed '1s/ (.*)$//'; done; ` +
		`echo "== direct"; ` + fc + ` --direct --get-all-chains; ` + fc + ` --direct --get-all-rules; ` + fc + ` --direct --get-all-passthroughs; ` +
		`echo "== ipsets"; ` + fc + ` --get-ipsets`
}

// firewalldDrift names the sections of two dumps that differ.
func firewalldDrift(running, saved string) []string {
	split := func(dump string) map[string]string {
		sections := map[string]string{}
		name := ""
		for _, line := range strings.Split(dump, "\n") {
			if rest, ok := strings.CutPrefix(line, "== "); ok {
				name = rest
				sections[name] = ""
				continue
			}
			// Values within a field may be listed in any order.
			if m := firewalldInfoField.FindStringSubmatch(line); m != nil {
				values := strings.Fields(m[2])
				sort.Strings(values)
				line = "  " + m[1] + ": " + strings.Join(values, " ")
			}
			sections[name] += strings.TrimRight(line, " ") + "\n"
		}
		return sections
	}
	a, b := split(running), split(saved)
	for _, m := range []map[string]string{a, b} {
		for name, body := range m {
			m[name] = strings.TrimRight(body, "\n")
		}
	}
	var drift []string
	for name, body := range a {
		if other, ok := b[name]; !ok || other != body {
			drift = append(drift, name)
		}
	}
	for name := range b {
		if _, ok := a[name]; !ok {
			drift = append(drift, name)
		}
	}
	sort.Strings(drift)
	return drift
}

// firewalldInfo is one --info-zone or --info-policy listing, keyed by field.
type firewalldInfo map[string][]string

var firewalldInfoField = regexp.MustCompile(`^  ([a-z][a-z -]*): ?(.*)$`)

func parseFirewalldInfo(lines []string) firewalldInfo {
	info := firewalldInfo{}
	key := ""
	for _, line := range lines {
		if m := firewalldInfoField.FindStringSubmatch(line); m != nil {
			key = m[1]
			info[key] = append([]string{}, strings.Fields(m[2])...)
			continue
		}
		// Rich rules continue on their own lines.
		if key != "" && strings.TrimSpace(line) != "" {
			info[key] = append(info[key], strings.TrimSpace(line))
		}
	}
	return info
}

// allowHostIPv6Rule permits IPv6 ICMP accept rules in the stock policy.
var allowHostIPv6Rule = regexp.MustCompile(`^rule family="ipv6" icmp-type name="[a-z0-9-]+" accept$`)

// firewalldMismatches checks the queried runtime state against Kipper's expected
// zones and policies. Failed queries and missing required fields are problems.
func firewalldMismatches(state string, want firewalldWant) []string {
	sections := map[string][]string{}
	var name string
	var problems []string
	for _, line := range strings.Split(state, "\n") {
		if rest, ok := strings.CutPrefix(line, "== "); ok {
			name = rest
			sections[name] = []string{}
			continue
		}
		if strings.HasPrefix(line, "FAILED: ") {
			problems = append(problems, "could not run "+strings.TrimPrefix(line, "FAILED: "))
			continue
		}
		if name != "" {
			sections[name] = append(sections[name], line)
		}
	}
	first := func(section string) string {
		for _, l := range sections[section] {
			if t := strings.TrimSpace(l); t != "" {
				return t
			}
		}
		return ""
	}

	if got := first("state"); got != "running" {
		problems = append(problems, fmt.Sprintf("firewalld state is %q", got))
	}
	if got := first("default"); got != "public" {
		problems = append(problems, fmt.Sprintf("default zone is %q, want public", got))
	}
	if got := first("panic"); got != "no" {
		problems = append(problems, fmt.Sprintf("panic mode query answered %q", got))
	}
	for _, line := range sections["direct"] {
		if strings.TrimSpace(line) != "" {
			problems = append(problems, "direct configuration "+strings.TrimSpace(line))
		}
	}

	activeZones := map[string]string{}
	var zone string
	for _, line := range sections["active-zones"] {
		switch {
		case strings.TrimSpace(line) == "":
		case !strings.HasPrefix(line, " "):
			zone = strings.Fields(line)[0]
			activeZones[zone] += ""
		case zone != "":
			activeZones[zone] += strings.TrimSpace(line) + " "
		}
	}
	sourceZones := []string{firewalldPodZone}
	for _, pz := range want.peerZones {
		sourceZones = append(sourceZones, pz.name)
	}
	for z, bindings := range activeZones {
		switch {
		case z == "public" && strings.Contains(bindings, "sources:"):
			problems = append(problems, "public zone has sources bound: "+strings.TrimSpace(bindings))
		case containsString(sourceZones, z) && strings.Contains(bindings, "interfaces:"):
			problems = append(problems, z+" zone has interfaces bound: "+strings.TrimSpace(bindings))
		case z != "public" && !containsString(sourceZones, z):
			problems = append(problems, fmt.Sprintf("zone %s is active (%s)", z, strings.TrimSpace(bindings)))
		}
	}
	for _, z := range sourceZones {
		if _, ok := activeZones[z]; !ok {
			problems = append(problems, z+" zone is not active")
		}
	}

	expect := func(what string, info firewalldInfo, field string, want []string) {
		if got, ok := info[field]; !ok {
			problems = append(problems, fmt.Sprintf("%s does not report %s", what, field))
		} else if !sameSet(got, want) {
			problems = append(problems, fmt.Sprintf("%s %s is %q, want %q", what, field, strings.Join(got, " "), strings.Join(want, " ")))
		}
	}
	checkZone := func(z string, services, ports, sources []string) {
		info := parseFirewalldInfo(sections["zone "+z])
		what := "zone " + z
		expect(what, info, "target", []string{"default"})
		expect(what, info, "icmp-block-inversion", []string{"no"})
		expect(what, info, "sources", sources)
		expect(what, info, "services", services)
		expect(what, info, "ports", ports)
		expect(what, info, "protocols", nil)
		expect(what, info, "forward", []string{"yes"})
		expect(what, info, "masquerade", []string{"no"})
		expect(what, info, "forward-ports", nil)
		expect(what, info, "source-ports", nil)
		expect(what, info, "rich rules", nil)
		// Require default priorities when the listing includes them.
		for _, f := range []string{"ingress-priority", "egress-priority"} {
			if _, ok := info[f]; ok {
				expect(what, info, f, []string{"0"})
			}
		}
	}
	checkZone("public", want.publicServices, want.publicPorts, nil)
	checkZone(firewalldPodZone, nil, want.podPorts, want.podSources)
	for _, pz := range want.peerZones {
		checkZone(pz.name, nil, pz.ports, pz.sources)
	}

	policies := strings.Fields(strings.Join(sections["policies"], " "))
	expected := map[string]bool{"allow-host-ipv6": true}
	for _, p := range firewalldPolicies {
		expected[p.name] = true
		if !containsString(policies, p.name) {
			problems = append(problems, "policy "+p.name+" is not loaded")
			continue
		}
		problems = append(problems, kipperPolicyProblems(p.name, parseFirewalldInfo(sections["policy "+p.name]))...)
	}
	for _, p := range policies {
		if !expected[p] && !firewalldPolicyDisabled(parseFirewalldInfo(sections["policy "+p])) {
			problems = append(problems, "policy "+p+" is loaded")
		}
	}
	if containsString(policies, "allow-host-ipv6") {
		problems = append(problems, allowHostIPv6Problems(parseFirewalldInfo(sections["policy allow-host-ipv6"]))...)
	}

	sort.Strings(problems)
	return problems
}

func firewalldPolicyDisabled(info firewalldInfo) bool {
	return sameSet(info["disable"], []string{"yes"})
}

func kipperPolicyProblems(name string, info firewalldInfo) []string {
	what := "policy " + name
	var problems []string
	expect := func(field string, want []string) {
		if got, ok := info[field]; !ok {
			problems = append(problems, fmt.Sprintf("%s does not report %s", what, field))
		} else if !sameSet(got, want) {
			problems = append(problems, fmt.Sprintf("%s %s is %q, want %q", what, field, strings.Join(got, " "), strings.Join(want, " ")))
		}
	}
	for _, p := range firewalldPolicies {
		if p.name != name {
			continue
		}
		masquerade := "no"
		if p.masquerade {
			masquerade = "yes"
		}
		expect("target", []string{"ACCEPT"})
		expect("priority", []string{"-1"})
		expect("ingress-zones", []string{p.ingress})
		expect("egress-zones", []string{p.egress})
		expect("masquerade", []string{masquerade})
		for _, f := range []string{"services", "ports", "protocols", "forward-ports", "source-ports", "rich rules"} {
			expect(f, nil)
		}
		// Accept listings that omit disable; require our policies to be enabled when reported.
		if _, ok := info["disable"]; ok {
			expect("disable", []string{"no"})
		}
		return problems
	}
	return []string{what + " is not one of Kipper's"}
}

// allowHostIPv6Problems checks the policy settings and permits only ICMPv6
// accept rules to the host. It accepts any set of ICMPv6 types.
func allowHostIPv6Problems(info firewalldInfo) []string {
	const what = "policy allow-host-ipv6"
	var problems []string
	expect := func(field string, want []string) {
		if got, ok := info[field]; !ok {
			problems = append(problems, fmt.Sprintf("%s does not report %s", what, field))
		} else if !sameSet(got, want) {
			problems = append(problems, fmt.Sprintf("%s %s is %q, want %q", what, field, strings.Join(got, " "), strings.Join(want, " ")))
		}
	}
	expect("target", []string{"CONTINUE"})
	expect("ingress-zones", []string{"ANY"})
	expect("egress-zones", []string{"HOST"})
	expect("masquerade", []string{"no"})
	for _, f := range []string{"services", "ports", "protocols", "forward-ports", "source-ports"} {
		expect(f, nil)
	}
	for _, rule := range info["rich rules"] {
		if !allowHostIPv6Rule.MatchString(rule) {
			problems = append(problems, what+" has rule "+rule)
		}
	}
	return problems
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		if seen[s] == 0 {
			return false
		}
		seen[s]--
	}
	return true
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// firewalldRun gives each attempt its own backup, markers, and rollback unit.
// Tests use root to redirect paths into a temporary directory.
type firewalldRun struct {
	id, root string
	role     HostRole
}

func newFirewalldRun() (firewalldRun, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return firewalldRun{}, err
	}
	return firewalldRun{id: hex.EncodeToString(b)}, nil
}

func (r firewalldRun) state(name string) string { return r.root + firewalldStateDir + "/" + name }
func (r firewalldRun) backup() string           { return r.state("firewalld-backup-" + r.id) }
func (r firewalldRun) script() string           { return r.state("firewalld-rollback-" + r.id + ".sh") }
func (r firewalldRun) committed() string        { return r.state("firewalld-" + r.id + ".committed") }
func (r firewalldRun) loaded() string           { return r.state("firewalld-" + r.id + ".loaded") }
func (r firewalldRun) started() string          { return r.state("firewalld-" + r.id + ".rollback") }
func (r firewalldRun) restored() string         { return r.state("firewalld-" + r.id + ".restored") }
func (r firewalldRun) unit() string             { return firewalldRollbackUnit + "-" + r.id }

// rollbackScript restores saved files and, if loading was attempted, restores
// firewalld's prior service state. The "files" argument limits recovery to files.
// Commit and rollback markers enforce mutual exclusion under the shared lock.
func (r firewalldRun) rollbackScript(wasActive, wasEnabled, wasClaimed bool) string {
	etc := r.root + firewalldConfigDir
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# Restore the saved firewall configuration.\nset -eu\n")
	fmt.Fprintf(&b, "exec 9>%s\nflock 9\n", r.root+firewalldLock)
	b.WriteString("# Restore only an uncommitted run that still owns the reservation.\n")
	fmt.Fprintf(&b, "if [ -e %s ]; then exit 0; fi\n", r.committed())
	// Check for a completed restore before ownership: the timer releases the
	// reservation, and an installer waiting on the lock can resume afterward.
	fmt.Fprintf(&b, "if [ -e %s ]; then echo 'the previous firewall is already restored'; exit 0; fi\n", r.restored())
	fmt.Fprintf(&b, "if ! %s; then echo 'this firewall change no longer holds the reservation; nothing to restore'; exit 0; fi\n", r.owns())
	fmt.Fprintf(&b, "touch %s\n", r.started())
	fmt.Fprintf(&b, "[ -e %s/complete ] || { echo 'the firewalld backup is incomplete' >&2; exit 1; }\n", r.backup())
	fmt.Fprintf(&b, "rm -rf %[1]s.kipper-restore\ncp -a %[2]s/etc %[1]s.kipper-restore\nrm -rf %[1]s\nmv %[1]s.kipper-restore %[1]s\n", etc, r.backup())
	fmt.Fprintf(&b, "if command -v restorecon >/dev/null 2>&1; then restorecon -R %s; fi\n", etc)
	fmt.Fprintf(&b, "if [ \"${1:-}\" != files ] && [ -e %s ]; then\n", r.loaded())
	switch {
	case wasActive:
		b.WriteString("  firewall-cmd --reload >/dev/null || systemctl restart firewalld\n")
	case wasEnabled:
		b.WriteString("  systemctl stop firewalld\n")
	default:
		b.WriteString("  systemctl disable --now firewalld\n")
	}
	// Restart k3s to restore its network rules. Report restart failures while
	// allowing ownership cleanup to finish.
	unit := r.role.k3sUnit()
	fmt.Fprintf(&b, "  if systemctl is-active --quiet %[1]s; then systemctl restart %[1]s || echo '%[2]s; restart it with systemctl restart %[1]s'; fi\n", unit, k3sRestartFailed)
	b.WriteString("fi\n")
	if !wasClaimed {
		b.WriteString("rm -f " + r.root + firewalldClaimPath + "\n")
	}
	b.WriteString(r.releaseCommand() + "\n")
	// Stop the timer so later audits can proceed after a successful restore.
	fmt.Fprintf(&b, "systemctl stop %s.timer >/dev/null 2>&1 || true\n", r.unit())
	// Record success before deleting the script so the installer can confirm recovery.
	fmt.Fprintf(&b, "touch %s\n", r.restored())
	// Keep the rollback marker so a later commit can report why it was rejected.
	fmt.Fprintf(&b, "rm -rf %s %s %s\n", r.backup(), r.loaded(), r.script())
	return b.String()
}

func (r firewalldRun) reservation() string { return r.state("firewalld-run") }

// owns is a shell test that this run still holds the reservation.
func (r firewalldRun) owns() string {
	return fmt.Sprintf(`[ "$(cat %s 2>/dev/null)" = %s ]`, r.reservation(), r.id)
}

// locked runs command under the transaction lock, refusing once another run
// has taken the reservation over or this run's rollback has begun.
func (r firewalldRun) locked(command string) string {
	return fmt.Sprintf("(exec 9>%s && flock 9 || exit 1; "+
		"if [ -e %s ]; then echo 'the rollback has already restored the previous firewall' >&2; exit 1; fi; "+
		"if ! %s; then echo 'another firewall change took over from this one' >&2; exit 1; fi; %s)",
		r.root+firewalldLock, r.started(), r.owns(), command)
}

// reserveCommand reserves the firewall for this run. A reservation can be
// replaced after 15 minutes if its rollback timer is inactive. Subsequent
// commands from the previous run fail the ownership check in locked.
func (r firewalldRun) reserveCommand() string {
	return fmt.Sprintf(`(mkdir -p %[1]s && exec 9>%[2]s && flock 9 || exit 1; f=%[3]s; if [ -s "$f" ] && ! %[6]s; then other=$(cat "$f"); `+
		`case "$other" in *[!0-9a-f]*) echo "$f holds $other, which is not a run" >&2; exit 1 ;; esac; `+
		`if systemctl is-active --quiet "%[4]s-$other.timer" || [ -n "$(find "$f" -mmin -15)" ]; then echo "another firewall change ($other) is in progress; retry once its rollback timer is inactive and its reservation in $f is at least 15 minutes old" >&2; exit 1; fi; fi; `+
		`printf '%%s\n' %[5]s > "$f.tmp" && mv -f "$f.tmp" "$f")`,
		r.root+firewalldStateDir, r.root+firewalldLock, r.reservation(), firewalldRollbackUnit, r.id, r.owns())
}

// releaseCommand releases this run's reservation. The caller must hold the lock.
func (r firewalldRun) releaseCommand() string {
	return fmt.Sprintf(`if %s; then rm -f %s; fi`, r.owns(), r.reservation())
}

func (r firewalldRun) releaseLockedCommand() string {
	return fmt.Sprintf("(exec 9>%s && flock 9 && %s)", r.root+firewalldLock, r.releaseCommand())
}

// commitCommand keeps this run's firewall if rollback has not begun and removes
// its rollback files. Releasing the reservation prevents a late rollback even
// after the commit marker is removed.
func (r firewalldRun) commitCommand() string {
	return r.locked(fmt.Sprintf("touch %s && %s", r.committed(), r.releaseCommand())) +
		fmt.Sprintf(" && { systemctl stop %[1]s.timer %[1]s.service >/dev/null 2>&1; rm -rf %s %s %s %s %s; }", r.unit(), r.backup(), r.script(), r.started(), r.loaded(), r.committed())
}

// k3sRestartFailed also identifies restart failures in rollback output.
const k3sRestartFailed = "k3s did not restart after the firewall was restored"

// pendingRollbackCommand prints any rollback unit still waiting or running.
const pendingRollbackCommand = "systemctl list-units --all --no-legend --plain '" + firewalldRollbackUnit + "-*' | awk '($3 == \"active\" || $3 == \"activating\") && $4 != \"elapsed\" {print $1}'"

// configureFirewalld backs up, loads, and verifies Kipper's configuration under
// a rollback timer. SSH and server-driven runs verify a new SSH connection
// before commit. Other local runs may start a stopped firewall or verify a matching one.
func configureFirewalld(client FirewallRunner, host HostOS, consent FirewallConsent, mesh FirewallMesh) error {
	install := host.InstallPackagesCommand(false, "firewalld")
	if install == "" {
		return fmt.Errorf("installing firewalld: kip cannot install packages on %s", host.ID)
	}
	if err := ensureInstalled(client, "firewall-cmd", "firewalld", install, os.Stdout); err != nil {
		return err
	}

	sshPorts, err := readSSHPorts(client)
	if err != nil {
		return err
	}

	run, err := newFirewalldRun()
	if err != nil {
		return fmt.Errorf("naming the firewall change: %w", err)
	}
	run.role = mesh.Role
	if _, err := client.Run(run.reserveCommand()); err != nil {
		return fmt.Errorf("reserving the firewall for this change: %w", err)
	}
	// Inspect after reserving so cooperating runs cannot change the configuration
	// between inspection and application.
	seen, err := inspectFirewalld(client, sshPorts, consent, mesh)
	if err != nil || seen.done {
		if _, releaseErr := client.Run(run.releaseLockedCommand()); releaseErr != nil {
			return errors.Join(err, fmt.Errorf("releasing the firewall reservation: %w", releaseErr))
		}
		return err
	}
	wasActive, local, want := seen.wasActive, seen.local, seen.want

	if err := backUpFirewalld(client, run, seen); err != nil {
		if _, releaseErr := client.Run(run.releaseLockedCommand()); releaseErr != nil {
			return errors.Join(err, fmt.Errorf("releasing the firewall reservation: %w", releaseErr))
		}
		return err
	}
	// Arm recovery before changing firewall files in case the installer exits.
	arm := fmt.Sprintf("systemd-run --quiet --unit=%s --on-active=%d --timer-property=RemainAfterElapse=no /bin/sh %s", run.unit(), firewalldRollbackSeconds, run.script())
	if _, err := client.Run(run.locked(arm)); err != nil {
		return fmt.Errorf("could not schedule firewall rollback; the new rules were not loaded: %w", errors.Join(err, rollBackFirewalld(client, run, false)))
	}
	if _, err := client.Run(run.locked(claimCommandFor(firewalldClaimPath, "firewalld configuration"))); err != nil {
		return fmt.Errorf("claiming the firewall: %w", errors.Join(err, rollBackFirewalld(client, run, false)))
	}
	if err := writeFirewalldFiles(client, run, want, wasActive); err != nil {
		return errors.Join(err, rollBackFirewalld(client, run, false))
	}

	load := "systemctl enable --now firewalld"
	if wasActive {
		load = "firewall-cmd --reload"
	}
	if _, err := client.Run(run.locked("touch " + run.loaded() + " && " + load + " >/dev/null")); err != nil {
		return fmt.Errorf("loading the firewalld configuration: %w", errors.Join(err, rollBackFirewalld(client, run, true)))
	}

	state, err := client.Run(firewalldStateCommand(want))
	if err != nil {
		return fmt.Errorf("reading the loaded firewalld configuration: %w", errors.Join(err, rollBackFirewalld(client, run, true)))
	}
	if problems := firewalldMismatches(state, want); len(problems) > 0 {
		return fmt.Errorf("firewalld loaded a different configuration than kip wrote (%s): %w",
			strings.Join(problems, "; "), rollBackFirewalld(client, run, true))
	}
	if !local {
		if err := client.ConfirmFreshConnection(); err != nil {
			return fmt.Errorf("new SSH connections fail with the new firewall: %w", errors.Join(err, rollBackFirewalld(client, run, true)))
		}
	}

	if _, err := client.Run(run.commitCommand()); err != nil {
		return fmt.Errorf("could not confirm the firewall commit; automatic rollback was scheduled with a %d-second delay: %w", firewalldRollbackSeconds, err)
	}

	// Restart running k3s to restore its network rules after loading firewalld.
	unit := mesh.Role.k3sUnit()
	if _, err := client.Run(fmt.Sprintf("if systemctl is-active --quiet %[1]s; then systemctl restart %[1]s; fi", unit)); err != nil {
		return fmt.Errorf("restarting %s after loading the firewall: %w", unit, err)
	}
	return nil
}

// firewalldSeen holds the inspection result for the reserved configuration attempt.
type firewalldSeen struct {
	wasActive, wasEnabled, wasClaimed, local, done bool
	want                                           firewalldWant
}

// inspectFirewalld checks ownership, connectivity, and configuration before
// making changes. The caller must hold the run's reservation.
func inspectFirewalld(client FirewallRunner, sshPorts []string, consent FirewallConsent, mesh FirewallMesh) (firewalldSeen, error) {
	audit, err := AuditFirewall(client)
	if err != nil {
		return firewalldSeen{}, fmt.Errorf("re-auditing firewall: %w", err)
	}
	adopting := consent.Adopt && audit.Adoptable
	if audit.BlocksConfiguration() && !adopting {
		return firewalldSeen{}, fmt.Errorf("a %s firewall became active on this host, so kip left it alone", audit.Existing)
	}
	if strings.Contains(audit.Existing, "ufw") {
		return firewalldSeen{}, fmt.Errorf("ufw is active on this host, and kip uses firewalld here. Switch ufw off or keep it as your own firewall with --firewall=false")
	}
	wasActive := audit.Existing == "firewalld"
	wasClaimed := firewalldClaimed(client)

	pending, err := client.Run(pendingRollbackCommand)
	if err != nil {
		return firewalldSeen{}, fmt.Errorf("checking for an earlier firewall rollback: %w", err)
	}
	if units := strings.Fields(pending); len(units) > 0 {
		return firewalldSeen{}, fmt.Errorf("an earlier firewall change is still waiting to be rolled back (%s); run the command again in %d seconds", strings.Join(units, ", "), firewalldRollbackSeconds)
	}

	// Establish that fresh connections work before using them to validate the change.
	baseline := client.ConfirmFreshConnection()
	local := errors.Is(baseline, localrun.ErrNoConnection)
	if baseline != nil && !local {
		return firewalldSeen{}, fmt.Errorf("kip could not open a second connection to the host, so it left the firewall alone: %w", baseline)
	}
	if adopting && local {
		return firewalldSeen{}, fmt.Errorf("taking over a running firewalld needs a check from outside the host, so kip left it alone; run kip install over SSH or with --via-server")
	}

	want := wantFirewalldFor(sshPorts, mesh)
	saved, err := readFirewalldSaved(client)
	if err != nil {
		return firewalldSeen{}, err
	}
	if changes := saved.changes(wasClaimed, adopting, want); len(changes) > 0 {
		stopped := ""
		if !wasActive {
			stopped = " firewalld is stopped, and --adopt-firewall only takes over a running firewalld."
		}
		return firewalldSeen{}, fmt.Errorf("the saved firewalld configuration has custom changes (%s). Remove them to let kip configure it, or use --firewall=false to manage it yourself. For manual management, also delete %s if present.%s",
			strings.Join(changes, ", "), firewalldClaimPath, stopped)
	}
	// Interactive consent applies to the previewed configuration.
	if adopting && consent.Fingerprint != "" {
		current, err := currentFirewalldFingerprint(client, sshPorts, mesh)
		if err != nil {
			return firewalldSeen{}, err
		}
		if current != consent.Fingerprint {
			return firewalldSeen{}, fmt.Errorf("the running firewalld changed after you agreed to its takeover, so kip left it alone; run the command again to see the new changes")
		}
	}

	enabled, err := client.Run("systemctl is-enabled firewalld 2>/dev/null || true")
	if err != nil {
		return firewalldSeen{}, fmt.Errorf("checking whether firewalld starts at boot: %w", err)
	}
	seen := firewalldSeen{wasActive: wasActive, wasEnabled: strings.TrimSpace(enabled) == "enabled", wasClaimed: wasClaimed, local: local, want: want}
	if wasActive && local {
		seen.done = true
		return seen, checkLocalFirewalld(client, want)
	}
	if wasActive {
		if err := requireSavedMatchesRunning(client); err != nil {
			return seen, err
		}
	}
	return seen, nil
}

// checkLocalFirewalld verifies that a claimed firewall already matches the
// expected runtime state and saved files when no external SSH check is available.
func checkLocalFirewalld(client CommandRunner, want firewalldWant) error {
	state, err := client.Run(firewalldStateCommand(want))
	if err != nil {
		return fmt.Errorf("reading the loaded firewalld configuration: %w", err)
	}
	problems := firewalldMismatches(state, want)
	files := want.files()
	paths := sortedKeys(files)
	for _, p := range paths {
		got, err := client.Run("cat " + firewalldConfigDir + "/" + p)
		if err != nil || got != files[p] {
			problems = append(problems, "saved "+p+" differs")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("firewalld is running with a configuration that differs from kip's (%s). Run kip install over SSH to change it, or stop firewalld first", strings.Join(problems, "; "))
	}
	return nil
}

func firewalldClaimed(client CommandRunner) bool {
	out, err := client.Run("test -s " + firewalldClaimPath + " && echo claimed || true")
	return err == nil && strings.TrimSpace(out) == "claimed"
}

// requireSavedMatchesRunning checks that reloading the saved configuration can
// restore the current rules. Runtime-only rules, including SSH access, would be lost.
func requireSavedMatchesRunning(client CommandRunner) error {
	running, err := client.Run(firewalldDumpCommand(false))
	if err != nil {
		return fmt.Errorf("reading the running firewalld configuration: %w", err)
	}
	saved, err := client.Run(firewalldDumpCommand(true))
	if err != nil {
		return fmt.Errorf("reading the saved firewalld configuration: %w", err)
	}
	if drift := firewalldDrift(running, saved); len(drift) > 0 {
		return fmt.Errorf("firewalld's running configuration differs from its saved one (%s), so a rollback could not restore it. "+
			"Save the running rules with 'firewall-cmd --runtime-to-permanent' or drop them with 'firewall-cmd --reload', then run the command again",
			strings.Join(drift, ", "))
	}
	return nil
}

func backUpFirewalld(client CommandRunner, run firewalldRun, seen firewalldSeen) error {
	backup := fmt.Sprintf("(set -e; mkdir -p %[1]s; rm -rf %[2]s; mkdir -p %[2]s; cp -a %[3]s %[2]s/etc; touch %[2]s/complete)",
		firewalldStateDir, run.backup(), firewalldConfigDir)
	if _, err := client.Run(run.locked(backup)); err != nil {
		return fmt.Errorf("backing up the firewalld configuration: %w", err)
	}
	write := fmt.Sprintf("umask 077 && cat > %[1]s.tmp && mv -f %[1]s.tmp %[1]s", run.script())
	if _, err := client.RunStdin(run.locked(write), strings.NewReader(run.rollbackScript(seen.wasActive, seen.wasEnabled, seen.wasClaimed))); err != nil {
		return fmt.Errorf("writing the firewalld rollback: %w", err)
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// writeFirewalldFiles writes zones and policies, restores SELinux labels when
// restorecon is available, and validates the saved configuration before loading.
func writeFirewalldFiles(client CommandRunner, run firewalldRun, want firewalldWant, running bool) error {
	files := want.files()
	for _, rel := range sortedKeys(files) {
		p := firewalldConfigDir + "/" + rel
		dir := p[:strings.LastIndex(p, "/")]
		write := fmt.Sprintf("mkdir -p %[1]s && umask 077 && cat > %[2]s.tmp && mv -f %[2]s.tmp %[2]s", dir, p)
		if _, err := client.RunStdin(run.locked(write), strings.NewReader(files[rel])); err != nil {
			return fmt.Errorf("writing %s: %w", p, err)
		}
	}
	check := "firewall-offline-cmd --check-config"
	if running {
		check = "firewall-cmd --check-config"
	}
	// Remove previously managed peer zones that are no longer needed.
	var stale []string
	for _, name := range []string{firewalldPeerZone, firewalldServerZone} {
		if _, ok := files["zones/"+name+".xml"]; !ok {
			stale = append(stale, firewalldConfigDir+"/zones/"+name+".xml")
		}
	}
	if len(stale) > 0 {
		if _, err := client.Run(run.locked("rm -f " + strings.Join(stale, " "))); err != nil {
			return fmt.Errorf("removing peer zones no longer needed: %w", err)
		}
	}
	relabel := "if command -v restorecon >/dev/null 2>&1; then restorecon -R " + firewalldConfigDir + "; fi"
	if _, err := client.Run(run.locked(relabel + " && " + check)); err != nil {
		return fmt.Errorf("firewalld rejected kip's configuration: %w", err)
	}
	return nil
}

// rollBackFirewalld restores the backup immediately. If loading was attempted,
// it also restores the service state; otherwise it restores only files.
// A failed restore leaves any armed timer available to retry.
func rollBackFirewalld(client CommandRunner, run firewalldRun, loaded bool) error {
	mode := " files"
	if loaded {
		mode = ""
	}
	// The timer deletes the script after restoring the firewall. Check the
	// success marker after a script error to recognize a completed restore.
	restoredByTimer := func() bool {
		out, err := client.Run(fmt.Sprintf("if [ -e %s ]; then echo restored; fi", run.restored()))
		return err == nil && strings.TrimSpace(out) == "restored"
	}
	out, err := client.Run("/bin/sh " + run.script() + mode)
	if err == nil && strings.Contains(out, k3sRestartFailed) {
		unit := run.role.k3sUnit()
		return fmt.Errorf("kip restored the previous firewall; %[1]s restart failed. Run systemctl restart %[1]s", unit)
	}
	if err == nil && strings.Contains(out, "no longer holds the reservation") {
		return fmt.Errorf("another firewall change holds the firewall now, so kip left it alone")
	}
	if err != nil && restoredByTimer() {
		return fmt.Errorf("kip restored the previous firewall")
	}
	if err != nil {
		return fmt.Errorf("restoring the previous firewall failed too (%w). An armed rollback timer may retry after its %d-second delay. If recovery fails, restore the backup from the provider's console", err, firewalldRollbackSeconds)
	}
	// The released reservation makes any later timer invocation harmless.
	_, _ = client.Run("systemctl stop " + run.unit() + ".timer >/dev/null 2>&1")
	return fmt.Errorf("kip restored the previous firewall")
}

// ensureInstalled runs install when binary is missing from the host's PATH.
// It announces the installation on out before the quiet command runs so users
// know work is underway while waiting for package mirrors.
func ensureInstalled(client CommandRunner, binary, pkg, install string, out io.Writer) error {
	present, err := client.Run("command -v " + binary + " >/dev/null 2>&1 && echo present || true")
	if err != nil {
		return fmt.Errorf("checking for %s: %w", pkg, err)
	}
	if strings.TrimSpace(present) == "present" {
		return nil
	}
	_, _ = fmt.Fprint(out, packageInstallNotice([]string{pkg}))
	if _, err := client.Run(install + " >/dev/null"); err != nil {
		return fmt.Errorf("installing %s: %w", pkg, err)
	}
	return nil
}
