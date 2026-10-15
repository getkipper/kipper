package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/localrun"
)

// operatorFirewalld runs the operator check against a fake firewall-cmd. The
// default route leaves through eth0, bound to extZone; open lists what that
// zone allows, and the pod network's zone has the given target and ports.
func operatorFirewalld(t *testing.T, extZone, open, podZone, podTarget, podPorts string) []string {
	t.Helper()
	return operatorFirewalldWith(t, extZone, open, podZone, podTarget, podPorts, "", "")
}

var (
	httpsPorts  = "443/tcp"
	directEntry = ""
	policyRule  = ""
	forwardZone = "none"
	// extraSources adds lines to the active zone listing, quoted for printf.
	extraSources    = ""
	panicAnswer     = "no"
	kipperOutTarget = "ACCEPT"
	gatewayDisable  = "yes"
	policiesFail    = "no"
)

// operatorFirewalldWith can add a dropping rich rule in richZone and an extra policy.
func operatorFirewalldWith(t *testing.T, extZone, open, podZone, podTarget, podPorts, richZone, extraPolicy string) []string {
	t.Helper()
	bin := t.TempDir()
	fakes := map[string]string{
		"sshd": "echo 'port 22'\n",
		"ip":   "echo 'default via 192.0.2.1 dev eth0 proto static'\n",
		"firewall-cmd": `case "$*" in
  *--permanent*) echo "the check must read the running configuration" >&2; exit 1 ;;
  *--get-zone-of-interface=eth0*) [ -n "` + extZone + `" ] && echo ` + extZone + ` && exit 0; exit 2 ;;
  *--get-default-zone*) echo public ;;
  *--get-zone-of-source=*) [ -n "` + podZone + `" ] && echo ` + podZone + ` && exit 0; echo no zone >&2; exit 2 ;;
  *--list-rich-rules*) case "$*" in *"--zone=` + richZone + ` "*) echo 'rule priority="-100" port port="6443" protocol="tcp" drop' ;; esac ;;
  *--get-policies*) [ ` + policiesFail + ` = yes ] && exit 2; echo "allow-host-ipv6 ` + extraPolicy + `" ;;
  *--info-policy=broken*) echo "  disable: yes"; exit 2 ;;
  *--list-forward-ports*) case "$*" in *"--zone=` + forwardZone + ` "*) echo "port=443:proto=tcp:toport=8443" ;; esac ;;
  *--get-all-passthroughs*) echo "` + directEntry + `" ;;
  *--direct*) ;;
  *--info-policy=kipper-pods-out*) printf '%s\n' "kipper-pods-out (active)" "  priority: -1" "  target: ` + kipperOutTarget + `" "  ingress-zones: kipper-pods" "  egress-zones: public" "  services: " "  ports: " "  protocols: " "  masquerade: yes" "  forward-ports: " "  source-ports: " "  rich rules: " ;;
  *--info-policy=gateway-lan-to-world*) printf '%s\n' "gateway-lan-to-world (disabled)" "  disable: ` + gatewayDisable + `" "  target: ACCEPT" ;;
  *--info-policy=allow-host-ipv6*) printf '%s\n' "allow-host-ipv6 (active)" "  target: CONTINUE" "  ingress-zones: ANY" "  egress-zones: HOST" "  services: " "  ports: " "  protocols: " "  masquerade: no" "  forward-ports: " "  source-ports: " "  rich rules: " "	rule family=\"ipv6\" icmp-type name=\"neighbour-solicitation\" accept" "` + policyRule + `" ;;
  *--info-service=*) svc=${*#*--info-service=}; case "$svc" in
      ssh) echo "  ports: 22/tcp" ;; http) echo "  ports: 80/tcp" ;;
      https) echo "  ports: ` + httpsPorts + `" ;; kube-apiserver) echo "  ports: 6443/tcp" ;; esac ;;
  *--query-panic*) echo ` + panicAnswer + ` ;;
  *--get-active-zones*) printf '%s\n' "` + extZone + `" "  interfaces: eth0" "` + podZone + `" "  sources: 10.42.0.0/16" ` + extraSources + ` ;;
  *--info-zone=*) [ -n "` + podTarget + `" ] && echo "  target: ` + podTarget + `" ;;
  *"--zone=` + podZone + ` --query-port="*) for p in ` + podPorts + `; do [ "$*" != "${*%=$p}" ] && exit 0; done; exit 1 ;;
  *"--zone=` + extZone + ` --query-"*) for p in ` + open + `; do [ "$*" != "${*%=$p}" ] && exit 0; done; exit 1 ;;
  *) exit 1 ;;
esac
`,
	}
	for name, body := range fakes {
		//nolint:gosec // the fake commands have to be executable
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755))
	}
	runner := &localrun.Runner{Home: t.TempDir(), Dir: t.TempDir(), Path: bin + ":/usr/bin:/bin"}

	missing, err := CheckOperatorFirewalld(runner, FirewallMesh{})

	require.NoError(t, err)
	return missing
}

func TestCheckOperatorFirewalldAcceptsTheK3sRecipe(t *testing.T) {
	missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "")
	assert.Empty(t, missing)
}

func TestCheckOperatorFirewalldAcceptsNarrowPodRules(t *testing.T) {
	missing := operatorFirewalld(t, "public", "22/tcp 80/tcp 443/tcp kube-apiserver", "pods", "default", "6443/tcp 10250/tcp 9100/tcp")
	assert.Empty(t, missing)
}

func TestCheckOperatorFirewalldListsEverythingMissing(t *testing.T) {
	missing := operatorFirewalld(t, "public", "ssh", "", "", "")

	all := strings.Join(missing, "\n")
	assert.Contains(t, all, "80/tcp open in zone public")
	assert.Contains(t, all, "443/tcp open in zone public")
	assert.Contains(t, all, "6443/tcp open in zone public")
	assert.Contains(t, all, "a zone with source 10.42.0.0/16")
	assert.NotContains(t, all, "22/tcp")
}

func TestCheckOperatorFirewalldNamesPortsClosedToPods(t *testing.T) {
	missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "pods", "default", "6443/tcp")

	assert.Equal(t, []string{"10250/tcp open to the pod network", "9100/tcp open to the pod network"}, missing)
}

func TestCheckOperatorFirewalldSaysWhenItCannotReadThePodZone(t *testing.T) {
	missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "pods", "", "")

	assert.Equal(t, []string{"a readable target for the pod network's zone"}, missing)
}

func TestCheckOperatorFirewalldReadsTheZoneOfTheExternalInterface(t *testing.T) {
	missing := operatorFirewalld(t, "external", "ssh", "trusted", "ACCEPT", "")

	all := strings.Join(missing, "\n")
	assert.Contains(t, all, "80/tcp open in zone external")
	assert.NotContains(t, all, "22/tcp")
}

// A rich rule can block traffic even when a port entry allows it.
func TestCheckOperatorFirewalldReportsOverridesItCannotEvaluate(t *testing.T) {
	missing := operatorFirewalldWith(t, "public", "ssh http https 6443/tcp", "pods", "default", "6443/tcp 10250/tcp 9100/tcp", "pods", "")
	assert.Equal(t, []string{"no rich rules in zone pods, which kip cannot evaluate"}, missing)

	missing = operatorFirewalldWith(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "", "public", "block-api")
	assert.Equal(t, []string{"no rich rules in zone public, which kip cannot evaluate", "no policy block-api, which kip cannot evaluate"}, missing)
}

func TestCheckOperatorFirewalldReadsTheServiceDefinition(t *testing.T) {
	httpsPorts = "8443/tcp"
	defer func() { httpsPorts = "443/tcp" }()

	missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "")

	assert.Equal(t, []string{"443/tcp open in zone public"}, missing)
}

func TestCheckOperatorFirewalldReadsTheStockPolicy(t *testing.T) {
	policyRule = "	rule family=\\\"ipv4\\\" port port=\\\"6443\\\" protocol=\\\"tcp\\\" drop"
	defer func() { policyRule = "" }()

	missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "")

	require.Len(t, missing, 1)
	assert.Contains(t, missing[0], "the stock allow-host-ipv6 policy")
	assert.Contains(t, missing[0], "6443")
}

func TestCheckOperatorFirewalldReportsDirectPassthroughs(t *testing.T) {
	directEntry = "ipv4 -t filter -I INPUT 1 -p tcp --dport 6443 -j DROP"
	defer func() { directEntry = "" }()

	missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "")

	assert.Equal(t, []string{"no direct configuration, which kip cannot evaluate"}, missing)
}

// Port forwarding can redirect traffic away from the expected service.
func TestCheckOperatorFirewalldReportsForwardPorts(t *testing.T) {
	forwardZone = "public"
	defer func() { forwardZone = "none" }()

	missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "")

	assert.Equal(t, []string{"no forward ports in zone public, which kip cannot evaluate"}, missing)
}

// Overlapping source bindings can route pod traffic through another zone.
func TestCheckOperatorFirewalldReportsOtherSourceZones(t *testing.T) {
	extraSources = `"drop" "  sources: 10.42.0.0/24"`
	defer func() { extraSources = "" }()

	missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "")

	assert.Equal(t, []string{"no sources bound to zone drop, which kip cannot evaluate"}, missing)
}

// An unreadable panic-mode result must also fail the check.
func TestCheckOperatorFirewalldReportsPanicMode(t *testing.T) {
	for _, answer := range []string{"yes", "'Error: INVALID_COMMAND'"} {
		panicAnswer = answer
		missing := operatorFirewalld(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "")
		assert.Equal(t, []string{"panic mode off"}, missing, answer)
	}
	panicAnswer = "no"
}

func TestCheckOperatorFirewalldAcceptsKipsLeftoverPolicies(t *testing.T) {
	missing := operatorFirewalldWith(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "", "", "kipper-pods-out")
	assert.Empty(t, missing)

	kipperOutTarget = "DROP"
	defer func() { kipperOutTarget = "ACCEPT" }()
	missing = operatorFirewalldWith(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "", "", "kipper-pods-out")
	require.Len(t, missing, 1)
	assert.Contains(t, missing[0], "the kipper-pods-out policy as Kipper wrote it")
}

func TestCheckOperatorFirewalldIgnoresDisabledPolicies(t *testing.T) {
	missing := operatorFirewalldWith(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "", "", "gateway-lan-to-world")
	assert.Empty(t, missing)

	gatewayDisable = "no"
	defer func() { gatewayDisable = "yes" }()
	missing = operatorFirewalldWith(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "", "", "gateway-lan-to-world")
	assert.Equal(t, []string{"no policy gateway-lan-to-world, which kip cannot evaluate"}, missing)
}

func TestCheckOperatorFirewalldReportsUnreadablePolicies(t *testing.T) {
	missing := operatorFirewalldWith(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "", "", "broken")
	assert.Equal(t, []string{"no policy broken, which kip cannot evaluate"}, missing)

	policiesFail = "yes"
	defer func() { policiesFail = "no" }()
	missing = operatorFirewalldWith(t, "public", "ssh http https 6443/tcp", "trusted", "ACCEPT", "", "", "")
	assert.Equal(t, []string{"a readable list of firewalld policies"}, missing)
}

// operatorPeerFirewalld runs the operator check against a fake firewalld that
// binds 192.0.2.10 to zone mesh, which opens meshPorts and holds rich, if set.
func operatorPeerFirewalld(t *testing.T, mesh FirewallMesh, meshPorts, rich string) []string {
	return operatorPeerFirewalldWith(t, mesh, meshPorts, rich, "", "")
}

// operatorPeerFirewalldWith also binds meshExtraSources to zone mesh and
// podExtraSources to the pod zone, each with a leading space.
func operatorPeerFirewalldWith(t *testing.T, mesh FirewallMesh, meshPorts, rich, meshExtraSources, podExtraSources string) []string {
	t.Helper()
	bin := t.TempDir()
	fakes := map[string]string{
		"sshd": "echo 'port 22'\n",
		"ip": `case "$*" in *"route get 192.0.2.20"*) echo '192.0.2.20 dev eth1 src 192.0.2.2 uid 0' ;; *) echo 'default via 192.0.2.1 dev eth0 proto static' ;; esac
`,
		"firewall-cmd": `case "$*" in
  *--get-zone-of-interface=eth0*) echo public ;;
  *--get-zone-of-interface=eth1*) echo internal ;;
  *--get-zone-of-source=192.0.2.10*) echo mesh ;;
  *--get-zone-of-source=10.42.0.0/16*) echo pods ;;
  *--get-zone-of-source=*) exit 2 ;;
  *--query-panic*) echo no ;;
  *--get-policies*) echo "" ;;
  *"--zone=mesh --list-rich-rules"*) echo "` + rich + `" ;;
  *--get-active-zones*) printf '%s\n' public "  interfaces: eth0" pods "  sources: 10.42.0.0/16` + podExtraSources + `" mesh "  sources: 192.0.2.10` + meshExtraSources + `" ;;
  *--info-zone=pods*) echo "  target: ACCEPT" ;;
  *--info-zone=*) echo "  target: default" ;;
  *--info-service=https*) echo "  ports: 443/tcp" ;;
  *"--zone=mesh --query-service=https"*) case "` + meshPorts + `" in *https*) exit 0 ;; esac; exit 1 ;;
  *"--zone=public --query-port="*) for p in 22/tcp 80/tcp 443/tcp 6443/tcp; do [ "$*" != "${*%=$p}" ] && exit 0; done; exit 1 ;;
  *"--zone=internal --query-port="*) for p in ` + meshPorts + `; do [ "$*" != "${*%=$p}" ] && exit 0; done; exit 1 ;;
  *"--zone=mesh --query-port="*) for p in ` + meshPorts + `; do [ "$*" != "${*%=$p}" ] && exit 0; done; exit 1 ;;
  *--query-*) exit 1 ;;
  *) ;;
esac
`,
	}
	for name, body := range fakes {
		//nolint:gosec // the fake commands have to be executable
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755))
	}
	runner := &localrun.Runner{Home: t.TempDir(), Dir: t.TempDir(), Path: bin + ":/usr/bin:/bin"}
	missing, err := CheckOperatorFirewalld(runner, mesh)
	require.NoError(t, err)
	return missing
}

func TestOperatorFirewalldAcceptsPeerSourceZones(t *testing.T) {
	worker := FirewallMesh{Role: RoleWorker, Peers: []FirewallPeer{{Address: "192.0.2.10", Server: true}}}
	assert.Equal(t, []string{"9100/tcp open to cluster node 192.0.2.10"}, operatorPeerFirewalld(t, worker, "51820/udp 10250/tcp", ""))
	assert.Empty(t, operatorPeerFirewalld(t, worker, "51820/udp 10250/tcp 9100/tcp", ""))
}

func TestOperatorFirewalldChecksOverridesInPeerZones(t *testing.T) {
	worker := FirewallMesh{Role: RoleWorker, Peers: []FirewallPeer{{Address: "192.0.2.10", Server: true}}}
	missing := operatorPeerFirewalld(t, worker, "51820/udp 10250/tcp 9100/tcp", `rule priority=\"-100\" source address=\"192.0.2.10\" drop`)
	assert.Equal(t, []string{"no rich rules in zone mesh, which kip cannot evaluate"}, missing)
}

// On the server a worker's source zone replaces the external zone, so the
// worker reaches the API only if that zone opens 6443 as well.
func TestOperatorFirewalldNeedsTheAPIInAWorkersZone(t *testing.T) {
	server := FirewallMesh{Role: RoleServer, Peers: []FirewallPeer{{Address: "192.0.2.10"}}}
	assert.Contains(t, operatorPeerFirewalld(t, server, "51820/udp 10250/tcp 80/tcp 443/tcp", ""), "6443/tcp open to cluster node 192.0.2.10")
	assert.Empty(t, operatorPeerFirewalld(t, server, "51820/udp 10250/tcp 80/tcp https 6443/tcp", ""),
		"a port opened through its service counts")
}

// A peer using a private interface needs access in that interface's zone.
func TestOperatorFirewalldChecksTheZoneOfThePeersInterface(t *testing.T) {
	server := FirewallMesh{Role: RoleServer, Peers: []FirewallPeer{{Address: "192.0.2.20"}}}
	missing := operatorPeerFirewalld(t, server, "80/tcp 443/tcp 6443/tcp", "")
	assert.Contains(t, missing, "51820/udp open to cluster node 192.0.2.20")
	assert.Contains(t, missing, "10250/tcp open to cluster node 192.0.2.20")
	// The fake's mesh zone holds another address, which stays unsupported here.
	assert.Equal(t, []string{"no sources bound to zone mesh, which kip cannot evaluate"},
		operatorPeerFirewalld(t, server, "51820/udp 10250/tcp 80/tcp 443/tcp 6443/tcp", ""))
}

// A subnet in a peer's zone could catch another peer whose ports were checked
// in a different zone, so only exact peer addresses may sit there.
func TestOperatorFirewalldRefusesOtherSourcesInAPeerZone(t *testing.T) {
	worker := FirewallMesh{Role: RoleWorker, Peers: []FirewallPeer{{Address: "192.0.2.10", Server: true}}}
	missing := operatorPeerFirewalldWith(t, worker, "51820/udp 10250/tcp 9100/tcp", "", " 198.51.100.0/24", "")
	assert.Equal(t, []string{"no source 198.51.100.0/24 bound to zone mesh, which kip cannot evaluate"}, missing)
}

func TestOperatorFirewalldRefusesPeerCatchingSourcesInThePodZone(t *testing.T) {
	worker := FirewallMesh{Role: RoleWorker, Peers: []FirewallPeer{{Address: "192.0.2.10", Server: true}}}
	missing := operatorPeerFirewalldWith(t, worker, "51820/udp 10250/tcp 9100/tcp", "", "", " 10.43.0.0/16 198.51.100.0/24")
	assert.Equal(t, []string{"no source 198.51.100.0/24 bound to zone pods, which kip cannot evaluate"}, missing)
}
