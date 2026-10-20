package installer

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// removeKipperFirewall removes kip's rules from UFW or firewalld when its
// ownership file is nonempty. The firewall stays running or stopped, and the
// ownership file lets a later install manage it again. Rules for sshd's TCP
// ports stay. Operator rules stay unless they match kip's rules or belong to
// a firewalld zone or policy kip deletes.
// acted reports whether a nonempty ownership file was found, even on failure.
// Removal is skipped if sshd's ports cannot be read or a firewall change is pending.
func removeKipperFirewall(client CommandRunner) (acted bool, err error) {
	ufw, firewalld := firewallClaimed(client), firewalldClaimed(client)
	if !ufw && !firewalld {
		return false, nil
	}
	sshPorts, err := readSSHPorts(client)
	if err != nil {
		return true, err
	}
	out, err := client.Run(firewallChangePendingCommand(""))
	if err != nil {
		return true, fmt.Errorf("checking for a pending firewall change: %w", err)
	}
	if strings.TrimSpace(out) == "pending" {
		return true, fmt.Errorf("another firewall change is pending or being rolled back, so kip left the firewall alone")
	}
	var errs []error
	if ufw {
		errs = append(errs, removeKipperUFWRules(client, sshPorts))
	}
	if firewalld {
		errs = append(errs, removeKipperFirewalldRules(client, sshPorts))
	}
	return true, errors.Join(errs...)
}

// firewallChangePendingCommand builds a command that prints "pending" while
// a UFW or firewalld change holds its reservation or a rollback unit is active
// or activating. It holds both firewall locks during the check to exclude
// concurrent locked steps. Tests use root to redirect host paths.
func firewallChangePendingCommand(root string) string {
	dir := root + firewalldStateDir
	return `exec 9>>` + dir + `/ufw.lock 8>>` + root + firewalldLock + ` && flock -w 120 9 && flock -w 120 8 || exit 1; ` +
		`units=$(systemctl list-units --state=active,activating --plain --no-legend '` + ufwRollbackUnit + `*' '` + firewalldRollbackUnit + `*') || exit 1; ` +
		`if [ -e ` + dir + `/ufw-run ] || [ -e ` + dir + `/firewalld-run ] || [ -n "$units" ]; then echo pending; fi`
}

// removeKipperUFWRules deletes kip's saved UFW rules and saves the routed
// policy as DROP. It stops at the first command error.
func removeKipperUFWRules(client CommandRunner, sshPorts []string) error {
	added, err := runUFWOutput(client, "ufw show added")
	if err != nil {
		return fmt.Errorf("listing ufw's rules: %w", err)
	}
	for _, rule := range uninstallUFWDeletes(added, sshPorts) {
		if err := runUFW(client, rule); err != nil {
			return fmt.Errorf("applying %q: %w", rule, err)
		}
	}
	if _, err := client.Run(ufwForwardDropCommand("")); err != nil {
		return fmt.Errorf("saving ufw's routed policy as deny: %w", err)
	}
	return nil
}

// uninstallUFWDeletes returns delete commands for the rules kip adds that
// `ufw show added` lists: public server ports, pod access to host ports and
// peer rules. Rules that open a TCP port in sshPorts stay. An operator rule
// written exactly like one of kip's is deleted with it.
func uninstallUFWDeletes(added string, sshPorts []string) []string {
	present := map[string]bool{}
	for _, line := range strings.Split(added, "\n") {
		present[strings.TrimSpace(line)] = true
	}
	var deletes []string
	for _, rule := range append(slices.Clone(ufwServerOnlyRules), ufwPodRules()...) {
		if present[rule] {
			deletes = append(deletes, "ufw delete "+strings.TrimPrefix(rule, "ufw "))
		}
	}
	deletes = append(deletes, staleUFWPeerRules(added, FirewallMesh{})...)
	return slices.DeleteFunc(deletes, func(rule string) bool {
		for _, p := range sshPorts {
			if strings.HasSuffix(rule, " "+p+"/tcp") || strings.Contains(rule, " port "+p+" proto tcp") {
				return true
			}
		}
		return false
	})
}

// ufwForwardDropCommand saves UFW's routed policy as DROP. UFW applies it the
// next time it starts. Applying it immediately would require restarting UFW,
// which could interrupt the SSH session.
func ufwForwardDropCommand(root string) string {
	return fmt.Sprintf(`sed -i 's|^DEFAULT_FORWARD_POLICY=.*|DEFAULT_FORWARD_POLICY="DROP"|' %s/etc/default/ufw`, root)
}

// removeKipperFirewalldRules removes kip's firewalld configuration. A running
// firewalld whose runtime rules differ from its saved ones is left alone,
// because the reload that applies the removal would drop the runtime rules.
func removeKipperFirewalldRules(client CommandRunner, sshPorts []string) error {
	state, err := client.Run("firewall-cmd --state 2>/dev/null || true")
	if err != nil {
		return fmt.Errorf("checking whether firewalld runs: %w", err)
	}
	if strings.TrimSpace(state) == "running" {
		running, err := client.Run(firewalldDumpCommand(false))
		if err != nil {
			return fmt.Errorf("reading the running firewalld configuration: %w", err)
		}
		saved, err := client.Run(firewalldDumpCommand(true))
		if err != nil {
			return fmt.Errorf("reading the saved firewalld configuration: %w", err)
		}
		if drift := firewalldDrift(running, saved); len(drift) > 0 {
			return fmt.Errorf("firewalld's running configuration differs from its saved one (%s), so kip left it alone. "+
				"Remove kip's zones %s, %s and %s, its policies and the public ports %s by hand",
				strings.Join(drift, ", "), firewalldPodZone, firewalldPeerZone, firewalldServerZone, strings.Join(firewalldServerPorts(sshPorts), ", "))
		}
	}
	if _, err := client.Run(firewalldUninstallCommand("", sshPorts)); err != nil {
		return fmt.Errorf("removing kip's firewalld zones, policies and ports: %w", err)
	}
	return nil
}

// firewalldServerPorts lists the public server ports kip opens on firewalld
// that sshd does not listen on.
func firewalldServerPorts(sshPorts []string) []string {
	var ports []string
	for _, p := range []string{"80", "443", "6443"} {
		if !slices.Contains(sshPorts, p) {
			ports = append(ports, p+"/tcp")
		}
	}
	return ports
}

// firewalldUninstallCommand deletes kip's forwarding policies and its pod,
// peer and server zones, including any operator additions. It closes public
// server ports except those used by sshd and removes firewalld's .old copies
// of kip's files. A running firewalld is changed permanently and reloaded.
// A stopped firewalld is changed offline and stays stopped.
func firewalldUninstallCommand(root string, sshPorts []string) string {
	var policies, ports, old []string
	for _, p := range firewalldPolicies {
		policies = append(policies, p.name)
		old = append(old, root+firewalldConfigDir+"/policies/"+p.name+".xml.old")
	}
	zones := []string{firewalldPodZone, firewalldPeerZone, firewalldServerZone}
	for _, z := range zones {
		old = append(old, root+firewalldConfigDir+"/zones/"+z+".xml.old")
	}
	ports = firewalldServerPorts(sshPorts)
	// A closed port gives --query-port exit code 1, so only other failures abort removal.
	return `set -e; ` +
		`if firewall-cmd --state >/dev/null 2>&1; then running=1; fc() { firewall-cmd --permanent "$@"; }; else running=; fc() { firewall-offline-cmd "$@"; }; fi; ` +
		`has() { for x in $1; do [ "$x" = "$2" ] && return 0; done; return 1; }; ` +
		`have=$(fc --get-policies); for p in ` + strings.Join(policies, " ") + `; do if has "$have" "$p"; then fc --delete-policy="$p" >/dev/null; fi; done; ` +
		`have=$(fc --get-zones); for z in ` + strings.Join(zones, " ") + `; do if has "$have" "$z"; then fc --delete-zone="$z" >/dev/null; fi; done; ` +
		`for p in ` + strings.Join(ports, " ") + `; do rc=0; fc --zone=public --query-port="$p" >/dev/null || rc=$?; ` +
		`case $rc in 0) fc --zone=public --remove-port="$p" >/dev/null ;; 1) ;; *) echo "firewalld could not say whether port $p is open" >&2; exit 1 ;; esac; done; ` +
		`rm -f ` + strings.Join(old, " ") + `; ` +
		`if [ -n "$running" ]; then firewall-cmd --reload >/dev/null; fi`
}
