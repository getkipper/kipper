package installer

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
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
}

func wantFirewalld(sshPort string) firewalldWant {
	return firewalldWant{
		publicPorts:    []string{sshPort + "/tcp", "80/tcp", "443/tcp", "6443/tcp"},
		publicServices: []string{"dhcpv6-client"},
		podPorts:       []string{"6443/tcp", kubeletPort + "/tcp", nodeExporterPort + "/tcp"},
		podSources:     []string{k3sPodCIDR, k3sServiceCIDR},
	}
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

// firewalldStockFiles are the files a freshly installed firewalld may carry in
// /etc/firewalld.
var firewalldStockFiles = map[string]bool{
	"firewalld.conf":         true,
	"lockdown-whitelist.xml": true,
	"zones/public.xml":       true,
}

var firewalldStockPublicServices = map[string]bool{"ssh": true, "dhcpv6-client": true, "cockpit": true}

// firewalldInventoryCommand lists non-directory entries with their file types
// so the audit can reject symlinks and other non-regular files.
const firewalldInventoryCommand = "cd " + firewalldConfigDir + " && find . -mindepth 1 ! -type d -printf '%y %P\\n'"

type firewalldSavedConfig struct {
	inventory string
	conf      string
	public    string
}

// changes checks saved files and selected settings for operator customizations.
// A claim also permits Kipper's zone and policy files and skips public-zone
// content checks. Backups ending in .old follow the same filename allowlist.
func (c firewalldSavedConfig) changes(claimed bool, want firewalldWant) []string {
	ours := want.files()
	var changes []string
	for _, line := range strings.Split(c.inventory, "\n") {
		kind, file, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		base := strings.TrimSuffix(file, ".old")
		_, isOurs := ours[base]
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
		switch {
		case key == "DefaultZone" && value != "public":
			changes = append(changes, fmt.Sprintf("default zone %q", value))
		case key == "AllowZoneDrifting" && strings.EqualFold(value, "yes"):
			changes = append(changes, "zone drifting allowed")
		}
	}

	if c.public == "" || claimed {
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
	for _, s := range zone.Services {
		if !firewalldStockPublicServices[s.Name] {
			changes = append(changes, "public zone service "+s.Name)
		}
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

// firewalldStateCommand groups runtime queries under "== name" headings.
// The q helper marks failed queries with "FAILED:"; the panic query is read directly.
const firewalldStateCommand = `q() { out=$("$@" 2>&1) || { echo "FAILED: $*"; return 0; }; printf '%s\n' "$out"; }; ` +
	`echo "== state"; q firewall-cmd --state; ` +
	`echo "== default"; q firewall-cmd --get-default-zone; ` +
	`echo "== panic"; firewall-cmd --query-panic 2>&1; ` +
	`echo "== direct"; q firewall-cmd --direct --get-all-chains; q firewall-cmd --direct --get-all-rules; q firewall-cmd --direct --get-all-passthroughs; ` +
	`echo "== active-zones"; q firewall-cmd --get-active-zones; ` +
	`echo "== policies"; q firewall-cmd --get-policies; ` +
	`for z in public ` + firewalldPodZone + `; do echo "== zone $z"; q firewall-cmd --info-zone="$z"; done; ` +
	`for p in $(firewall-cmd --get-policies 2>/dev/null); do echo "== policy $p"; q firewall-cmd --info-policy="$p"; done`

// firewalldDumpCommand lists zones, policies, direct rules, and ipset names
// from runtime or permanent configuration. It omits zone interface bindings
// and status annotations when comparing the two configurations.
func firewalldDumpCommand(permanent bool) string {
	p := ""
	if permanent {
		p = " --permanent"
	}
	fc := "firewall-cmd" + p
	return "set -e; zones=$(" + fc + " --get-zones); policies=$(" + fc + " --get-policies); " +
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
var allowHostIPv6Rule = regexp.MustCompile(`^rule family="ipv6" icmp-type name="[a-z-]+" accept$`)

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
	for z, bindings := range activeZones {
		switch {
		case z == "public" && strings.Contains(bindings, "sources:"):
			problems = append(problems, "public zone has sources bound: "+strings.TrimSpace(bindings))
		case z == firewalldPodZone && strings.Contains(bindings, "interfaces:"):
			problems = append(problems, firewalldPodZone+" zone has interfaces bound: "+strings.TrimSpace(bindings))
		case z != "public" && z != firewalldPodZone:
			problems = append(problems, fmt.Sprintf("zone %s is active (%s)", z, strings.TrimSpace(bindings)))
		}
	}
	if _, ok := activeZones[firewalldPodZone]; !ok {
		problems = append(problems, firewalldPodZone+" zone is not active")
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

	policies := strings.Fields(strings.Join(sections["policies"], " "))
	expected := map[string]bool{"allow-host-ipv6": true}
	for _, p := range firewalldPolicies {
		expected[p.name] = true
		if !containsString(policies, p.name) {
			problems = append(problems, "policy "+p.name+" is not loaded")
			continue
		}
		info := parseFirewalldInfo(sections["policy "+p.name])
		what := "policy " + p.name
		masquerade := "no"
		if p.masquerade {
			masquerade = "yes"
		}
		expect(what, info, "target", []string{"ACCEPT"})
		expect(what, info, "priority", []string{"-1"})
		expect(what, info, "ingress-zones", []string{p.ingress})
		expect(what, info, "egress-zones", []string{p.egress})
		expect(what, info, "masquerade", []string{masquerade})
		for _, f := range []string{"services", "ports", "protocols", "forward-ports", "source-ports", "rich rules"} {
			expect(what, info, f, nil)
		}
	}
	for _, p := range policies {
		if !expected[p] {
			problems = append(problems, "policy "+p+" is loaded")
		}
	}
	if containsString(policies, "allow-host-ipv6") {
		info := parseFirewalldInfo(sections["policy allow-host-ipv6"])
		what := "policy allow-host-ipv6"
		expect(what, info, "target", []string{"CONTINUE"})
		expect(what, info, "ingress-zones", []string{"ANY"})
		expect(what, info, "egress-zones", []string{"HOST"})
		expect(what, info, "masquerade", []string{"no"})
		for _, f := range []string{"services", "ports", "protocols", "forward-ports", "source-ports"} {
			expect(what, info, f, nil)
		}
		for _, rule := range info["rich rules"] {
			if !allowHostIPv6Rule.MatchString(rule) {
				problems = append(problems, what+" has rule "+rule)
			}
		}
	}

	sort.Strings(problems)
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
type firewalldRun struct{ id, root string }

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
func (r firewalldRun) started() string          { return r.state("firewalld-" + r.id + ".rollback") }
func (r firewalldRun) unit() string             { return firewalldRollbackUnit + "-" + r.id }

// rollbackScript restores saved files and reloads or stops firewalld according
// to its previous state. The "files" argument restores only the saved files.
// Commit and rollback markers enforce mutual exclusion under the shared lock.
func (r firewalldRun) rollbackScript(wasActive, wasClaimed bool) string {
	etc := r.root + firewalldConfigDir
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# Restore the saved firewall configuration.\nset -eu\n")
	fmt.Fprintf(&b, "exec 9>%s\nflock 9\n", r.root+firewalldLock)
	b.WriteString("# Restore only an uncommitted run that still owns the reservation.\n")
	fmt.Fprintf(&b, "if [ -e %s ]; then exit 0; fi\n", r.committed())
	fmt.Fprintf(&b, "if ! %s; then echo 'this firewall change no longer holds the reservation; nothing to restore'; exit 0; fi\n", r.owns())
	fmt.Fprintf(&b, "touch %s\n", r.started())
	fmt.Fprintf(&b, "[ -e %s/complete ] || { echo 'the firewalld backup is incomplete' >&2; exit 1; }\n", r.backup())
	fmt.Fprintf(&b, "rm -rf %[1]s.kipper-restore\ncp -a %[2]s/etc %[1]s.kipper-restore\nrm -rf %[1]s\nmv %[1]s.kipper-restore %[1]s\n", etc, r.backup())
	fmt.Fprintf(&b, "if command -v restorecon >/dev/null 2>&1; then restorecon -R %s; fi\n", etc)
	b.WriteString("if [ \"${1:-}\" != files ]; then\n")
	if wasActive {
		b.WriteString("  firewall-cmd --reload >/dev/null || systemctl restart firewalld\n")
	} else {
		b.WriteString("  systemctl disable --now firewalld\n")
	}
	b.WriteString("fi\n")
	if !wasClaimed {
		b.WriteString("rm -f " + r.root + firewalldClaimPath + "\n")
	}
	b.WriteString(r.releaseCommand() + "\n")
	// Stop the timer so later audits can proceed after a successful restore.
	fmt.Fprintf(&b, "systemctl stop %s.timer >/dev/null 2>&1 || true\n", r.unit())
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
		`if systemctl is-active --quiet "%[4]s-$other.timer" || [ -n "$(find "$f" -mmin -15)" ]; then echo "another firewall change ($other) is in progress" >&2; exit 1; fi; fi; `+
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

// commitCommand records that the run's firewall stays, unless its rollback has
// already begun, then clears this run's rollback files.
func (r firewalldRun) commitCommand() string {
	return r.locked(fmt.Sprintf("touch %s && %s", r.committed(), r.releaseCommand())) +
		fmt.Sprintf(" && { systemctl stop %s.timer >/dev/null 2>&1; rm -rf %s %s %s; }", r.unit(), r.backup(), r.script(), r.started())
}

// pendingRollbackCommand prints any rollback unit still waiting or running.
const pendingRollbackCommand = "systemctl list-units --all --no-legend --plain '" + firewalldRollbackUnit + "-*' | awk '($3 == \"active\" || $3 == \"activating\") && $4 != \"elapsed\" {print $1}'"

// configureFirewalld backs up, loads, and verifies Kipper's configuration under
// a rollback timer. Remote runs also require a new SSH connection before commit.
// Local runs may start a stopped firewall or verify an already matching one.
func configureFirewalld(client FirewallRunner, host HostOS) error {
	install := host.InstallPackagesCommand(false, "firewalld")
	if install == "" {
		return fmt.Errorf("installing firewalld: kip cannot install packages on %s", host.ID)
	}
	if _, err := client.Run("command -v firewall-cmd >/dev/null 2>&1 || " + install + " >/dev/null"); err != nil {
		return fmt.Errorf("installing firewalld: %w", err)
	}

	sshPort, err := readSSHPort(client)
	if err != nil {
		return err
	}

	run, err := newFirewalldRun()
	if err != nil {
		return fmt.Errorf("naming the firewall change: %w", err)
	}
	if _, err := client.Run(run.reserveCommand()); err != nil {
		return fmt.Errorf("reserving the firewall for this change: %w", err)
	}
	// Inspect after reserving so cooperating runs cannot change the configuration
	// between inspection and application.
	seen, err := inspectFirewalld(client, sshPort)
	if err != nil || seen.done {
		if _, releaseErr := client.Run(run.releaseLockedCommand()); releaseErr != nil {
			return errors.Join(err, fmt.Errorf("releasing the firewall reservation: %w", releaseErr))
		}
		return err
	}
	wasActive, wasClaimed, local, want := seen.wasActive, seen.wasClaimed, seen.local, seen.want

	if err := backUpFirewalld(client, run, wasActive, wasClaimed); err != nil {
		if _, releaseErr := client.Run(run.releaseLockedCommand()); releaseErr != nil {
			return errors.Join(err, fmt.Errorf("releasing the firewall reservation: %w", releaseErr))
		}
		return err
	}
	if _, err := client.Run(run.locked(claimCommandFor(firewalldClaimPath, "firewalld configuration"))); err != nil {
		return fmt.Errorf("claiming the firewall: %w", errors.Join(err, rollBackFirewalld(client, run, false)))
	}
	if err := writeFirewalldFiles(client, run, want, wasActive); err != nil {
		return errors.Join(err, rollBackFirewalld(client, run, false))
	}

	arm := fmt.Sprintf("systemd-run --quiet --unit=%s --on-active=%d --timer-property=RemainAfterElapse=no /bin/sh %s", run.unit(), firewalldRollbackSeconds, run.script())
	if _, err := client.Run(run.locked(arm)); err != nil {
		return fmt.Errorf("could not schedule firewall rollback; the new rules were not loaded: %w", errors.Join(err, rollBackFirewalld(client, run, false)))
	}

	load := "systemctl enable --now firewalld"
	if wasActive {
		load = "firewall-cmd --reload"
	}
	if _, err := client.Run(run.locked(load + " >/dev/null")); err != nil {
		return fmt.Errorf("loading the firewalld configuration: %w", errors.Join(err, rollBackFirewalld(client, run, true)))
	}

	state, err := client.Run(firewalldStateCommand)
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

	// Loading firewalld rebuilds its rules, so a running k3s restarts to put
	// its own netfilter rules back.
	if _, err := client.Run("if systemctl is-active --quiet k3s; then systemctl restart k3s; fi"); err != nil {
		return fmt.Errorf("restarting k3s after loading the firewall: %w", err)
	}
	return nil
}

// firewalldSeen holds the inspection result for the reserved configuration attempt.
type firewalldSeen struct {
	wasActive, wasClaimed, local, done bool
	want                               firewalldWant
}

// inspectFirewalld checks ownership, connectivity, and configuration before
// making changes. The caller must hold the run's reservation.
func inspectFirewalld(client FirewallRunner, sshPort string) (firewalldSeen, error) {
	audit, err := AuditFirewall(client)
	if err != nil {
		return firewalldSeen{}, fmt.Errorf("re-auditing firewall: %w", err)
	}
	if audit.BlocksConfiguration() {
		return firewalldSeen{}, fmt.Errorf("a %s firewall became active on this host, so kip left it alone", audit.Existing)
	}
	if audit.Existing == "ufw" {
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

	want := wantFirewalld(sshPort)
	saved, err := readFirewalldSaved(client)
	if err != nil {
		return firewalldSeen{}, err
	}
	if changes := saved.changes(wasClaimed, want); len(changes) > 0 {
		return firewalldSeen{}, fmt.Errorf("the saved firewalld configuration has custom changes (%s). Remove them to let kip configure it, or use --firewall=false to manage it yourself. For manual management, also delete %s if present",
			strings.Join(changes, ", "), firewalldClaimPath)
	}

	seen := firewalldSeen{wasActive: wasActive, wasClaimed: wasClaimed, local: local, want: want}
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
// expected runtime state and saved files. Local execution cannot verify SSH access.
func checkLocalFirewalld(client CommandRunner, want firewalldWant) error {
	state, err := client.Run(firewalldStateCommand)
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

func backUpFirewalld(client CommandRunner, run firewalldRun, wasActive, wasClaimed bool) error {
	backup := fmt.Sprintf("(set -e; mkdir -p %[1]s; rm -rf %[2]s; mkdir -p %[2]s; cp -a %[3]s %[2]s/etc; touch %[2]s/complete)",
		firewalldStateDir, run.backup(), firewalldConfigDir)
	if _, err := client.Run(run.locked(backup)); err != nil {
		return fmt.Errorf("backing up the firewalld configuration: %w", err)
	}
	write := fmt.Sprintf("umask 077 && cat > %[1]s.tmp && mv -f %[1]s.tmp %[1]s", run.script())
	if _, err := client.RunStdin(run.locked(write), strings.NewReader(run.rollbackScript(wasActive, wasClaimed))); err != nil {
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
	out, err := client.Run("/bin/sh " + run.script() + mode)
	if err == nil && strings.Contains(out, "no longer holds the reservation") {
		return fmt.Errorf("another firewall change holds the firewall now, so kip left it alone")
	}
	if err != nil {
		return fmt.Errorf("restoring the previous firewall failed too (%w). An armed rollback timer may retry after its %d-second delay. If recovery fails, restore the backup from the provider's console", err, firewalldRollbackSeconds)
	}
	// The released reservation makes any later timer invocation harmless.
	_, _ = client.Run("systemctl stop " + run.unit() + ".timer >/dev/null 2>&1")
	return fmt.Errorf("kip restored the previous firewall")
}
