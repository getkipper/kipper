package installer

import (
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const vxlanConfig = "tls-san:\n  - \"203.0.113.10\"\ndisable:\n  - traefik\nresolv-conf: /etc/rancher/k3s/resolv.conf\n"

// netHost simulates a k3s server through a network change: the files kip
// touches, whether k3s runs, which flannel link exists and what flannel
// reports. With wgFails, k3s never comes up on WireGuard.
type netHost struct {
	config, dropIn       string
	dropInPresent        bool
	backupConfig         string
	backupDropIn         string
	backupDropInAbsent   bool
	savedReady           string
	snapshotSaved        bool
	record               string
	active               bool
	links                map[string]bool
	backend              string
	nodes                int
	wgLoads, wgFails     bool
	workloadsAfterChange string
	started              int
	commands             []string
	internalIP           string
	keepsNodeIP          bool
	addressReadFailures  int
}

func newNetHost() *netHost {
	return &netHost{
		config: vxlanConfig, active: true, links: map[string]bool{"flannel.1": true},
		backend: "vxlan", nodes: 1, wgLoads: true,
	}
}

const workloadListing = "Deployment/gate/web 1 1\nStatefulSet/gate/db 1 1\nDeployment/gate/stopped 0 \nDaemonSet/monitoring/promtail 1 1\n"

func (h *netHost) Run(cmd string) (string, error) { return h.RunStdin(cmd, strings.NewReader("")) }

// unfence extracts the command wrapped by fencedRunner.fence for test matching.
// It returns cmd unchanged if the subshell opening is absent.
func unfence(cmd string) string {
	_, body, ok := strings.Cut(cmd, "\n(\n")
	if !ok {
		return cmd
	}
	body, _, _ = strings.Cut(body, "\n); rc=$?; touch -c "+apiServerLockPath)
	return body
}

func (h *netHost) RunStdin(cmd string, in io.Reader) (string, error) {
	fencePrefix := "exec 9>" + apiServerLockPath + ".guard"
	if strings.HasPrefix(cmd, fencePrefix) {
		if strings.Contains(cmd, "mkdir "+apiServerLockPath) {
			return lockAcquiredMarker + "\n", nil
		}
		if strings.Contains(cmd, "rm -rf "+apiServerLockPath) {
			return "", nil
		}
		cmd = unfence(cmd)
	}
	h.commands = append(h.commands, cmd)
	stdin, _ := io.ReadAll(in)
	down := errors.New("The connection to the server 127.0.0.1:6443 was refused")
	switch {
	case strings.HasPrefix(cmd, "[ -f "+networkChangeRecord+" ]"), cmd == "cat "+networkChangeRecord:
		return h.record, nil
	case strings.Contains(cmd, "backend-type"):
		if !h.active {
			return "", down
		}
		return h.backend + "\n", nil
	case cmd == "kubectl get nodes -o name":
		return strings.Repeat("node/n\n", h.nodes), nil
	case cmd == "modprobe wireguard":
		if !h.wgLoads {
			return "modprobe: FATAL: Module wireguard not found", errors.New("exit status 1")
		}
		return "", nil
	case strings.HasPrefix(cmd, "grep -l '^flannel-backend:'"):
		return "", nil
	case strings.Contains(cmd, "get deployments,statefulsets,daemonsets -A"):
		if !h.active {
			return "", down
		}
		if h.started > 0 && h.workloadsAfterChange != "" {
			return h.workloadsAfterChange, nil
		}
		return workloadListing, nil
	case strings.HasPrefix(cmd, "if [ ! -e "+k3sConfigPath):
		return h.config, nil
	case strings.Contains(cmd, networkChangeBackup+"/config.yaml") && strings.Contains(cmd, "from="):
		h.backupConfig, h.backupDropIn, h.backupDropInAbsent = h.config, h.dropIn, !h.dropInPresent
		h.savedReady, h.snapshotSaved = string(stdin), true
		h.record = strings.ReplaceAll(strings.SplitN(strings.SplitN(cmd, "printf '", 2)[1], "' > ", 2)[0], `\n`, "\n")
	case strings.HasPrefix(cmd, "if [ -e "+k3sNetworkConfigPath):
		return h.dropIn, nil
	case strings.Contains(cmd, `mv "$staged" `+k3sNetworkConfigPath):
		h.dropIn, h.dropInPresent = string(stdin), true
	case cmd == "systemctl stop k3s":
		h.active = false
	case strings.HasSuffix(cmd, resetFlannelCommand):
		h.links = map[string]bool{}
	case strings.Contains(cmd, `cat > "$staged"`) && strings.Contains(cmd, k3sConfigPath):
		h.config = string(stdin)
	case strings.Contains(cmd, `type=="InternalIP"`):
		if h.addressReadFailures > 0 {
			h.addressReadFailures--
			return "", errors.New("exit status 1")
		}
		return h.internalIP, nil
	case cmd == "systemctl start --no-block k3s":
		h.started++
		if ip, err := dropInNodeIP(h.dropIn); err == nil && h.dropInPresent && !h.keepsNodeIP {
			h.internalIP = ip
		}
		if strings.Contains(h.config, "flannel-backend: "+wireguardBackend) {
			if h.wgFails {
				return "", nil
			}
			h.active, h.links["flannel-wg"], h.backend = true, true, "wireguard"
		} else {
			h.active, h.links["flannel.1"], h.backend = true, true, "vxlan"
		}
	case strings.Contains(cmd, networkUpMarker):
		for link := range h.links {
			if h.active && strings.Contains(cmd, "ip link show "+link+" ") {
				return networkUpMarker + "\n", nil
			}
		}
	case strings.Contains(cmd, "no backup of the k3s configuration"):
		if h.backupConfig == "" {
			return "no backup", errors.New("exit status 1")
		}
		h.config = h.backupConfig
		if h.backupDropInAbsent {
			h.dropIn, h.dropInPresent = "", false
		} else {
			h.dropIn, h.dropInPresent = h.backupDropIn, true
		}
	case strings.Contains(cmd, networkChangeBackup+"/ready-workloads ]"):
		if h.savedReady == "" && !h.snapshotSaved {
			return noSnapshotMarker + "\n", nil
		}
		return h.savedReady, nil
	case cmd == "rm -rf "+networkChangeRecord+" "+networkChangeBackup:
		h.record = ""
	}
	return "", nil
}

func (h *netHost) index(fragment string) int {
	for i, c := range h.commands {
		if strings.Contains(c, fragment) {
			return i
		}
	}
	return -1
}

var fastNetworkChange = networkChangeTimeouts{start: 50 * time.Millisecond, workloads: 50 * time.Millisecond, confirm: 50 * time.Millisecond, interval: time.Millisecond}

func TestChangeNetworkMovesASingleNodeServerToWireGuard(t *testing.T) {
	h := newNetHost()

	require.NoError(t, changeNetwork(h, fastNetworkChange))

	assert.Contains(t, h.config, "flannel-backend: wireguard-native\n")
	assert.Contains(t, h.config, "resolv-conf: /etc/rancher/k3s/resolv.conf", "the rest of the config stays")
	assert.True(t, h.links["flannel-wg"])
	assert.False(t, h.links["flannel.1"], "a VXLAN link left behind keeps routes the WireGuard backend does not own")
	assert.False(t, h.dropInPresent, "k3s fails its next start once an agent token is added to a running server")
	assert.Empty(t, h.record, "a finished change leaves no record")

	stop, reset, write, start := h.index("systemctl stop k3s"), h.index(resetFlannelCommand), h.index(`cat > "$staged"`), h.index("systemctl start")
	require.NotEqual(t, -1, stop)
	assert.Less(t, h.index(networkChangeBackup+"/config.yaml"), stop, "the backup comes first")
	assert.Less(t, stop, reset, "flannel recreates its link while k3s runs")
	assert.Less(t, reset, write)
	assert.Less(t, write, start)
}

func TestChangeNetworkRestoresVXLANWhenWireGuardDoesNotComeUp(t *testing.T) {
	h := newNetHost()
	h.wgFails = true

	err := changeNetwork(h, fastNetworkChange)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "previous configuration is back in place")
	assert.Contains(t, err.Error(), "flannel link flannel-wg")
	assert.Equal(t, vxlanConfig, h.config)
	assert.False(t, h.dropInPresent, "the drop-in did not exist before")
	assert.True(t, h.links["flannel.1"])
	assert.Equal(t, "vxlan", h.backend)
	assert.Empty(t, h.record)
	assert.Equal(t, 2, h.started)
}

func TestChangeNetworkRestoresWhenWorkloadsDoNotRecover(t *testing.T) {
	h := newNetHost()
	h.workloadsAfterChange = "Deployment/gate/web 1 0\nStatefulSet/gate/db 1 1\nDaemonSet/monitoring/promtail 1 1\n"

	err := changeNetwork(h, fastNetworkChange)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Deployment/gate/web")
	assert.Equal(t, vxlanConfig, h.config)
}

func TestChangeNetworkRefusesAClusterWithAWorker(t *testing.T) {
	h := newNetHost()
	h.nodes = 2

	err := changeNetwork(h, fastNetworkChange)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "only node")
	assert.Contains(t, err.Error(), "kubectl delete node", "the remedy names a command that exists")
	assert.NotContains(t, err.Error(), "kip node remove", "kip has no node removal command yet")
	assert.Contains(t, err.Error(), "kip node add", "a removed worker comes back with kip node add")
	assert.Equal(t, -1, h.index("systemctl stop k3s"), "a worker's flannel would lose the server mid-change")
}

func TestChangeNetworkRefusesAKernelWithoutWireGuard(t *testing.T) {
	h := newNetHost()
	h.wgLoads = false

	err := changeNetwork(h, fastNetworkChange)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "wireguard")
	assert.Equal(t, -1, h.index("systemctl stop k3s"))
}

func TestChangeNetworkLeavesAWireGuardServerAlone(t *testing.T) {
	h := newNetHost()
	h.backend, h.links = "wireguard", map[string]bool{"flannel-wg": true}

	require.NoError(t, changeNetwork(h, fastNetworkChange))

	assert.Equal(t, -1, h.index("systemctl stop"))
}

func TestChangeNetworkFinishesAnInterruptedChangeThatCameUp(t *testing.T) {
	h := newNetHost()
	h.record, h.backend, h.links = "from=vxlan\n", "wireguard", map[string]bool{"flannel-wg": true}
	h.snapshotSaved = true

	require.NoError(t, changeNetwork(h, fastNetworkChange))

	assert.Empty(t, h.record)
	assert.Equal(t, -1, h.index("systemctl stop"), "a server already serving on WireGuard needs no restart")
}

func TestChangeNetworkUndoesAnInterruptedChangeBeforeStartingAgain(t *testing.T) {
	h := newNetHost()
	// A run that died after writing the new config, with k3s stopped.
	h.record, h.backupConfig, h.backupDropInAbsent, h.snapshotSaved = "from=vxlan\n", vxlanConfig, true, true
	h.config = vxlanConfig + "flannel-backend: wireguard-native\n"
	h.active, h.links = false, map[string]bool{}

	require.NoError(t, changeNetwork(h, fastNetworkChange))

	assert.Equal(t, 2, h.started, "first back to VXLAN, then the change itself")
	assert.Equal(t, "wireguard", h.backend)
	assert.Empty(t, h.record)
}

func TestWithFlannelBackend(t *testing.T) {
	got, err := withFlannelBackend(vxlanConfig, wireguardBackend)
	require.NoError(t, err)
	assert.Equal(t, vxlanConfig+"flannel-backend: wireguard-native\n", got)

	got, err = withFlannelBackend("flannel-backend: vxlan\nresolv-conf: /x\n", wireguardBackend)
	require.NoError(t, err)
	assert.Equal(t, "flannel-backend: wireguard-native\nresolv-conf: /x\n", got)
}

func TestNetworkChangeConsent(t *testing.T) {
	h := newNetHost()

	_, err := networkChangeConsent(h, true, Options{})
	assert.ErrorIs(t, err, ErrNetworkChangeNotConfirmed, "a re-run must not restart k3s without the operator agreeing")

	ok, err := networkChangeConsent(h, true, Options{ChangeNetwork: true})
	require.NoError(t, err)
	assert.True(t, ok)

	asked := ""
	ok, err = networkChangeConsent(h, true, Options{ConfirmNetworkChange: func(q string) bool { asked = q; return true }})
	require.NoError(t, err)
	assert.True(t, ok)
	assert.NotEmpty(t, asked)

	ok, err = networkChangeConsent(h, false, Options{})
	require.NoError(t, err)
	assert.False(t, ok, "a fresh install starts on WireGuard")
}

func TestInstallK3sRefusesAVXLANServerWithoutConsent(t *testing.T) {
	h := &k3sInstallHost{installed: true, backend: "vxlan"}

	err := InstallK3s(h, "203.0.113.10", nil, true, false)

	require.ErrorIs(t, err, ErrNetworkChangeNotConfirmed)
	assert.Equal(t, -1, h.index("cat > /etc/rancher/k3s/config.yaml"), "the template alone would switch flannel without removing the VXLAN link")
}

func TestK3sTemplateRunsWireGuard(t *testing.T) {
	assert.Contains(t, k3sConfig, "flannel-backend: wireguard-native\n")
}

func TestChangeNetworkKeepsTheConfigOffCommandLines(t *testing.T) {
	h := newNetHost()
	h.config = vxlanConfig + "token: secret-test-value\n"

	require.NoError(t, changeNetwork(h, fastNetworkChange))

	assert.Contains(t, h.config, "token: secret-test-value", "every other setting stays")
	for _, c := range h.commands {
		assert.NotContains(t, c, "secret-test-value", "a command line reaches the process list and error messages")
		assert.NotContains(t, c, base64Of("token: secret-test-value"))
	}
}

func base64Of(s string) string { return base64.StdEncoding.EncodeToString([]byte(s))[:16] }

func TestChangeNetworkStopsWhenTheOldLinkCannotBeRemoved(t *testing.T) {
	h := &resetFailingHost{newNetHost()}

	err := changeNetwork(h, fastNetworkChange)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "removing the old flannel link")
	assert.Equal(t, vxlanConfig, h.config, "the new backend is never written next to a stale link")
}

// resetFailingHost fails link deletion until k3s has restarted.
type resetFailingHost struct{ *netHost }

func (h *resetFailingHost) Run(cmd string) (string, error) {
	return h.RunStdin(cmd, strings.NewReader(""))
}

func (h *resetFailingHost) RunStdin(cmd string, in io.Reader) (string, error) {
	if strings.HasSuffix(unfence(cmd), resetFlannelCommand) && h.started == 0 {
		h.commands = append(h.commands, cmd)
		return "flannel.1 is still present", errors.New("exit status 1")
	}
	return h.netHost.RunStdin(cmd, in)
}

func TestChangeNetworkUndoesAnInterruptedChangeWhoseWorkloadsDidNotRecover(t *testing.T) {
	h := newNetHost()
	// The earlier run got WireGuard up but died while web was still down.
	h.record, h.backupConfig, h.backupDropInAbsent = "from=vxlan\n", vxlanConfig, true
	h.savedReady = "Deployment/gate/web\nStatefulSet/gate/db\n"
	h.config = vxlanConfig + "flannel-backend: wireguard-native\n"
	h.backend, h.links, h.started = "wireguard", map[string]bool{"flannel-wg": true}, 1
	h.workloadsAfterChange = "Deployment/gate/web 1 0\nStatefulSet/gate/db 1 1\n"

	err := recoverNetworkChangeForTest(h)

	require.Error(t, err, "web stays down on VXLAN too in this fake, so the restore reports it")
	assert.Equal(t, vxlanConfig, h.config, "a change whose workloads did not come back is undone, not recorded as done")
}

func TestRecoverNetworkChangeWorksWhileK3sIsStopped(t *testing.T) {
	h := newNetHost()
	h.record, h.backupConfig, h.backupDropInAbsent = "from=vxlan\n", vxlanConfig, true
	h.savedReady = "Deployment/gate/web\n"
	h.config = vxlanConfig + "flannel-backend: wireguard-native\n"
	h.active, h.links = false, map[string]bool{}

	require.NoError(t, recoverNetworkChangeForTest(h))

	assert.Equal(t, vxlanConfig, h.config)
	assert.True(t, h.active)
	assert.Equal(t, "vxlan", h.backend)
	assert.Empty(t, h.record)
}

func TestRecoverNetworkChangeWithoutARecordDoesNothing(t *testing.T) {
	h := newNetHost()

	require.NoError(t, recoverNetworkChangeForTest(h))

	assert.Equal(t, -1, h.index("systemctl stop"))
}

func recoverNetworkChangeForTest(client CommandRunner) error {
	locked, release, err := lockAPIServerConfig(client)
	if err != nil {
		return err
	}
	defer release()
	return finishInterrupted(locked, fastNetworkChange)
}

func TestRecoverNetworkChangeRefusesARecordWithoutItsWorkloadList(t *testing.T) {
	h := newNetHost()
	h.record, h.backupConfig, h.backupDropInAbsent = "from=vxlan\n", vxlanConfig, true
	h.backend, h.links = "wireguard", map[string]bool{"flannel-wg": true}

	err := recoverNetworkChangeForTest(h)

	require.Error(t, err)
	assert.NotEmpty(t, h.record, "without the list, finishing would skip the check that decides between done and undo")
}

const privateServerDropIn = "advertise-address: 10.0.0.3\nflannel-iface: enp7s0\nnode-external-ip: 203.0.113.10\nnode-ip: 10.0.0.3\ntls-san+:\n- 10.0.0.3\n"

func TestChangeServerNetworkMovesAWireGuardServerToItsPrivateAddress(t *testing.T) {
	h := newNetHost()
	h.config, h.backend, h.links, h.internalIP = vxlanConfig+"flannel-backend: wireguard-native\n", "wireguard", map[string]bool{"flannel-wg": true}, "203.0.113.10"
	dropIn := privateServerDropIn

	require.NoError(t, changeNetworkTo(h, fastNetworkChange, &dropIn))

	assert.Equal(t, privateServerDropIn, h.dropIn)
	assert.Equal(t, "10.0.0.3", h.internalIP)
	assert.Empty(t, h.record)
	assert.Less(t, h.index("systemctl stop k3s"), h.index(`mv "$staged" `+k3sNetworkConfigPath), "the drop-in changes while k3s is stopped")
	assert.Less(t, h.index(networkChangeBackup+"/config.yaml"), h.index("systemctl stop k3s"), "the old drop-in is backed up first")
}

func TestChangeServerNetworkAlsoMovesAVXLANServerToWireGuard(t *testing.T) {
	h := newNetHost()
	dropIn := privateServerDropIn

	require.NoError(t, changeNetworkTo(h, fastNetworkChange, &dropIn))

	assert.Contains(t, h.config, "flannel-backend: wireguard-native\n")
	assert.Equal(t, "10.0.0.3", h.internalIP)
}

func TestChangeServerNetworkRestoresWhenTheNodeKeepsItsOldAddress(t *testing.T) {
	h := newNetHost()
	h.config, h.backend, h.links, h.internalIP, h.keepsNodeIP = vxlanConfig+"flannel-backend: wireguard-native\n", "wireguard", map[string]bool{"flannel-wg": true}, "203.0.113.10", true
	dropIn := privateServerDropIn

	err := changeNetworkTo(h, fastNetworkChange, &dropIn)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not report 10.0.0.3")
	assert.Contains(t, err.Error(), "previous configuration is back in place")
	assert.False(t, h.dropInPresent, "the server had no drop-in before")
	assert.Empty(t, h.record)
}

func TestChangeServerNetworkDoesNothingWhenTheDropInMatches(t *testing.T) {
	h := newNetHost()
	h.config, h.backend, h.links = vxlanConfig+"flannel-backend: wireguard-native\n", "wireguard", map[string]bool{"flannel-wg": true}
	h.dropIn, h.dropInPresent, h.internalIP = privateServerDropIn, true, "10.0.0.3"
	dropIn := privateServerDropIn

	require.NoError(t, changeNetworkTo(h, fastNetworkChange, &dropIn))

	assert.Equal(t, -1, h.index("systemctl stop k3s"))
}

func TestChangeServerNetworkRefusesAMatchingDropInThatK3sDoesNotRun(t *testing.T) {
	h := newNetHost()
	h.config, h.backend, h.links = vxlanConfig+"flannel-backend: wireguard-native\n", "wireguard", map[string]bool{"flannel-wg": true}
	h.dropIn, h.dropInPresent, h.internalIP = privateServerDropIn, true, "203.0.113.10"
	dropIn := privateServerDropIn

	err := changeNetworkTo(h, fastNetworkChange, &dropIn)

	assert.ErrorContains(t, err, "k3s runs with another address")
	assert.Equal(t, -1, h.index("systemctl stop k3s"))
}

func TestRecoveryRestoresAnInterruptedAddressMoveTheNodeDidNotTake(t *testing.T) {
	h := newNetHost()
	h.config, h.backend, h.links, h.internalIP = vxlanConfig+"flannel-backend: wireguard-native\n", "wireguard", map[string]bool{"flannel-wg": true}, "203.0.113.10"
	h.record = "from=wireguard-native\nnode-ip=10.0.0.3\n"
	h.savedReady, h.snapshotSaved = "Deployment/gate/web\n", true
	h.backupConfig, h.backupDropInAbsent = h.config, true
	h.dropIn, h.dropInPresent, h.keepsNodeIP = privateServerDropIn, true, true

	require.NoError(t, finishInterrupted(h, fastNetworkChange))

	assert.False(t, h.dropInPresent, "the backup without a drop-in is restored")
	assert.Empty(t, h.record)
	assert.GreaterOrEqual(t, h.index("systemctl stop k3s"), 0, "the restore ran")
}

func TestRecoveryFinishesAnInterruptedAddressMoveTheNodeTook(t *testing.T) {
	h := newNetHost()
	h.config, h.backend, h.links, h.internalIP = vxlanConfig+"flannel-backend: wireguard-native\n", "wireguard", map[string]bool{"flannel-wg": true}, "10.0.0.3"
	h.record = "from=wireguard-native\nnode-ip=10.0.0.3\n"
	h.savedReady, h.snapshotSaved = "Deployment/gate/web\n", true
	h.dropIn, h.dropInPresent = privateServerDropIn, true

	require.NoError(t, finishInterrupted(h, fastNetworkChange))

	assert.Empty(t, h.record)
	assert.Equal(t, -1, h.index("systemctl stop k3s"))
	assert.Equal(t, privateServerDropIn, h.dropIn)
}

func TestChangeServerNetworkToleratesATransientAddressReadOnAMatchingDropIn(t *testing.T) {
	h := newNetHost()
	h.config, h.backend, h.links = vxlanConfig+"flannel-backend: wireguard-native\n", "wireguard", map[string]bool{"flannel-wg": true}
	h.dropIn, h.dropInPresent, h.internalIP = privateServerDropIn, true, "10.0.0.3"
	h.addressReadFailures = 1
	dropIn := privateServerDropIn

	require.NoError(t, changeNetworkTo(h, fastNetworkChange, &dropIn))

	assert.Equal(t, -1, h.index("systemctl stop k3s"))
}

func TestRecoveryToleratesATransientAddressReadBeforeFinishing(t *testing.T) {
	h := newNetHost()
	h.config, h.backend, h.links, h.internalIP = vxlanConfig+"flannel-backend: wireguard-native\n", "wireguard", map[string]bool{"flannel-wg": true}, "10.0.0.3"
	h.record = "from=wireguard-native\nnode-ip=10.0.0.3\n"
	h.savedReady, h.snapshotSaved = "Deployment/gate/web\n", true
	h.dropIn, h.dropInPresent = privateServerDropIn, true
	h.addressReadFailures = 1

	require.NoError(t, finishInterrupted(h, fastNetworkChange))

	assert.Empty(t, h.record)
	assert.Equal(t, -1, h.index("systemctl stop k3s"), "a healthy WireGuard server is not rolled back over one failed read")
}

func TestUpgradeNetworkLeavesAKernelWithoutWireGuardOnItsBackend(t *testing.T) {
	h := newNetHost()
	h.wgLoads = false

	warning, err := upgradeNetwork(h, fastNetworkChange)

	require.NoError(t, err, "the rest of the upgrade must still run")
	assert.Contains(t, warning, "wireguard")
	assert.NotContains(t, warning, "upgrade", "kip install re-runs print the same warning")
	assert.Equal(t, -1, h.index("systemctl stop k3s"))
	assert.Equal(t, vxlanConfig, h.config)
}

func TestUpgradeNetworkStillFailsOnOtherErrors(t *testing.T) {
	h := newNetHost()
	h.wgFails = true

	warning, err := upgradeNetwork(h, fastNetworkChange)

	require.Error(t, err)
	assert.Empty(t, warning)
}

// installNetHost extends netHost with host preparation and k3s config capture.
type installNetHost struct {
	*netHost
	written string
}

func (h *installNetHost) Run(cmd string) (string, error) {
	return h.RunStdin(cmd, strings.NewReader(""))
}

func (h *installNetHost) RunStdin(cmd string, in io.Reader) (string, error) {
	switch {
	case cmd == "cat /etc/os-release":
		return "ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"26.04\"\n", nil
	case cmd == k3sVersionProbeCmd:
		return "k3s version " + k3sVersion + " (abc)\n", nil
	case cmd == "uname -r":
		return "6.8.0-test\n", nil
	case strings.HasPrefix(cmd, "for m in "):
		if !h.wgLoads {
			return "MISSING:wireguard\n", nil
		}
		return "", nil
	case strings.HasPrefix(cmd, "cat > "+k3sConfigPath+" << 'KIPEOF'"):
		h.written = cmd
		return "", nil
	}
	return h.netHost.RunStdin(cmd, in)
}

func TestInstallK3sKeepsVXLANOnAnExistingServerWithoutWireGuard(t *testing.T) {
	h := &installNetHost{netHost: newNetHost()}
	h.wgLoads = false

	require.NoError(t, InstallK3s(h, "203.0.113.10", nil, true, true), "re-running install can upgrade k3s while keeping VXLAN")

	assert.Equal(t, -1, h.index("systemctl stop k3s"))
	assert.Contains(t, h.written, "flannel-backend: vxlan\n")
	assert.NotContains(t, h.written, wireguardBackend)
}

func TestInstallK3sStillRequiresWireGuardOnAFreshServer(t *testing.T) {
	h := &installNetHost{netHost: newNetHost()}
	h.wgLoads = false

	err := InstallK3s(h, "203.0.113.10", nil, false, false)

	require.ErrorContains(t, err, "wireguard")
	assert.Empty(t, h.written)
}

func TestInstallK3sStillRequiresWireGuardOnAnExistingWireGuardServer(t *testing.T) {
	h := &installNetHost{netHost: newNetHost()}
	h.backend, h.wgLoads = "wireguard", false

	err := InstallK3s(h, "203.0.113.10", nil, true, true)

	require.ErrorContains(t, err, "wireguard")
	assert.Empty(t, h.written)
}
