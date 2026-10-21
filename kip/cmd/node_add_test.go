package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

func nodeAddFlags(t *testing.T, host string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{}
	c.Flags().String("host", "", "")
	if host != "" {
		require.NoError(t, c.Flags().Set("host", host))
	}
	return c
}

func TestNodeAddHostTakesTheArgumentOrTheFlag(t *testing.T) {
	got, err := nodeAddHost(nodeAddFlags(t, ""), []string{"192.0.2.11"})
	require.NoError(t, err)
	assert.Equal(t, "192.0.2.11", got)

	got, err = nodeAddHost(nodeAddFlags(t, "worker.example.com"), nil)
	require.NoError(t, err)
	assert.Equal(t, "worker.example.com", got)

	_, err = nodeAddHost(nodeAddFlags(t, "192.0.2.12"), []string{"192.0.2.11"})
	assert.ErrorContains(t, err, "given twice")
	_, err = nodeAddHost(nodeAddFlags(t, ""), nil)
	assert.ErrorContains(t, err, "kip node add <host>")
	_, err = nodeAddHost(nodeAddFlags(t, ""), []string{"-oProxyCommand=id"})
	assert.Error(t, err)
}

func TestPeerAddressFollowsTheMode(t *testing.T) {
	n := installer.NodeNetwork{NodeIP: "10.0.0.4", ExternalIP: "192.0.2.11", Interface: "eth1"}
	assert.Equal(t, "10.0.0.4", peerAddress(installer.NetworkModePrivate, n))
	assert.Equal(t, "192.0.2.11", peerAddress(installer.NetworkModePublic, n))
}

// firewallHost answers the firewall audit with the given ufw and firewalld states.
type firewallHost struct {
	ufw, firewalld, osRelease string
	commands                  []string
}

func (h *firewallHost) Run(command string) (string, error) {
	h.commands = append(h.commands, command)
	switch {
	case strings.Contains(command, "is-active firewalld"):
		return h.firewalld + "\n", nil
	case strings.Contains(command, "ufw status"):
		return h.ufw + "\n", nil
	case command == "cat /etc/os-release":
		return h.osRelease, nil
	}
	return "", nil
}

func (h *firewallHost) RunStdin(command string, _ io.Reader) (string, error) { return h.Run(command) }
func (h *firewallHost) SurvivesSSHRateLimit() bool                           { return true }
func (h *firewallHost) ConfirmFreshConnection() error                        { return nil }
func (h *firewallHost) ObservedAddress(string) (string, error) {
	return "", errors.New("no fresh connection in this test")
}

var testMesh = installer.FirewallMesh{Peers: []installer.FirewallPeer{{Address: "192.0.2.11"}}}

func TestApplyPeerFirewallRefusesAnOperatorsUFW(t *testing.T) {
	err := applyPeerFirewall(io.Discard, &firewallHost{ufw: "active"}, "the server", testMesh, existingFirewall, false, false)
	assert.ErrorContains(t, err, "kip needs to manage")
	assert.ErrorContains(t, err, "so the join stops", "opening the ports by hand does not let this join continue")
	assert.ErrorContains(t, err, "kip cluster harden")
	assert.NotContains(t, err.Error(), "open 51820/udp")

	err = applyPeerFirewall(io.Discard, &firewallHost{ufw: "active"}, "192.0.2.11", testMesh, managedFirewall, false, false)
	assert.ErrorContains(t, err, "--firewall=false", "a joining node can keep its own firewall")
}

func TestApplyPeerFirewallLeavesAnExistingNodeWithoutFirewallAlone(t *testing.T) {
	h := &firewallHost{ufw: "inactive", firewalld: "inactive"}
	var out strings.Builder
	require.NoError(t, applyPeerFirewall(&out, h, "the server", testMesh, existingFirewall, false, false))
	for _, c := range h.commands {
		assert.NotContains(t, c, "ufw allow", "an operator who chose no firewall keeps none")
	}
	assert.Contains(t, out.String(), "Manage the firewall on the server yourself: allow 51820/udp and 10250/tcp from the other nodes")
}

func TestNewNodeFirewallFollowsTheServerUnlessTheFlagIsGiven(t *testing.T) {
	managed := installer.FirewallAudit{Existing: "ufw", KipperManaged: true}
	for _, tc := range []struct {
		name   string
		server installer.FirewallAudit
		flag   string
		want   nodeFirewall
	}{
		{"kip manages the server's firewall", managed, "", managedFirewall},
		{"the server has no firewall", installer.FirewallAudit{}, "", followedServerFirewall},
		{"the operator runs the server's ufw", installer.FirewallAudit{Existing: "ufw"}, "", followedServerFirewall},
		{"kip's ufw on the server is switched off", installer.FirewallAudit{KipperManaged: true}, "", managedFirewall},
		{"--firewall overrides a server without one", installer.FirewallAudit{}, "true", managedFirewall},
		{"--firewall=false overrides a managed server", managed, "false", declinedFirewall},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &cobra.Command{}
			c.Flags().Bool("firewall", true, "")
			if tc.flag != "" {
				require.NoError(t, c.Flags().Set("firewall", tc.flag))
			}
			assert.Equal(t, tc.want, newNodeFirewall(c, tc.server))
		})
	}
}

func TestPeerFirewallActionForEveryWorkerState(t *testing.T) {
	none := installer.FirewallAudit{}
	ours := installer.FirewallAudit{Existing: "ufw", KipperManaged: true}
	theirUFW := installer.FirewallAudit{Existing: "ufw"}
	theirFirewalld := installer.FirewallAudit{Existing: "firewalld", Adoptable: true}
	for _, tc := range []struct {
		name   string
		worker installer.FirewallAudit
		mode   nodeFirewall
		want   peerFirewallAction
	}{
		{"managed, no firewall", none, managedFirewall, configureFirewall},
		{"managed, ours", ours, managedFirewall, configureFirewall},
		{"managed, operator ufw", theirUFW, managedFirewall, refuseFirewall},
		{"managed, operator firewalld", theirFirewalld, managedFirewall, checkOperatorFirewalld},
		{"followed, no firewall", none, followedServerFirewall, leaveFirewall},
		{"followed, ours on a retry", ours, followedServerFirewall, configureFirewall},
		{"followed, operator ufw", theirUFW, followedServerFirewall, leaveFirewall},
		{"followed, operator firewalld", theirFirewalld, followedServerFirewall, leaveFirewall},
		{"declined, no firewall", none, declinedFirewall, leaveFirewall},
		{"declined, ours on a retry", ours, declinedFirewall, configureFirewall},
		{"declined, operator ufw", theirUFW, declinedFirewall, leaveFirewall},
		{"declined, operator firewalld", theirFirewalld, declinedFirewall, leaveFirewall},
		{"existing node, no firewall", none, existingFirewall, leaveFirewall},
		{"existing node, ours", ours, existingFirewall, configureFirewall},
		{"existing node, ours switched off", installer.FirewallAudit{KipperManaged: true}, existingFirewall, configureFirewall},
		{"existing node, operator ufw", theirUFW, existingFirewall, refuseFirewall},
		{"existing node, operator firewalld", theirFirewalld, existingFirewall, checkOperatorFirewalld},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, peerFirewallActionFor(tc.worker, tc.mode))
		})
	}
}

func TestANewNodeThatFollowsTheServerKeepsAnOperatorsUFWAndSaysWhichPortsToOpen(t *testing.T) {
	mesh := installer.FirewallMesh{Role: installer.RoleWorker, Peers: []installer.FirewallPeer{{Address: "192.0.2.10", Server: true}, {Address: "192.0.2.12"}}}
	for _, tc := range []struct {
		mode nodeFirewall
		want string
	}{
		{followedServerFirewall, "ℹ  Firewall setup skipped on 192.0.2.11: kip leaves firewall management to you, as on the server. In your firewall rules, allow 51820/udp and 10250/tcp from the other nodes (192.0.2.10, 192.0.2.12), and 9100/tcp from the server. Restrict these ports to those sources, keep 8888/tcp closed to outside traffic, and limit access to any NodePort services you expose. Use --firewall to enable firewall setup on this node."},
		{declinedFirewall, "⚠  Firewall setup skipped on 192.0.2.11 (--firewall=false). In your firewall rules, allow 51820/udp and 10250/tcp from the other nodes (192.0.2.10, 192.0.2.12), and 9100/tcp from the server. Restrict these ports to those sources, keep 8888/tcp closed to outside traffic, and limit access to any NodePort services you expose."},
	} {
		h := &firewallHost{ufw: "active"}
		var out strings.Builder
		require.NoError(t, applyPeerFirewall(&out, h, "192.0.2.11", mesh, tc.mode, true, false), "an inherited opt-out must not fail the join")
		assert.Contains(t, out.String(), tc.want)
		for _, c := range h.commands {
			assert.NotContains(t, c, "ufw allow")
			assert.NotContains(t, c, "ufw --force")
		}
	}
}

func TestNodeAddFailuresLeaveTheUsageOut(t *testing.T) {
	assert.True(t, nodeAddCmd.SilenceUsage)
}

func TestSSHRateLimitForKeepsAManagedFirewallsOwnRule(t *testing.T) {
	open := &firewallHost{ufw: "Status: active\n\n22/tcp                     ALLOW       Anywhere"}

	limit, err := sshRateLimitFor(open, installer.FirewallAudit{Existing: "ufw", KipperManaged: true}, true, false)
	require.NoError(t, err)
	assert.False(t, limit, "a rejoining worker keeps its unrestricted SSH rule")

	limit, err = sshRateLimitFor(open, installer.FirewallAudit{}, true, false)
	require.NoError(t, err)
	assert.True(t, limit, "a new firewall takes the server's setting")
}

type joinHost struct {
	responses map[string]string
	commands  []string
}

func (h *joinHost) Run(command string) (string, error) {
	h.commands = append(h.commands, command)
	for match, out := range h.responses {
		if strings.Contains(command, match) {
			return out, nil
		}
	}
	return "", nil
}

func (h *joinHost) RunStdin(command string, _ io.Reader) (string, error) { return h.Run(command) }
func (h *joinHost) SurvivesSSHRateLimit() bool                           { return true }
func (h *joinHost) ConfirmFreshConnection() error                        { return nil }
func (h *joinHost) ObservedAddress(string) (string, error) {
	return "", errors.New("no fresh connection in this test")
}

func TestPrepareJoinHostRefusesNetworkOptionsBeforeChangingTheHost(t *testing.T) {
	const machineID = "0123456789abcdef0123456789abcdef"
	for name, tc := range map[string]struct {
		opts installer.NetworkModeOptions
		want string
	}{
		"malformed private network":   {installer.NetworkModeOptions{PrivateNetwork: "invalid", ClusterMode: installer.NetworkModePrivate}, "not an IPv4 network"},
		"public in a private cluster": {installer.NetworkModeOptions{Public: true, ClusterMode: installer.NetworkModePrivate}, "cannot join it with --public"},
	} {
		t.Run(name, func(t *testing.T) {
			server := &joinHost{responses: map[string]string{"kubectl get nodes": "worker-1\n"}}
			worker := &joinHost{responses: map[string]string{
				"is-active cockpit.socket": "active\n",
				"cockpit.socket -p Listen": "[::]:9090 (Stream)\n",
			}}

			_, _, _, err := prepareJoinHost(server, worker, machineID, "192.0.2.11", tc.opts, true)

			require.ErrorContains(t, err, tc.want)
			for _, c := range server.commands {
				assert.NotContains(t, c, "cordon", "a refused option must leave a schedulable worker alone")
			}
			for _, c := range worker.commands {
				assert.NotContains(t, c, "systemctl disable", "a refused option must not harden the host")
			}
		})
	}
}

func TestANewNodeThatDoesNotManageItsFirewallLeavesAnOperatorsFirewalldUnread(t *testing.T) {
	for _, mode := range []nodeFirewall{followedServerFirewall, declinedFirewall} {
		h := &firewallHost{firewalld: "active", ufw: "inactive", osRelease: "ID=rocky\nID_LIKE=\"rhel centos fedora\"\n"}
		var out strings.Builder
		require.NoError(t, applyPeerFirewall(&out, h, "192.0.2.11", testMesh, mode, true, false))
		assert.Contains(t, out.String(), "Firewall setup skipped on 192.0.2.11")
		for _, c := range h.commands {
			assert.NotContains(t, c, "firewall-cmd", "the operator's firewalld is neither checked nor changed")
			assert.NotContains(t, c, "sshd -T")
		}
	}
}

func TestARetryThatDoesNotManageTheFirewallStillSaysKipUpdatedItsOwnRules(t *testing.T) {
	for _, mode := range []nodeFirewall{followedServerFirewall, declinedFirewall} {
		assert.Equal(t, "  ℹ  Updated peer rules on 192.0.2.11's existing kip-managed firewall. Existing kip-managed firewalls are updated even with --firewall=false.", managedFirewallNotice("192.0.2.11", mode))
	}
	for _, mode := range []nodeFirewall{managedFirewall, existingFirewall} {
		assert.Equal(t, "  ✔  Firewall on 192.0.2.11 opens the cluster ports to the other nodes", managedFirewallNotice("192.0.2.11", mode))
	}
}

func TestNodeAddIsListedInHelp(t *testing.T) {
	assert.False(t, nodeAddCmd.Hidden)
	assert.NotContains(t, nodeAddNotice, "preview")
	assert.Contains(t, nodeAddNotice, "sole control plane", "The notice explains that the server remains the sole control plane.")
}

// TestSSHRateLimitForDropsTheLimitWhenAsked covers a managed rule retained
// from an earlier install and the default for a new firewall.
func TestSSHRateLimitForDropsTheLimitWhenAsked(t *testing.T) {
	limited := &firewallHost{ufw: "Status: active\n\n22/tcp                     LIMIT       Anywhere"}

	limit, err := sshRateLimitFor(limited, installer.FirewallAudit{Existing: "ufw", KipperManaged: true}, true, true)
	require.NoError(t, err)
	assert.False(t, limit)

	limit, err = sshRateLimitFor(limited, installer.FirewallAudit{}, true, true)
	require.NoError(t, err)
	assert.False(t, limit)
}

func TestNodeAddOffersNoSSHRateLimit(t *testing.T) {
	assert.NotNil(t, nodeAddCmd.Flags().Lookup("no-ssh-rate-limit"))
}

func TestNodeAddNamesTheSSHLimitOnATimeout(t *testing.T) {
	err := explainSSHTimeout(errors.New(`listing the host's addresses: running "ip -o -4 addr show" on 192.0.2.11: exit status 255: ssh: connect to host 192.0.2.11 port 22: Operation timed out`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Operation timed out")
	assert.Contains(t, err.Error(), "192.0.2.11")
	assert.Contains(t, err.Error(), "22/tcp LIMIT")
	assert.Contains(t, err.Error(), "ufw allow 22/tcp")

	linux := fmt.Errorf("connecting to 192.0.2.11: %w", errors.New("ssh: connect to host 192.0.2.11 port 2222: Connection timed out"))
	err = explainSSHTimeout(linux)
	assert.ErrorIs(t, err, linux)
	assert.Contains(t, err.Error(), "ufw allow 2222/tcp")

	other := errors.New("ssh: connect to host 192.0.2.11 port 22: Connection refused")
	assert.Equal(t, other, explainSSHTimeout(other))
	assert.NoError(t, explainSSHTimeout(nil))
}

func TestNodeAddLogsInAsTheChosenUser(t *testing.T) {
	cluster := &config.Cluster{SSHUser: "rocky", Nodes: []config.NodeRecord{{Host: "203.0.113.12", SSHUser: "ubuntu"}}}

	login, err := nodeAddLogin(cluster, "203.0.113.11", "ec2-user")
	require.NoError(t, err)
	assert.Equal(t, "ec2-user", login)

	login, err = nodeAddLogin(cluster, "203.0.113.11", "")
	require.NoError(t, err)
	assert.Equal(t, "rocky", login, "without the flag a new node uses the cluster's login")

	login, err = nodeAddLogin(cluster, "203.0.113.12", "")
	require.NoError(t, err)
	assert.Equal(t, "ubuntu", login, "a node joining again keeps its recorded login")

	for _, bad := range []string{"rocky\n  ProxyCommand x", "-oProxyCommand=x", "a b"} {
		_, err := nodeAddLogin(cluster, "203.0.113.11", bad)
		assert.Error(t, err, bad)
	}
}

func TestNodeAddRecordsOnlyAChosenLogin(t *testing.T) {
	recorded := &installer.NodeRecord{Host: "203.0.113.11", SSHUser: "ec2-user"}

	assert.Equal(t, "ec2-user", recordedLogin(nil, "ec2-user"), "--ssh-user is recorded with the node")
	assert.Equal(t, "rocky", recordedLogin(nil, "rocky"), "--ssh-user is recorded even when it matches this workstation's login")
	assert.Empty(t, recordedLogin(nil, ""), "a new node without the flag follows the cluster's login")
	assert.Equal(t, "ec2-user", recordedLogin(recorded, ""), "joining again without the flag keeps the recorded login, whatever this workstation logs in as")
	assert.Equal(t, "ubuntu", recordedLogin(recorded, "ubuntu"), "--ssh-user replaces the recorded login")
}

func TestMirrorNodesTellsThisRunEachNodesLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cluster := &config.Cluster{Name: "prod", SSHUser: "rocky"}
	require.NoError(t, config.Save(&config.Config{CurrentCluster: "prod", Clusters: []config.Cluster{*cluster}}))

	mirrorNodes(cluster, []installer.NodeRecord{{Name: "worker-1", Host: "203.0.113.11", SSHUser: "ec2-user"}})

	assert.Equal(t, "ec2-user", nodeSSHUser(cluster, "203.0.113.11"), "a node saved during this run is reached as its own user")
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "ec2-user", cfg.GetCluster("prod").Nodes[0].SSHUser)
}

func TestWithRecordedNodesUsesTheClustersRecordedLogins(t *testing.T) {
	stale := &config.Cluster{Name: "prod", SSHUser: "rocky"}
	server := recordsOnlyServer{t: t, records: `{"0123456789abcdef0123456789abcdef":"{\"name\":\"worker-1\",\"host\":\"203.0.113.11\",\"machineID\":\"0123456789abcdef0123456789abcdef\",\"role\":\"worker\",\"state\":\"ready\",\"sshUser\":\"ec2-user\"}"}`}

	fresh, err := withRecordedNodes(server, stale)
	require.NoError(t, err)

	assert.Equal(t, "ec2-user", nodeSSHUser(fresh, "203.0.113.11"), "a node added from another workstation keeps its login")
	assert.Empty(t, stale.Nodes, "the loaded cluster stays as it was")

	unreadable, err := withRecordedNodes(recordsOnlyServer{t: t, err: errors.New("connection reset")}, stale)
	require.ErrorContains(t, err, "connection reset", "the caller decides whether to go on without the records")
	assert.Equal(t, "rocky", nodeSSHUser(unreadable, "203.0.113.11"), "unreadable records leave this machine's copy")
}

func TestNodeAddStopsWhenTheRecordsNameAnotherLogin(t *testing.T) {
	node := config.NodeRecord{Host: "worker-1.example.com", SSHUser: "ec2-user"}
	recorded := &config.Cluster{SSHUser: "rocky", Nodes: []config.NodeRecord{node}}

	err := confirmJoinLogin(recorded, &node, "worker-1.example.com", "", "rocky")
	require.Error(t, err, "a login picked while the records were unreadable must not overwrite the recorded one")
	assert.Contains(t, err.Error(), `"ec2-user"`)
	assert.Contains(t, err.Error(), "kip node add worker-1.example.com --ssh-user ec2-user")

	assert.Error(t, confirmJoinLogin(recorded, &node, "203.0.113.11", "", "rocky"), "the same machine reached by another address keeps its recorded login")
	assert.NoError(t, confirmJoinLogin(recorded, &node, "203.0.113.11", "", "ec2-user"))
	assert.NoError(t, confirmJoinLogin(recorded, &node, "worker-1.example.com", "rocky", "rocky"), "--ssh-user replaces the recorded login")

	followsCluster := config.NodeRecord{Host: "203.0.113.12"}
	assert.NoError(t, confirmJoinLogin(recorded, &followsCluster, "203.0.113.12", "", "rocky"), "a node without its own user follows the cluster")
	assert.Error(t, confirmJoinLogin(recorded, &followsCluster, "203.0.113.12", "", "ec2-user"))
	assert.NoError(t, confirmJoinLogin(recorded, nil, "203.0.113.13", "", "rocky"), "a new node follows the cluster")
}

func TestNodeSSHUserMatchesHostNamesInAnyCase(t *testing.T) {
	c := &config.Cluster{SSHUser: "rocky", Nodes: []config.NodeRecord{{Host: "Worker-1.example.com", SSHUser: "ec2-user"}}}
	assert.Equal(t, "ec2-user", nodeSSHUser(c, "worker-1.example.com"))
}
