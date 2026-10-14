package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// FirewalldAdoption describes the proposed changes to an operator-managed firewalld.
type FirewalldAdoption struct {
	Possible    bool
	Reason      string
	Removed     []string
	Opened      []string
	Fingerprint string
}

// PreviewFirewalldAdoption checks takeover eligibility and describes changes
// without modifying the host. Ineligibility is reported in Reason; read failures
// return an error.
func PreviewFirewalldAdoption(client CommandRunner) (FirewalldAdoption, error) {
	audit, err := AuditFirewall(client)
	if err != nil {
		return FirewalldAdoption{}, err
	}
	if !audit.Adoptable {
		return FirewalldAdoption{Reason: "no running firewalld that kip could take over"}, nil
	}
	ports, err := readSSHPorts(client)
	if err != nil {
		return FirewalldAdoption{}, err
	}
	want := wantFirewalld(ports)

	saved, err := readFirewalldSaved(client)
	if err != nil {
		return FirewalldAdoption{}, err
	}
	if changes := saved.changes(false, true, want); len(changes) > 0 {
		return FirewalldAdoption{Reason: "its saved configuration has changes kip cannot take over (" + strings.Join(changes, ", ") + ")"}, nil
	}
	running, err := client.Run(firewalldDumpCommand(false))
	if err != nil {
		return FirewalldAdoption{}, fmt.Errorf("reading the running firewalld configuration: %w", err)
	}
	savedDump, err := client.Run(firewalldDumpCommand(true))
	if err != nil {
		return FirewalldAdoption{}, fmt.Errorf("reading the saved firewalld configuration: %w", err)
	}
	if drift := firewalldDrift(running, savedDump); len(drift) > 0 {
		return FirewalldAdoption{Reason: "its running configuration differs from its saved one (" + strings.Join(drift, ", ") + "); save or drop the running rules first"}, nil
	}

	public, err := client.Run("firewall-cmd --info-zone=public")
	if err != nil {
		return FirewalldAdoption{}, fmt.Errorf("reading the running public zone: %w", err)
	}
	zone := parseFirewalldInfo(strings.Split(public, "\n"))
	return FirewalldAdoption{
		Possible:    true,
		Removed:     removedFromPublicZone(zone, want),
		Opened:      openedInPublicZone(zone, want),
		Fingerprint: firewalldFingerprint(public, saved, ports),
	}, nil
}

func currentFirewalldFingerprint(client CommandRunner, ports []string) (string, error) {
	saved, err := readFirewalldSaved(client)
	if err != nil {
		return "", err
	}
	public, err := client.Run("firewall-cmd --info-zone=public")
	if err != nil {
		return "", fmt.Errorf("reading the running public zone: %w", err)
	}
	return firewalldFingerprint(public, saved, ports), nil
}

// firewalldFingerprint binds consent to the running public zone, saved file
// inventory, firewalld.conf, saved public zone, and configured SSH ports.
func firewalldFingerprint(public string, saved firewalldSavedConfig, ports []string) string {
	h := sha256.New()
	for _, part := range []string{public, saved.inventory, saved.conf, saved.public, strings.Join(ports, " ")} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func removedFromPublicZone(zone firewalldInfo, want firewalldWant) []string {
	var removed []string
	for _, s := range zone["services"] {
		if !containsString(want.publicServices, s) {
			removed = append(removed, "service "+s)
		}
	}
	for _, p := range zone["ports"] {
		if !containsString(want.publicPorts, p) {
			removed = append(removed, "port "+p)
		}
	}
	for _, field := range []string{"protocols", "source-ports", "forward-ports", "rich rules"} {
		for _, v := range zone[field] {
			removed = append(removed, strings.TrimSuffix(field, "s")+" "+v)
		}
	}
	for _, v := range zone["icmp-blocks"] {
		removed = append(removed, "ICMP block "+v)
	}
	for _, v := range zone["sources"] {
		removed = append(removed, "source "+v)
	}
	if t := strings.Join(zone["target"], " "); t != "" && t != "default" {
		removed = append(removed, "target "+t)
	}
	if strings.Join(zone["masquerade"], " ") == "yes" {
		removed = append(removed, "masquerading")
	}
	if strings.Join(zone["icmp-block-inversion"], " ") == "yes" {
		removed = append(removed, "ICMP block inversion")
	}
	if strings.Join(zone["forward"], " ") == "no" {
		removed = append(removed, "forwarding off (Kipper turns it on)")
	}
	for _, f := range []string{"ingress-priority", "egress-priority"} {
		if p := strings.Join(zone[f], " "); p != "" && p != "0" {
			removed = append(removed, f+" "+p)
		}
	}
	return removed
}

// openedInPublicZone includes all resulting public ports, even those already
// open, plus newly added services.
func openedInPublicZone(zone firewalldInfo, want firewalldWant) []string {
	opened := append([]string{}, want.publicPorts...)
	for _, s := range want.publicServices {
		if !containsString(zone["services"], s) {
			opened = append(opened, "service "+s)
		}
	}
	return opened
}

// Question formats the takeover preview and consent prompt.
func (a FirewalldAdoption) Question(host string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "firewalld is running on %s with an operator-managed configuration. Kipper will replace its public zone:\n", host)
	if len(a.Removed) > 0 {
		fmt.Fprintf(&b, "     removed: %s\n", strings.Join(a.Removed, ", "))
	}
	fmt.Fprintf(&b, "     opened to everyone: %s\n", strings.Join(a.Opened, ", "))
	b.WriteString("  It also adds a zone for pod traffic and two forwarding policies. If new SSH connections fail\n")
	b.WriteString("  afterwards, Kipper restores the previous firewall, with an automatic rollback scheduled after three minutes as a fallback. Allow takeover?")
	return b.String()
}
