package installer

import (
	"fmt"
	"strings"
)

// operatorFirewalldCommand queries runtime rules for missing cluster access
// and configurations this check cannot evaluate.
func operatorFirewalldCommand(sshPort string) string {
	need := func(port, service string) string {
		q := `firewall-cmd --zone="$ext" --query-port=` + port + " >/dev/null 2>&1"
		if service != "" {
			// Validate the service definition because operators can customize it.
			q += ` || { firewall-cmd --zone="$ext" --query-service=` + service + " >/dev/null 2>&1 && " +
				`firewall-cmd --info-service=` + service + ` 2>/dev/null | awk -v p=` + port + ` '$1 == "ports:" {for (i = 2; i <= NF; i++) if ($i == p) ok = 1} $1 ~ /^destination/ && NF > 1 {bad = 1} END {exit !(ok && !bad)}'; }`
		}
		return q + " || echo MISSING:" + port + "; "
	}
	sshService := ""
	if sshPort == "22" {
		sshService = "ssh"
	}
	podPorts := ""
	for _, p := range []string{"6443/tcp", kubeletPort + "/tcp", nodeExporterPort + "/tcp"} {
		podPorts += `firewall-cmd --zone="$z" --query-port=` + p + ` >/dev/null 2>&1 || echo MISSING-POD:` + p + "; "
	}
	// Use the default-route interface to identify the external zone, falling
	// back to the default zone if its binding is unavailable.
	return `dev=$(ip route show default 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "dev") {print $(i+1); exit}}'); ` +
		`ext=$( { [ -n "$dev" ] && firewall-cmd --get-zone-of-interface="$dev"; } 2>/dev/null || firewall-cmd --get-default-zone); ` +
		`echo "ZONE:$ext"; ` +
		`[ "$(firewall-cmd --query-panic 2>&1)" = no ] || echo PANIC; ` +
		// Reject overrides that can invalidate port checks. The allow-host-ipv6
		// policy is validated separately by CheckOperatorFirewalld.
		`[ -z "$(firewall-cmd --zone="$ext" --list-rich-rules 2>&1)" ] || echo "UNSUPPORTED:rich rules in zone $ext"; ` +
		`[ -z "$(firewall-cmd --zone="$ext" --list-forward-ports 2>&1)" ] || echo "UNSUPPORTED:forward ports in zone $ext"; ` +
		`for p in $(firewall-cmd --get-policies 2>/dev/null); do if [ "$p" = allow-host-ipv6 ]; then echo STOCK-POLICY; else echo "UNSUPPORTED:policy $p"; fi; done; ` +
		`[ -z "$(firewall-cmd --direct --get-all-chains 2>&1; firewall-cmd --direct --get-all-rules 2>&1; firewall-cmd --direct --get-all-passthroughs 2>&1)" ] || echo "UNSUPPORTED:direct configuration"; ` +
		need(sshPort+"/tcp", sshService) +
		need("80/tcp", "http") +
		need("443/tcp", "https") +
		need("6443/tcp", "kube-apiserver") +
		`z=$(firewall-cmd --get-zone-of-source=` + k3sPodCIDR + ` 2>/dev/null); ` +
		`if [ -z "$z" ]; then echo MISSING-POD-ZONE; else ` +
		// Any other zone holding sources, or a zone priority, can classify
		// traffic ahead of the zones checked here.
		`firewall-cmd --get-active-zones 2>/dev/null | awk -v pod="$z" '/^[^ ]/ {zone = $1} /^  sources:/ && zone != pod {print "UNSUPPORTED:sources bound to zone " zone}'; ` +
		`for q in "$ext" "$z"; do firewall-cmd --info-zone="$q" 2>/dev/null | awk -v q="$q" '$1 ~ /-priority:$/ && $2 != "0" {print "UNSUPPORTED:a zone priority in zone " q}'; done; ` +
		`[ -z "$(firewall-cmd --zone="$z" --list-rich-rules 2>&1)" ] || echo "UNSUPPORTED:rich rules in zone $z"; ` +
		`[ -z "$(firewall-cmd --zone="$z" --list-forward-ports 2>&1)" ] || echo "UNSUPPORTED:forward ports in zone $z"; ` +
		`target=$(firewall-cmd --info-zone="$z" 2>/dev/null | sed -n 's/^  target: //p'); ` +
		`if [ -z "$target" ]; then echo POD-TARGET-UNKNOWN; elif [ "$target" != ACCEPT ]; then ` + podPorts + `fi; fi`
}

// CheckOperatorFirewalld lists missing public and pod-network access and
// unsupported runtime rules in an operator-managed firewall.
func CheckOperatorFirewalld(client CommandRunner) ([]string, error) {
	sshPort, err := readSSHPort(client)
	if err != nil {
		return nil, err
	}
	out, err := client.Run(operatorFirewalldCommand(sshPort))
	if err != nil {
		return nil, fmt.Errorf("checking the running firewalld: %w", err)
	}
	var missing []string
	if strings.Contains(out, "STOCK-POLICY") {
		info, err := client.Run("firewall-cmd --info-policy=allow-host-ipv6")
		if err != nil {
			return nil, fmt.Errorf("reading the allow-host-ipv6 policy: %w", err)
		}
		if problems := allowHostIPv6Problems(parseFirewalldInfo(strings.Split(info, "\n"))); len(problems) > 0 {
			missing = append(missing, "the stock allow-host-ipv6 policy ("+strings.Join(problems, "; ")+")")
		}
	}
	zone := "public"
	for _, line := range strings.Split(out, "\n") {
		if z, ok := strings.CutPrefix(strings.TrimSpace(line), "ZONE:"); ok && z != "" {
			zone = z
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "PANIC":
			missing = append(missing, "panic mode off")
		case strings.HasPrefix(line, "UNSUPPORTED:"):
			missing = append(missing, "no "+strings.TrimPrefix(line, "UNSUPPORTED:")+", which kip cannot evaluate")
		case line == "POD-TARGET-UNKNOWN":
			missing = append(missing, "a readable target for the pod network's zone")
		case line == "MISSING-POD-ZONE":
			missing = append(missing, fmt.Sprintf("a zone with source %s (the pod network)", k3sPodCIDR))
		case strings.HasPrefix(line, "MISSING-POD:"):
			missing = append(missing, strings.TrimPrefix(line, "MISSING-POD:")+" open to the pod network")
		case strings.HasPrefix(line, "MISSING:"):
			missing = append(missing, strings.TrimPrefix(line, "MISSING:")+" open in zone "+zone)
		}
	}
	return missing, nil
}
