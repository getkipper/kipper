package installer

import (
	"fmt"
	"strconv"
	"strings"
)

// zoneOpensPort builds a shell test for direct or service-based port access.
// Service definitions are checked because operators can customize them.
func zoneOpensPort(zone, port, service string) string {
	q := `firewall-cmd --zone="` + zone + `" --query-port=` + port + " >/dev/null 2>&1"
	if service != "" {
		q += ` || { firewall-cmd --zone="` + zone + `" --query-service=` + service + " >/dev/null 2>&1 && " +
			`firewall-cmd --info-service=` + service + ` 2>/dev/null | awk -v p=` + port + ` '$1 == "ports:" {for (i = 2; i <= NF; i++) if ($i == p) ok = 1} $1 ~ /^destination/ && NF > 1 {bad = 1} END {exit !(ok && !bad)}'; }`
	}
	return q
}

// operatorFirewalldCommand builds a shell command that reports missing cluster
// access and runtime rules the check cannot evaluate.
func operatorFirewalldCommand(sshPorts []string, mesh FirewallMesh) string {
	need := func(port, service string) string {
		return zoneOpensPort("$ext", port, service) + " || echo MISSING:" + port + "; "
	}
	ssh := ""
	for _, p := range sshPorts {
		service := ""
		if p == "22" {
			service = "ssh"
		}
		ssh += need(p+"/tcp", service)
	}
	public := ""
	if mesh.Role != RoleWorker {
		public = need("80/tcp", "http") + need("443/tcp", "https") + need("6443/tcp", "kube-apiserver")
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
		// Reject overrides that can invalidate port checks. CheckOperatorFirewalld
		// validates the stock IPv6 policy and retained Kipper policies separately.
		`[ -z "$(firewall-cmd --zone="$ext" --list-rich-rules 2>&1)" ] || echo "UNSUPPORTED:rich rules in zone $ext"; ` +
		`[ -z "$(firewall-cmd --zone="$ext" --list-forward-ports 2>&1)" ] || echo "UNSUPPORTED:forward ports in zone $ext"; ` +
		`if policies=$(firewall-cmd --get-policies 2>/dev/null); then for p in $policies; do case "$p" in allow-host-ipv6) echo STOCK-POLICY ;; kipper-pods-in|kipper-pods-out) echo "KIPPER-POLICY:$p" ;; ` +
		`*) info=$(firewall-cmd --info-policy="$p" 2>/dev/null) && [ "$(printf '%s\n' "$info" | sed -n 's/^  disable: //p')" = yes ] || echo "UNSUPPORTED:policy $p" ;; esac; done; else echo UNREADABLE-POLICIES; fi; ` +
		`[ -z "$(firewall-cmd --direct --get-all-chains 2>&1; firewall-cmd --direct --get-all-rules 2>&1; firewall-cmd --direct --get-all-passthroughs 2>&1)" ] || echo "UNSUPPORTED:direct configuration"; ` +
		ssh +
		public +
		`peerzones=; ` +
		operatorPeerCommand(mesh) +
		`z=$(firewall-cmd --get-zone-of-source=` + k3sPodCIDR + ` 2>/dev/null); ` +
		`if [ -z "$z" ]; then echo MISSING-POD-ZONE; else ` +
		// Extra source bindings or zone priorities could send traffic to a different
		// zone than the one checked. Peer zones allow only exact peer addresses;
		// with peers present, the pod zone allows only the pod and service networks.
		`firewall-cmd --get-active-zones 2>/dev/null | awk -v pod="$z" -v pz=" $peerzones " -v peers=" ` + peerSourceList(mesh) + ` " -v podnets=" ` + k3sPodCIDR + ` ` + k3sServiceCIDR + ` " -v meshed=` + strconv.FormatBool(len(mesh.Peers) > 0) + ` ` +
		`'/^[^ ]/ {zone = $1} /^  sources:/ && zone == pod { if (meshed == "true") for (i = 2; i <= NF; i++) if (index(podnets, " " $i " ") == 0) print "UNSUPPORTED:source " $i " bound to zone " zone; next } ` +
		`/^  sources:/ { if (index(pz, " " zone " ") == 0) {print "UNSUPPORTED:sources bound to zone " zone; next} ` +
		`for (i = 2; i <= NF; i++) if (index(peers, " " $i " ") == 0) print "UNSUPPORTED:source " $i " bound to zone " zone}'; ` +
		`for q in "$ext" "$z" $peerzones; do firewall-cmd --info-zone="$q" 2>/dev/null | awk -v q="$q" '$1 ~ /-priority:$/ && $2 != "0" {print "UNSUPPORTED:a zone priority in zone " q}'; done; ` +
		`for q in $peerzones; do [ -z "$(firewall-cmd --zone="$q" --list-rich-rules 2>&1)" ] || echo "UNSUPPORTED:rich rules in zone $q"; ` +
		`[ -z "$(firewall-cmd --zone="$q" --list-forward-ports 2>&1)" ] || echo "UNSUPPORTED:forward ports in zone $q"; done; ` +
		`[ -z "$(firewall-cmd --zone="$z" --list-rich-rules 2>&1)" ] || echo "UNSUPPORTED:rich rules in zone $z"; ` +
		`[ -z "$(firewall-cmd --zone="$z" --list-forward-ports 2>&1)" ] || echo "UNSUPPORTED:forward ports in zone $z"; ` +
		`target=$(firewall-cmd --info-zone="$z" 2>/dev/null | sed -n 's/^  target: //p'); ` +
		`if [ -z "$target" ]; then echo POD-TARGET-UNKNOWN; elif [ "$target" != ACCEPT ]; then ` + podPorts + `fi; fi`
}

// CheckOperatorFirewalld lists missing public, pod and peer access and
// unsupported runtime rules in an operator-managed firewall.
func CheckOperatorFirewalld(client CommandRunner, mesh FirewallMesh) ([]string, error) {
	if err := mesh.validate(); err != nil {
		return nil, err
	}
	sshPorts, err := readSSHPorts(client)
	if err != nil {
		return nil, err
	}
	out, err := client.Run(operatorFirewalldCommand(sshPorts, mesh))
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
	for _, line := range strings.Split(out, "\n") {
		name, ok := strings.CutPrefix(strings.TrimSpace(line), "KIPPER-POLICY:")
		if !ok {
			continue
		}
		info, err := client.Run("firewall-cmd --info-policy=" + shellQuote(name))
		if err != nil {
			return nil, fmt.Errorf("reading the %s policy: %w", name, err)
		}
		if problems := kipperPolicyProblems(name, parseFirewalldInfo(strings.Split(info, "\n"))); len(problems) > 0 {
			missing = append(missing, "the "+name+" policy as Kipper wrote it ("+strings.Join(problems, "; ")+")")
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
		case line == "UNREADABLE-POLICIES":
			missing = append(missing, "a readable list of firewalld policies")
		case line == "POD-TARGET-UNKNOWN":
			missing = append(missing, "a readable target for the pod network's zone")
		case line == "MISSING-POD-ZONE":
			missing = append(missing, fmt.Sprintf("a zone with source %s (the pod network)", k3sPodCIDR))
		case strings.HasPrefix(line, "MISSING-PEER:"):
			peer, port, _ := strings.Cut(strings.TrimPrefix(line, "MISSING-PEER:"), ":")
			missing = append(missing, port+" open to cluster node "+peer)
		case strings.HasPrefix(line, "MISSING-POD:"):
			missing = append(missing, strings.TrimPrefix(line, "MISSING-POD:")+" open to the pod network")
		case strings.HasPrefix(line, "MISSING:"):
			missing = append(missing, strings.TrimPrefix(line, "MISSING:")+" open in zone "+zone)
		}
	}
	return missing, nil
}
