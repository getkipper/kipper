package installer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/localrun"
)

// These fixtures model cloud hosts with /32 addresses and explicit private routes.
const (
	serverAddrs = "1: lo    inet 127.0.0.1/8 scope host lo\n" +
		"2: eth0    inet 203.0.113.10/32 metric 100 scope global dynamic eth0\n" +
		"3: enp7s0    inet 10.0.0.3/32 metric 100 scope global dynamic enp7s0\n" +
		"5: cni0    inet 10.42.0.1/24 brd 10.42.0.255 scope global cni0\n" +
		"189: flannel-wg    inet 10.42.0.0/32 scope global flannel-wg\n"
	joinerAddrs = "1: lo    inet 127.0.0.1/8 scope host lo\n" +
		"2: eth0    inet 203.0.113.11/32 scope global dynamic noprefixroute eth0\n" +
		"3: eth1    inet 10.0.0.4/32 brd 10.0.0.4 scope global dynamic eth1\n"
	defaultViaEth0 = "default via 172.31.1.1 dev eth0 proto dhcp src %s metric 100\n"
)

func networkHosts(serverRoutes, joinerRoutes []route) (*routedRunner, *routedRunner) {
	server := &routedRunner{routes: append(serverRoutes,
		route{contains: "ip -o -4 addr show", out: serverAddrs},
		route{contains: "route show default", out: strings.Replace(defaultViaEth0, "%s", "203.0.113.10", 1)},
		route{contains: "/sys/class/net/eth0/mtu", out: "9001\n"},
		route{contains: "/sys/class/net/enp7s0/mtu", out: "1450\n"},
		route{contains: "ip -o route get 10.0.0.4", out: "10.0.0.4 via 10.0.0.1 dev enp7s0 src 10.0.0.3 uid 0 \\    cache\n"},
		route{contains: "route show dev enp7s0", out: "10.0.0.0/16 via 10.0.0.1 proto dhcp src 10.0.0.3 metric 100\n10.0.0.1 proto dhcp scope link src 10.0.0.3 metric 100\n"},
		route{contains: "spec.podCIDR", out: "10.42.0.0/24"},
		route{contains: "service kubernetes", out: "10.43.0.1"},
	)}
	joiner := &routedRunner{routes: append(joinerRoutes,
		route{contains: "ip -o -4 addr show", out: joinerAddrs},
		route{contains: "route show default", out: strings.Replace(defaultViaEth0, "%s", "203.0.113.11", 1)},
		route{contains: "/sys/class/net/eth0/mtu", out: "1460\n"},
		route{contains: "/sys/class/net/eth1/mtu", out: "1450\n"},
		route{contains: "ip -o route get 10.0.0.3", out: "10.0.0.3 via 10.0.0.1 dev eth1 src 10.0.0.4 uid 0 \\    cache mtu 1450\n"},
		route{contains: "route show dev eth1", out: "10.0.0.0/16 via 10.0.0.1 proto dhcp src 10.0.0.4 metric 1003 mtu 1450\n"},
	)}
	return server, joiner
}

var external = NetworkModeOptions{ServerExternalIP: "203.0.113.10", JoinerExternalIP: "203.0.113.11"}

func TestPlanNodeNetworkFindsTheCloudPrivateNetwork(t *testing.T) {
	server, joiner := networkHosts(nil, nil)

	plan, err := PlanNodeNetwork(server, joiner, external)

	require.NoError(t, err)
	assert.Equal(t, NetworkPlan{
		Mode:   NetworkModePrivate,
		Server: NodeNetwork{NodeIP: "10.0.0.3", ExternalIP: "203.0.113.10", Interface: "enp7s0"},
		Joiner: NodeNetwork{NodeIP: "10.0.0.4", ExternalIP: "203.0.113.11", Interface: "eth1"},
	}, plan)
	assert.True(t, server.ran("</dev/tcp/10.0.0.4/22"), "the server proves it reaches the new host on the private path")
	assert.True(t, joiner.ran("</dev/tcp/10.0.0.3/6443"), "the new host proves it reaches the API on the private path")
}

func TestPlanNodeNetworkRefusesARoutedPrivatePathThatFailsItsCheck(t *testing.T) {
	server, joiner := networkHosts([]route{{contains: "/dev/tcp/10.0.0.4/", err: errors.New("exit status 124")}}, nil)

	_, err := PlanNodeNetwork(server, joiner, external)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "10.0.0.4:22")
	assert.Contains(t, err.Error(), "--public")

	server, joiner = networkHosts(nil, []route{{contains: "/dev/tcp/10.0.0.3/6443", err: errors.New("exit status 124")}})

	_, err = PlanNodeNetwork(server, joiner, external)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "10.0.0.3:6443")
}

func TestPlanNodeNetworkFallsBackToPublicWithoutAPrivateRoute(t *testing.T) {
	server, joiner := networkHosts([]route{{contains: "ip -o route get 10.0.0.4", out: "10.0.0.4 via 172.31.1.1 dev eth0 src 203.0.113.10 uid 0 \\    cache\n"}}, nil)

	plan, err := PlanNodeNetwork(server, joiner, external)

	require.NoError(t, err)
	assert.Equal(t, NetworkPlan{
		Mode:       NetworkModePublic,
		Server:     NodeNetwork{NodeIP: "203.0.113.10", ExternalIP: "203.0.113.10", TunnelMTU: 1360},
		Joiner:     NodeNetwork{NodeIP: "203.0.113.11", ExternalIP: "203.0.113.11", TunnelMTU: 1360},
		PodNetwork: "10.42.0.0/16",
	}, plan, "jumbo-frame and 1460-byte interfaces both leave a 1280-byte tunnel")

	server, joiner = networkHosts([]route{
		{contains: "ip -o route get 10.0.0.4", out: "10.0.0.4 via 172.31.1.1 dev eth0 src 203.0.113.10 uid 0 \\    cache\n"},
		{contains: "/sys/class/net/eth0/mtu", out: "1300\n"},
	}, nil)
	plan, err = PlanNodeNetwork(server, joiner, external)
	require.NoError(t, err)
	assert.Equal(t, 1300, plan.Server.TunnelMTU, "an interface below the cap keeps its own MTU")
}

func TestPlanNodeNetworkNeedsTheInterfaceMTUInPublicMode(t *testing.T) {
	server, joiner := networkHosts(nil, []route{{contains: "/sys/class/net/eth0/mtu", out: "unknown"}})
	opts := external
	opts.Public = true
	_, err := PlanNodeNetwork(server, joiner, opts)
	assert.ErrorContains(t, err, "MTU of eth0")

	server, joiner = networkHosts(nil, []route{{contains: "/sys/class/net/eth0/mtu", out: "1200\n"}})
	_, err = PlanNodeNetwork(server, joiner, opts)
	assert.ErrorContains(t, err, "eth0 has an MTU of 1200", "refused while planning, before the server changes")
}

func TestPlanNodeNetworkHonoursPublic(t *testing.T) {
	server, joiner := networkHosts(nil, nil)
	opts := external
	opts.Public = true

	plan, err := PlanNodeNetwork(server, joiner, opts)

	require.NoError(t, err)
	assert.Equal(t, NetworkModePublic, plan.Mode)
	assert.False(t, server.ran("route get 10.0.0.4"), "--public skips the private lookup")
}

// A host whose default route leaves through the private LAN only counts as
// private when the operator names the network.
func TestPlanNodeNetworkUsesANamedPrivateNetworkOnTheDefaultRoute(t *testing.T) {
	lanServer := []route{{contains: "route show default", out: "default via 192.168.1.1 dev enp7s0 src 10.0.0.3\n"}}
	lanJoiner := []route{{contains: "route show default", out: "default via 192.168.1.1 dev eth1 src 10.0.0.4\n"}}

	server, joiner := networkHosts(lanServer, lanJoiner)
	plan, err := PlanNodeNetwork(server, joiner, external)
	require.NoError(t, err)
	assert.Equal(t, NetworkModePublic, plan.Mode, "without --private-network the default route is not private")

	server, joiner = networkHosts(lanServer, lanJoiner)
	opts := external
	opts.PrivateNetwork = "10.0.0.0/16"
	plan, err = PlanNodeNetwork(server, joiner, opts)
	require.NoError(t, err)
	assert.Equal(t, NetworkModePrivate, plan.Mode)
	assert.Equal(t, "eth1", plan.Joiner.Interface)
}

func TestPlanNodeNetworkKeepsTheClustersMode(t *testing.T) {
	server, joiner := networkHosts(nil, nil)
	opts := external
	opts.ClusterMode = NetworkModePublic
	plan, err := PlanNodeNetwork(server, joiner, opts)
	require.NoError(t, err)
	assert.Equal(t, NetworkModePublic, plan.Mode, "one mode per cluster, even when a private path exists")

	server, joiner = networkHosts([]route{{contains: "/dev/tcp/", err: errors.New("exit status 124")}}, nil)
	opts.ClusterMode = NetworkModePrivate
	_, err = PlanNodeNetwork(server, joiner, opts)
	assert.ErrorContains(t, err, "no private path")

	opts.Public = true
	_, err = PlanNodeNetwork(server, joiner, opts)
	assert.ErrorContains(t, err, "private mode")
}

func TestPlanNodeNetworkRefusesAnUnderlayOverlappingTheClusterNetworks(t *testing.T) {
	server, joiner := networkHosts([]route{{contains: "route show dev enp7s0", out: "10.42.0.0/16 via 10.0.0.1 proto static\n"}}, nil)

	_, err := PlanNodeNetwork(server, joiner, external)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "overlaps the cluster's pod network 10.42.0.0/16")
}

func TestPlanNodeNetworkRefusesClusterNetworksItCannotRead(t *testing.T) {
	server, joiner := networkHosts([]route{{contains: "spec.podCIDR", out: "172.16.0.0/24"}}, nil)

	_, err := PlanNodeNetwork(server, joiner, external)

	assert.ErrorContains(t, err, "not inside 10.42.0.0/16")
}

func TestPlanNodeNetworkRejectsBadFlags(t *testing.T) {
	server, joiner := networkHosts(nil, nil)
	for _, opts := range []NetworkModeOptions{
		{PrivateNetwork: "10.0.0.0"},
		{PrivateNetwork: "fd00::/8"},
		{PrivateNetwork: "10.0.0.0/16", Public: true},
	} {
		_, err := PlanNodeNetwork(server, joiner, opts)
		assert.Error(t, err, "%+v", opts)
	}
}

func TestParseIPAddrsSkipsClusterInterfacesAndKeepsVLANs(t *testing.T) {
	out := serverAddrs + "7: enp7s0.4000@enp7s0    inet 192.168.100.3/24 brd 192.168.100.255 scope global enp7s0.4000\n" +
		"8: veth1234@if3    inet 10.42.0.9/32 scope global veth1234\n9: docker0    inet 172.17.0.1/16 scope global docker0\n"
	var names []string
	for _, a := range parseIPAddrs(out) {
		names = append(names, a.iface+"="+a.prefix.String())
	}
	assert.Equal(t, []string{"eth0=203.0.113.10/32", "enp7s0=10.0.0.3/32", "enp7s0.4000=192.168.100.3/24"}, names)
}

func TestParseRouteGet(t *testing.T) {
	r, ok := parseRouteGet("10.0.0.4 via 10.0.0.1 dev enp7s0 src 10.0.0.3 uid 0 \\    cache")
	assert.True(t, ok)
	assert.Equal(t, "enp7s0", r.iface)
	assert.Equal(t, "10.0.0.3", r.src.String())
	_, ok = parseRouteGet("local 10.0.0.3 dev lo src 10.0.0.3 uid 0")
	assert.False(t, ok, "an address on the host itself is no path")
}

func TestRenderNetworkDropIn(t *testing.T) {
	server, err := RenderNetworkDropIn("", RoleServer, NetworkModePrivate, NodeNetwork{NodeIP: "10.0.0.3", ExternalIP: "203.0.113.10", Interface: "enp7s0"})
	require.NoError(t, err)
	assert.Equal(t, "advertise-address: 10.0.0.3\nflannel-iface: enp7s0\nnode-external-ip: 203.0.113.10\nnode-ip: 10.0.0.3\ntls-san+:\n- 10.0.0.3\n", server)

	worker, err := RenderNetworkDropIn("", RoleWorker, NetworkModePrivate, NodeNetwork{NodeIP: "10.0.0.4", ExternalIP: "203.0.113.11", Interface: "eth1"})
	require.NoError(t, err)
	assert.Equal(t, "flannel-iface: eth1\nnode-external-ip: 203.0.113.11\nnode-ip: 10.0.0.4\n", worker)

	public, err := RenderNetworkDropIn(server+"agent-token: keep-me\n", RoleServer, NetworkModePublic, NodeNetwork{NodeIP: "203.0.113.10", ExternalIP: "203.0.113.10"})
	require.NoError(t, err)
	assert.Equal(t, "advertise-address: 203.0.113.10\nagent-token: keep-me\nflannel-external-ip: true\nnode-external-ip: 203.0.113.10\nnode-ip: 203.0.113.10\n", public,
		"other keys stay and settings of the old mode go")

	capped, err := RenderNetworkDropIn("", RoleWorker, NetworkModePublic, NodeNetwork{NodeIP: "203.0.113.11", ExternalIP: "203.0.113.11", TunnelMTU: 1360})
	require.NoError(t, err)
	assert.Equal(t, "flannel-conf: /etc/rancher/k3s/kipper-flannel.json\nnode-external-ip: 203.0.113.11\nnode-ip: 203.0.113.11\n", capped)

	private, err := RenderNetworkDropIn(capped, RoleWorker, NetworkModePrivate, NodeNetwork{NodeIP: "10.0.0.4", ExternalIP: "203.0.113.11", Interface: "eth1"})
	require.NoError(t, err)
	assert.NotContains(t, private, "flannel-conf", "private mode keeps flannel's own MTU")
}

// Workers in other networks reach the API through the server's external
// address, so a public-mode server advertises it even when kip install put the
// private address in the base config.
func TestRenderNetworkDropInAdvertisesThePublicServersExternalAddress(t *testing.T) {
	out, err := RenderNetworkDropIn("", RoleServer, NetworkModePublic, NodeNetwork{NodeIP: "172.31.28.180", ExternalIP: "203.0.113.10"})
	require.NoError(t, err)
	assert.Contains(t, out, "advertise-address: 203.0.113.10\n")
}

func TestFlannelConfCapsTheWireGuardMTU(t *testing.T) {
	conf, err := flannelConf("10.42.0.0/16", 1360)
	require.NoError(t, err)
	assert.JSONEq(t, `{"Network":"10.42.0.0/16","EnableIPv4":true,"EnableIPv6":false,`+
		`"Backend":{"Type":"wireguard","PersistentKeepaliveInterval":25,"MTU":1360}}`, conf)

	for _, bad := range []struct {
		network string
		mtu     int
	}{{"10.42.0.0/16", 0}, {"10.42.0.0/16", 1200}, {"10.42.0.0/16", 1361}, {"not-a-network", 1360}, {"fd00::/64", 1360}} {
		_, err := flannelConf(bad.network, bad.mtu)
		assert.Error(t, err, "%s %d", bad.network, bad.mtu)
	}
}

func TestWriteFlannelConfWritesTheFileItNames(t *testing.T) {
	conf, err := RenderFlannelConf("10.42.0.0/16", 1300)
	require.NoError(t, err)
	host := &routedRunner{}
	require.NoError(t, WriteFlannelConf(host, conf))
	require.Len(t, host.stdins, 1)
	assert.Contains(t, host.stdins[0], `"MTU":1300`)
	assert.Contains(t, host.commands[0], flannelConfPath)

	conf, err = RenderFlannelConf("10.42.0.0/16", 0)
	require.NoError(t, err)
	assert.Empty(t, conf, "private mode has no flannel configuration")
	host = &routedRunner{}
	require.NoError(t, WriteFlannelConf(host, conf))
	assert.Empty(t, host.commands)
}

func TestRenderNetworkDropInRefusesBadInput(t *testing.T) {
	_, err := RenderNetworkDropIn("not: [yaml", RoleServer, NetworkModePublic, NodeNetwork{NodeIP: "203.0.113.10", ExternalIP: "203.0.113.10"})
	assert.Error(t, err)
	_, err = RenderNetworkDropIn("", RoleServer, NetworkModePrivate, NodeNetwork{NodeIP: "10.0.0.3", ExternalIP: "203.0.113.10", Interface: "eth0; reboot"})
	assert.Error(t, err)
	_, err = RenderNetworkDropIn("", RoleWorker, NetworkModePublic, NodeNetwork{NodeIP: "10.0.0.3\nselinux: false", ExternalIP: "203.0.113.10"})
	assert.Error(t, err)
}

func TestPlanNodeNetworkRefusesAnSSHPortThatIsNotANumber(t *testing.T) {
	for _, port := range []string{"22; id #", "22'", "0", "65536", "-1"} {
		server, joiner := networkHosts(nil, nil)
		opts := external
		opts.SSHPort = port
		_, err := PlanNodeNetwork(server, joiner, opts)
		assert.Error(t, err, port)
		assert.False(t, server.ran("/dev/tcp/"), "no probe runs with port %q", port)
	}
	server, joiner := networkHosts(nil, nil)
	opts := external
	opts.SSHPort = "2222"
	_, err := PlanNodeNetwork(server, joiner, opts)
	require.NoError(t, err)
	assert.True(t, server.ran("</dev/tcp/10.0.0.4/2222"))
}

func TestRenderNetworkDropInTreatsAnEmptyDocumentAsNoSettings(t *testing.T) {
	for _, existing := range []string{"null\n", "~\n", "---\n", "# nothing here\n"} {
		out, err := RenderNetworkDropIn(existing, RoleWorker, NetworkModePublic, NodeNetwork{NodeIP: "203.0.113.11", ExternalIP: "203.0.113.11"})
		require.NoError(t, err, existing)
		assert.Equal(t, "node-external-ip: 203.0.113.11\nnode-ip: 203.0.113.11\n", out, existing)
	}
	_, err := RenderNetworkDropIn("- a list\n", RoleWorker, NetworkModePublic, NodeNetwork{NodeIP: "203.0.113.11", ExternalIP: "203.0.113.11"})
	assert.Error(t, err)
}

func TestCIDRSettingsReadTheServersConfiguration(t *testing.T) {
	read := func(files, unit, name, def string) (string, error) {
		settings, err := cidrSettings(files, unit)
		if err != nil {
			return "", err
		}
		p, err := settingOrDefault(settings, name, def)
		return p.String(), err
	}
	got, err := read("==> /etc/rancher/k3s/config.yaml\ndisable:\n  - traefik\n", "{ path=/usr/local/bin/k3s ; argv[]=/usr/local/bin/k3s server ; }", "cluster-cidr", k3sPodCIDR)
	require.NoError(t, err)
	assert.Equal(t, "10.42.0.0/16", got, "unset means k3s's default")

	got, err = read("==> /etc/rancher/k3s/config.yaml.d/a.yaml\n\"cluster-cidr\": 10.40.0.0/14\n", "", "cluster-cidr", k3sPodCIDR)
	require.NoError(t, err)
	assert.Equal(t, "10.40.0.0/14", got, "a quoted key is still the key")

	got, err = read("", "{ argv[]=/usr/local/bin/k3s server --service-cidr 10.53.0.0/16 ; }", "service-cidr", k3sServiceCIDR)
	require.NoError(t, err)
	assert.Equal(t, "10.53.0.0/16", got)

	got, err = read("", "{ argv[]=/usr/local/bin/k3s server --service-cidr=10.54.0.0/16 ; }", "service-cidr", k3sServiceCIDR)
	require.NoError(t, err)
	assert.Equal(t, "10.54.0.0/16", got)

	_, err = read("==> /etc/rancher/k3s/config.yaml\ncluster-cidr: 10.52.0.0/16,fd00::/56\n", "", "cluster-cidr", k3sPodCIDR)
	assert.Error(t, err, "dual-stack is out of scope")
	_, err = read("==> /etc/rancher/k3s/config.yaml\ncluster-cidr: 10.52.0.0/16\n==> /etc/rancher/k3s/config.yaml.d/b.yaml\ncluster-cidr: 10.62.0.0/16\n", "", "cluster-cidr", k3sPodCIDR)
	assert.Error(t, err, "two different settings")
	_, err = read("==> /etc/rancher/k3s/config.yaml\nnot: [yaml\n", "", "cluster-cidr", k3sPodCIDR)
	assert.Error(t, err)
	_, err = read("", "{ argv[]=/usr/local/bin/k3s server '--cluster-cidr' '10.40.0.0/14' ; }", "cluster-cidr", k3sPodCIDR)
	assert.Error(t, err, "an argument kip cannot read fails closed")
	for _, unit := range []string{
		"{ argv[]=/usr/local/bin/k3s server --config /etc/rancher/k3s/custom.yaml ; }",
		"{ argv[]=/usr/local/bin/k3s server -c /etc/rancher/k3s/custom.yaml ; }",
		"{ argv[]=/usr/local/bin/k3s server --config=/etc/rancher/k3s/custom.yaml ; }",
		"K3S_CONFIG_FILE=/etc/rancher/k3s/custom.yaml",
	} {
		_, err = read("", unit, "cluster-cidr", k3sPodCIDR)
		assert.Error(t, err, "another configuration file fails closed: %s", unit)
	}
}

func TestPlanNodeNetworkRefusesAPodRangeWiderThanTheConfiguredNetwork(t *testing.T) {
	server, joiner := networkHosts([]route{
		{contains: "/etc/rancher/k3s/config.yaml.d/*.yaml", out: "==> /etc/rancher/k3s/config.yaml\ncluster-cidr: 10.42.0.0/25\n"},
	}, nil)
	_, err := PlanNodeNetwork(server, joiner, external)
	assert.ErrorContains(t, err, "is not inside 10.42.0.0/25")
}

func TestPlanNodeNetworkChecksAConfiguredPodNetwork(t *testing.T) {
	server, joiner := networkHosts([]route{
		{contains: "/etc/rancher/k3s/config.yaml.d/*.yaml", out: "==> /etc/rancher/k3s/config.yaml\ncluster-cidr: 10.0.0.0/16\n"},
		{contains: "spec.podCIDR", out: "10.0.5.0/24"},
	}, nil)
	_, err := PlanNodeNetwork(server, joiner, external)
	assert.ErrorContains(t, err, "overlaps the cluster's pod network 10.0.0.0/16")
}

func TestPlanNodeNetworkNeedsTheServersPodRange(t *testing.T) {
	server, joiner := networkHosts([]route{{contains: "spec.podCIDR", out: ""}}, nil)
	_, err := PlanNodeNetwork(server, joiner, external)
	assert.ErrorContains(t, err, "no pod range")
}

func TestK3sUnitSettingsCommandReadsEnvironmentFilesAndFailsOnErrors(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "k3s custom.env")
	require.NoError(t, os.WriteFile(envFile, []byte("K3S_TOKEN=secret\nK3S_CONFIG_FILE=/etc/rancher/k3s/custom.yaml\n"), 0o600))
	write := func(body string) {
		//nolint:gosec // the fake command has to be executable
		require.NoError(t, os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\n"+body), 0o755))
	}
	runner := &localrun.Runner{Home: t.TempDir(), Dir: t.TempDir(), Path: dir + ":/usr/bin:/bin"}

	write(`case "$*" in *EnvironmentFiles*) echo "` + envFile + ` (ignore_errors=yes) /nonexistent/k3s (ignore_errors=yes)" ;; *) echo "{ argv[]=/usr/local/bin/k3s server ; }" ;; esac` + "\n")
	out, err := runner.Run(k3sUnitSettingsCommand)
	require.NoError(t, err)
	assert.Contains(t, out, "K3S_CONFIG_FILE=/etc/rancher/k3s/custom.yaml")
	assert.NotContains(t, out, "secret", "only the selector line leaves the host")
	_, err = cidrSettings("", out)
	assert.Error(t, err)

	write("exit 1\n")
	_, err = runner.Run(k3sUnitSettingsCommand)
	assert.Error(t, err, "an unreadable unit is not an absent setting")

	require.NoError(t, os.Chmod(envFile, 0))
	t.Cleanup(func() { _ = os.Chmod(envFile, 0o600) })
	write(`case "$*" in *EnvironmentFiles*) echo "` + envFile + ` (ignore_errors=yes)" ;; *) echo "{ argv[]=/usr/local/bin/k3s server ; }" ;; esac` + "\n")
	if os.Geteuid() != 0 {
		_, err = runner.Run(k3sUnitSettingsCommand)
		assert.Error(t, err, "an unreadable environment file is not an absent setting")
	}
}

func TestPlanNodeNetworkNeedsTheAPIInPublicMode(t *testing.T) {
	server, joiner := networkHosts(nil, []route{{contains: "/dev/tcp/203.0.113.10/6443", err: errors.New("exit status 124")}})
	opts := external
	opts.Public = true
	_, err := PlanNodeNetwork(server, joiner, opts)
	assert.ErrorContains(t, err, "cannot reach the server's API at 203.0.113.10:6443")
}

func TestPlanNodeNetworkSaysAPrivateClusterNeedsAPrivatePath(t *testing.T) {
	server, joiner := networkHosts([]route{{contains: "ip -o route get 10.0.0.4", out: "10.0.0.4 via 172.31.1.1 dev eth0 src 203.0.113.10 uid 0 \\    cache\n"}}, nil)
	opts := external
	opts.ClusterMode = NetworkModePrivate

	_, err := PlanNodeNetwork(server, joiner, opts)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "the cluster runs in private mode")
	assert.Contains(t, err.Error(), "Attach the new host to the cluster's private network")
}
