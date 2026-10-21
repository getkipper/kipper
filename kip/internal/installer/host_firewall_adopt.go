package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/getkipper/kipper/kip/internal/localrun"
)

// UFWAdoption describes the proposed takeover of an active, unclaimed UFW.
type UFWAdoption struct {
	Possible    bool
	Reason      string
	Removed     []string
	Opened      []string
	Pods        []string
	Peers       []string
	Fingerprint string
}

// PreviewUFWAdoption checks takeover eligibility and describes the changes
// without modifying the host. Ineligibility is reported with Possible false
// and an explanation in Reason. Invalid mesh input and read failures return
// an error.
func PreviewUFWAdoption(client CommandRunner, mesh FirewallMesh) (UFWAdoption, error) {
	if err := mesh.validate(); err != nil {
		return UFWAdoption{}, err
	}
	audit, err := AuditFirewall(client)
	if err != nil {
		return UFWAdoption{}, err
	}
	if audit.Existing != "ufw" || !audit.Adoptable {
		return UFWAdoption{Reason: "no active UFW that kip could take over"}, nil
	}
	ports, added, ops, err := readUFWAdoptionInputs(client)
	if err != nil {
		return UFWAdoption{}, err
	}
	refusal, err := ufwTakeoverRefusal(client, added, ports, mesh, ops)
	if err != nil {
		return UFWAdoption{}, err
	}
	if refusal != "" {
		return UFWAdoption{Reason: refusal}, nil
	}
	want := ufwWantedRules(ports, mesh, ops)
	preview := UFWAdoption{
		Possible:    true,
		Peers:       openedToPeers(mesh),
		Fingerprint: ufwFingerprint(added, ports, mesh, ops),
	}
	for _, r := range foreignUFWRules(added, want, ports) {
		preview.Removed = append(preview.Removed, strings.TrimPrefix(r, "ufw "))
	}
	for _, r := range append(ufwRules(ports, true, mesh.Role), ops...) {
		f := strings.Fields(r)
		switch {
		case len(f) >= 3 && (f[1] == "allow" || f[1] == "limit") && f[2] != "from":
			preview.Opened = append(preview.Opened, f[2])
		case len(f) == 10 && f[2] == "from" && f[3] == k3sPodCIDR:
			preview.Pods = append(preview.Pods, f[7]+"/"+f[9])
		}
	}
	return preview, nil
}

// Question formats a successful takeover preview and consent prompt for host.
// Display it only when Possible is true.
func (a UFWAdoption) Question(host string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "UFW is running on %s without a Kipper ownership marker.\n", host)
	b.WriteString("  Kipper will replace its rules with the following:\n")
	if len(a.Removed) > 0 {
		fmt.Fprintf(&b, "     removed: %s\n", strings.Join(a.Removed, ", "))
	}
	fmt.Fprintf(&b, "     opened to everyone: %s\n", strings.Join(a.Opened, ", "))
	fmt.Fprintf(&b, "     opened to pods: %s\n", strings.Join(a.Pods, ", "))
	for _, p := range a.Peers {
		fmt.Fprintf(&b, "     opened to cluster node %s\n", p)
	}
	b.WriteString("  Incoming traffic is denied by default. If new SSH connections fail afterwards, Kipper\n")
	b.WriteString("  attempts to restore the previous firewall. A fallback timer also attempts restoration\n")
	b.WriteString("  after three minutes without progress unless Kipper confirms the changes. Allow takeover?")
	return b.String()
}

// readUFWAdoptionInputs reads the configured SSH ports, saved rules from
// ufw show added and operator tunnel rules used to check and preview takeover.
func readUFWAdoptionInputs(client CommandRunner) (ports []string, added string, ops []string, err error) {
	if ports, err = readSSHPorts(client); err != nil {
		return nil, "", nil, err
	}
	if added, err = runUFWOutput(client, "ufw show added"); err != nil {
		return nil, "", nil, fmt.Errorf("listing ufw's rules: %w", err)
	}
	if ops, err = opsUFWRules(client); err != nil {
		return nil, "", nil, err
	}
	return ports, added, ops, nil
}

// ufwTakeover records the rules checked before taking over an unclaimed UFW.
// approved contains all saved rules with quotes removed, including kip's rules.
// Only foreign rules from that set may be deleted. ops restricts added tunnel
// rules to those checked.
type ufwTakeover struct {
	approved map[string]bool
	ops      []string
}

// checkUFWAdoption allows a takeover only with an SSH check from outside the
// host and, for interactive consent, only of the rules that were previewed. It
// returns the saved rules it checked, which are the only ones the takeover
// may delete.
func checkUFWAdoption(client FirewallRunner, consent FirewallConsent, mesh FirewallMesh, sshPorts []string) (*ufwTakeover, error) {
	if err := client.ConfirmFreshConnection(); errors.Is(err, localrun.ErrNoConnection) {
		return nil, fmt.Errorf("taking over a running UFW needs a check from outside the host, so kip left it alone. Run kip install over SSH or with --via-server")
	} else if err != nil {
		return nil, fmt.Errorf("kip could not open a second connection to the host, so it left the firewall alone: %w", err)
	}
	added, err := runUFWOutput(client, "ufw show added")
	if err != nil {
		return nil, fmt.Errorf("listing ufw's rules: %w", err)
	}
	ops, err := opsUFWRules(client)
	if err != nil {
		return nil, err
	}
	if consent.Fingerprint != "" && ufwFingerprint(added, sshPorts, mesh, ops) != consent.Fingerprint {
		return nil, fmt.Errorf("the running UFW changed after you agreed to its takeover, so kip left it alone. Run the command again to review the changes")
	}
	refusal, err := ufwTakeoverRefusal(client, added, sshPorts, mesh, ops)
	if err != nil {
		return nil, err
	}
	if refusal != "" {
		return nil, fmt.Errorf("kip left the running UFW alone: %s", refusal)
	}
	takeover := &ufwTakeover{approved: map[string]bool{}, ops: ops}
	for _, line := range strings.Split(added, "\n") {
		takeover.approved[ufwRuleKey(strings.TrimSpace(line))] = true
	}
	return takeover, nil
}

// ufwTakeoverRefusal returns why kip cannot take over the saved rules, or an
// empty string if it can.
func ufwTakeoverRefusal(client CommandRunner, added string, sshPorts []string, mesh FirewallMesh, ops []string) (string, error) {
	idle, err := sshPortsNotListening(client, sshPorts)
	if err != nil {
		return "", err
	}
	if len(idle) > 0 {
		return fmt.Sprintf("sshd's configuration names port %s, but nothing listens there. Restart sshd (on hosts where systemd opens the SSH socket, run systemctl daemon-reload and restart ssh.socket) so the running SSH server matches its configuration", strings.Join(idle, ", ")), nil
	}
	for _, rule := range foreignUFWRules(added, ufwWantedRules(sshPorts, mesh, ops), sshPorts) {
		if !ufwRuleDeletable(rule) {
			return fmt.Sprintf("kip cannot remove the rule %s safely. Delete or simplify it first", strings.TrimPrefix(rule, "ufw ")), nil
		}
	}
	return "", nil
}

// sshPortsNotListening returns configured SSH ports without a listening TCP
// socket, in input order. Until sshd is restarted or its systemd socket is
// regenerated, listening ports can differ from the configuration read by sshd -T.
// Takeover preserves SSH access on configured ports, so they must be listening.
func sshPortsNotListening(client CommandRunner, sshPorts []string) ([]string, error) {
	out, err := client.Run("ss -Hltn")
	if err != nil {
		return nil, fmt.Errorf("listing the host's listening TCP ports: %w", err)
	}
	listening := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 4 {
			listening[f[3][strings.LastIndex(f[3], ":")+1:]] = true
		}
	}
	var idle []string
	for _, port := range sshPorts {
		if !listening[port] {
			idle = append(idle, port)
		}
	}
	return idle, nil
}

// deletableUFWRule matches a rule whose words are plain or single-quoted
// without inner quotes, so the shell passes `ufw delete` the words ufw printed.
var deletableUFWRule = regexp.MustCompile(`^ufw( ([A-Za-z0-9./:_+@,-]+|'[A-Za-z0-9./:_+@, -]*'))+$`)

// ufwRuleDeletable reports whether kip can delete rule, as `ufw show added`
// prints it, through a shell command.
func ufwRuleDeletable(rule string) bool {
	return deletableUFWRule.MatchString(rule)
}

// ufwFingerprint binds consent to the saved rules, the configured SSH ports,
// the host's role and peers, and the tunnel's rules.
func ufwFingerprint(added string, ports []string, mesh FirewallMesh, ops []string) string {
	h := sha256.New()
	for _, part := range []string{added, strings.Join(ports, " "), meshFingerprint(mesh), strings.Join(ops, "\n")} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ufwWantedRules lists every rule kip keeps on a host it manages.
func ufwWantedRules(sshPorts []string, mesh FirewallMesh, ops []string) []string {
	return append(append(ufwRules(sshPorts, true, mesh.Role), ufwPeerRules(mesh)...), ops...)
}

// foreignUFWRules returns the saved rules, as `ufw show added` prints them,
// that are neither in want nor an allow or limit rule for one of sshPorts.
// Quoting is ignored because ufw prints comments quoted.
func foreignUFWRules(added string, want, sshPorts []string) []string {
	keep := map[string]bool{}
	for _, r := range want {
		keep[ufwRuleKey(r)] = true
	}
	var foreign []string
	for _, line := range strings.Split(added, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ufw ") || keep[ufwRuleKey(line)] || isSSHRule(line, sshPorts) {
			continue
		}
		foreign = append(foreign, line)
	}
	return foreign
}

// ufwRuleKey removes single quotes so printed rules match unquoted commands.
func ufwRuleKey(rule string) string {
	return strings.ReplaceAll(rule, "'", "")
}

// isSSHRule reports whether rule allows or rate-limits one of sshPorts from
// everywhere, with an optional /tcp suffix and comment. These rules are kept
// so kip can update their SSH action in place.
func isSSHRule(rule string, sshPorts []string) bool {
	f := strings.Fields(rule)
	if len(f) < 3 || (f[1] != "allow" && f[1] != "limit") || (len(f) > 3 && f[3] != "comment") {
		return false
	}
	return slices.Contains(sshPorts, strings.TrimSuffix(f[2], "/tcp"))
}

// ufwDeleteCommand turns a rule printed by `ufw show added` into the command
// that deletes it.
func ufwDeleteCommand(rule string) string {
	if rest, ok := strings.CutPrefix(rule, "ufw route "); ok {
		return "ufw route delete " + rest
	}
	return "ufw delete " + strings.TrimPrefix(rule, "ufw ")
}
