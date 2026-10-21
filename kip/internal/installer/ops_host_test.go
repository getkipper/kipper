package installer

import (
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// opsScene is a node whose kernel table, interface and WireGuard config live
// in files under a temporary root, so ops.sh runs unchanged in a local shell.
type opsScene struct {
	t         *testing.T
	root, bin string
}

const fakeOpsNft = `echo "nft $*" >> ROOT/calls.log
check=; file=
while [ $# -gt 0 ]; do case "$1" in -c) check=1 ;; -f) file=$2; shift ;; *) break ;; esac; shift; done
if [ -n "$file" ]; then
  if [ "$file" = - ]; then body=$(cat); else body=$(cat "$file") || exit 1; fi
  [ -e ROOT/nft-fails ] && { echo "Error: syntax error" >&2; exit 1; }
  [ -n "$check" ] && exit 0
  printf '%s\n' "$body" > ROOT/ops-kernel
  exit 0
fi
case "$1 $2" in
  "list table") cat ROOT/ops-kernel 2>/dev/null || { echo "Error: No such file or directory" >&2; exit 1; } ;;
  "delete table") rm -f ROOT/ops-kernel ;;
esac
exit 0`

const fakeIP = `echo "ip $*" >> ROOT/calls.log
case "$1 $2" in
  "link show") [ -e ROOT/link ] ;;
  "link add") touch ROOT/link ;;
  "link del") [ ! -e ROOT/link-del-fails ] && rm -f ROOT/link ;;
  "route replace") [ ! -e ROOT/route-fails ] ;;
esac`

const fakeWg = `case "$1" in
  syncconf) echo "wg syncconf $2 $3" >> ROOT/calls.log; cat > ROOT/wg-config ;;
  genkey) echo "wg genkey" >> ROOT/calls.log; echo 'QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTIzNDU=' ;;
  pubkey) echo "wg pubkey" >> ROOT/calls.log; cat >/dev/null; echo 'cHViY3B1YmNwdWJjcHViY3B1YmNwdWJjcHViY3B1YmM=' ;;
  *) echo "wg $*" >> ROOT/calls.log ;;
esac`

func newOpsScene(t *testing.T) *opsScene {
	t.Helper()
	root, bin := t.TempDir(), t.TempDir()
	for _, d := range []string{"/etc/kipper/ops", "/etc/systemd/system", "/sys/module/wireguard", "/run"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o750))
	}
	stubs := map[string]string{
		"nft":       fakeOpsNft,
		"ip":        fakeIP,
		"wg":        fakeWg,
		"systemctl": fakeSystemctl,
		"flock":     fakeFlock,
		"modprobe":  `exit 0`,
		"sha256sum": `echo "$(cksum | cut -d' ' -f1)-fingerprint  -"`,
		"timeout":   `shift 3; exec "$@"`,
		"sync":      `echo "sync $*" >> ROOT/calls.log; [ ! -e ROOT/sync-fails ]`,
		"systemd-run": `echo "systemd-run $*" >> ROOT/calls.log
while [ $# -gt 0 ]; do case "$1" in --*) shift ;; *) break ;; esac; done
exec "$@"`,
	}
	for name, body := range stubs {
		body = strings.ReplaceAll(strings.ReplaceAll(body, "ROOT", root), "LOG", filepath.Join(root, "calls.log"))
		//nolint:gosec // the stand-ins have to be executable
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	}
	s := &opsScene{t: t, root: root, bin: bin}
	s.write(opsDir+"/ops.sh", opsScript(root))
	return s
}

// shell reports a failure with its output, as the SSH client does.
func (s *opsScene) shell(command string, stdin io.Reader) (string, error) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = []string{"PATH=" + s.bin + ":/usr/bin:/bin"}
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (s *opsScene) Run(command string) (string, error) {
	if command == "cat /etc/os-release" {
		return "ID=ubuntu\nVERSION_ID=\"24.04\"\n", nil
	}
	return s.shell(command, nil)
}

func (s *opsScene) RunStdin(command string, stdin io.Reader) (string, error) {
	return s.shell(command, stdin)
}

func (s *opsScene) write(name, body string) {
	s.t.Helper()
	require.NoError(s.t, os.WriteFile(filepath.Join(s.root, name), []byte(body), 0o600))
}

func (s *opsScene) file(name string) string {
	b, _ := os.ReadFile(filepath.Join(s.root, name))
	return string(b)
}

func (s *opsScene) exists(name string) bool {
	_, err := os.Stat(filepath.Join(s.root, name))
	return err == nil
}

func (s *opsScene) calls() string { return s.file("calls.log") }

func (s *opsScene) script(args string) (string, error) {
	return s.shell("sh "+filepath.Join(s.root, opsDir, "ops.sh")+" "+args, nil)
}

func sampleOpsIntent() OpsNodeIntent {
	return OpsNodeIntent{Address: netip.MustParseAddr("10.99.0.1"), Devices: netip.MustParsePrefix("10.99.0.128/25"), SSHPorts: []int{22, 2222}, Port: 443}
}

func sampleDevices() []AccessDevice {
	return []AccessDevice{
		{Name: "desktop", PublicKey: testKey(10), Address: "10.99.0.130", State: DevicePending},
		{Name: "laptop", PublicKey: testKey(9), Address: "10.99.0.129", State: DeviceActive},
	}
}

// prepared writes everything a load needs, as ApplyOpsNode would.
func (s *opsScene) prepared(devices []AccessDevice) {
	s.t.Helper()
	table, err := RenderOpsTable(sampleOpsIntent())
	require.NoError(s.t, err)
	peers, err := RenderOpsPeers(9, 443, devices)
	require.NoError(s.t, err)
	s.write(opsDir+"/ops.nft", table)
	s.write(opsDir+"/iface", renderOpsInterface(sampleOpsIntent()))
	s.write(opsDir+"/peers.conf", peers)
	s.write(opsDir+"/kipops.key", "c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0MDA=\n")
}

func TestRenderOpsTableRestrictsTheInterfaceToSSHAndTheAPI(t *testing.T) {
	table, err := RenderOpsTable(sampleOpsIntent())
	require.NoError(t, err)
	for _, want := range []string{
		"table inet kipper_ops\ndelete table inet kipper_ops\ntable inet kipper_ops {",
		"type filter hook input priority filter - 20; policy accept;",
		`iifname "kipops" meta nfproto != ipv4 drop`,
		`iifname "kipops" ip daddr != 10.99.0.1 drop`,
		`iifname "kipops" ip saddr != 10.99.0.128/25 drop`,
		`iifname "kipops" tcp dport { 22, 2222, 6443 } return`,
		`iifname "kipops" icmp type { destination-unreachable, echo-request } return`,
		"type filter hook forward priority filter - 20; policy accept;",
		`oifname "kipops" drop`,
		"type filter hook prerouting priority raw; policy accept;",
		"fib daddr type local udp dport 443 notrack",
	} {
		assert.Contains(t, table, want)
	}
	assert.True(t, strings.Index(table, `tcp dport { 22, 2222, 6443 } return`) < strings.LastIndex(table, `iifname "kipops" drop`), "the final drop comes after the allowed ports")
}

func TestRenderOpsTableRefusesABadIntent(t *testing.T) {
	for name, mutate := range map[string]func(*OpsNodeIntent){
		"no ssh ports":   func(in *OpsNodeIntent) { in.SSHPorts = nil },
		"bad port":       func(in *OpsNodeIntent) { in.Port = 70000 },
		"ipv6 address":   func(in *OpsNodeIntent) { in.Address = netip.MustParseAddr("fd00::1") },
		"wrong prefix":   func(in *OpsNodeIntent) { in.Devices = netip.MustParsePrefix("10.99.0.0/24") },
		"bad ssh port":   func(in *OpsNodeIntent) { in.SSHPorts = []int{0} },
		"missing addr":   func(in *OpsNodeIntent) { in.Address = netip.Addr{} },
		"missing prefix": func(in *OpsNodeIntent) { in.Devices = netip.Prefix{} },
	} {
		in := sampleOpsIntent()
		mutate(&in)
		_, err := RenderOpsTable(in)
		assert.Error(t, err, name)
	}
}

func TestRenderOpsPeersNamesEachDeviceAndHoldsNoPrivateKey(t *testing.T) {
	peers, err := RenderOpsPeers(9, 443, sampleDevices())
	require.NoError(t, err)
	assert.Equal(t, "# kipper-ops generation 9\n[Interface]\nListenPort = 443\n"+
		"\n# device: laptop\n[Peer]\nPublicKey = "+testKey(9)+"\nAllowedIPs = 10.99.0.129/32\n"+
		"\n# device: desktop\n[Peer]\nPublicKey = "+testKey(10)+"\nAllowedIPs = 10.99.0.130/32\n", peers)
	assert.NotContains(t, peers, "PrivateKey")

	bad := sampleDevices()
	bad[0].PublicKey = "x"
	_, err = RenderOpsPeers(9, 443, bad)
	assert.Error(t, err)
}

func TestOpsLoadLoadsTheTableBeforeTheInterfaceAndInsertsTheKey(t *testing.T) {
	s := newOpsScene(t)
	s.prepared(sampleDevices())
	out, err := s.script("load")
	require.NoError(t, err, out)
	calls := s.calls()
	assert.Less(t, strings.Index(calls, "nft -f"), strings.Index(calls, "ip link add kipops type wireguard"), calls)
	assert.Less(t, strings.Index(calls, "wg syncconf kipops /dev/stdin"), strings.Index(calls, "ip link set kipops mtu 1380 up"), calls)
	assert.Contains(t, calls, "ip address replace 10.99.0.1/32 dev kipops")
	assert.Contains(t, calls, "ip route replace 10.99.0.128/25 dev kipops")
	cfg := s.file("wg-config")
	assert.True(t, strings.HasPrefix(cfg, "# kipper-ops generation 9\n[Interface]\nPrivateKey = c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0MDA=\nListenPort = 443\n"), cfg)
	assert.Contains(t, cfg, "PublicKey = "+testKey(9))
	assert.Contains(t, cfg, "PublicKey = "+testKey(10))
	assert.NotContains(t, calls, "c2VjcmV0", "the private key never appears in a command line")
}

func TestOpsStateNoticesATableChangedAfterTheLoad(t *testing.T) {
	s := newOpsScene(t)
	s.prepared(sampleDevices())
	out, err := s.script("load")
	require.NoError(t, err, out)

	out, err = s.Run(opsStateCommand(s.root))
	require.NoError(t, err, out)
	st := parseOpsState(out)
	require.NotEmpty(t, st.TableSum)
	assert.Equal(t, st.TableSum, st.LoadedSum)

	kernel := strings.Replace(s.file("ops-kernel"), `iifname "kipops" drop`, `iifname "kipops" return`, 1)
	s.write("ops-kernel", kernel)
	out, err = s.Run(opsStateCommand(s.root))
	require.NoError(t, err, out)
	st = parseOpsState(out)
	assert.NotEqual(t, st.TableSum, st.LoadedSum)
}

func TestOpsStateReadsWhatTheBootUnitNeeds(t *testing.T) {
	s := newOpsScene(t)
	s.prepared(sampleDevices())
	s.write("enabled-"+opsUnitName, "")
	out, err := s.Run(opsStateCommand(s.root))
	require.NoError(t, err, out)
	st := parseOpsState(out)
	assert.True(t, st.Enabled)
	assert.Empty(t, st.Missing)

	require.NoError(t, os.Remove(filepath.Join(s.root, "enabled-"+opsUnitName)))
	require.NoError(t, os.Remove(filepath.Join(s.root, opsDir, "peers.conf")))
	out, err = s.Run(opsStateCommand(s.root))
	require.NoError(t, err, out)
	st = parseOpsState(out)
	assert.False(t, st.Enabled)
	assert.Equal(t, []string{"peers.conf"}, st.Missing)
}

func TestOpsLoadWithoutTheTableNeverBringsUpTheInterface(t *testing.T) {
	s := newOpsScene(t)
	s.prepared(sampleDevices())
	s.write("nft-fails", "")
	_, err := s.script("load")
	require.Error(t, err)
	assert.NotContains(t, s.calls(), "ip link add")
	assert.NotContains(t, s.calls(), "wg syncconf")
}

func TestOpsLoadFailsWhenTheDeviceRouteCannotBeSet(t *testing.T) {
	s := newOpsScene(t)
	s.prepared(sampleDevices())
	s.write("route-fails", "")
	_, err := s.script("load")
	require.Error(t, err, "the boot unit reports a node that cannot answer its devices")
}

func TestOpsLoadNeverHandsAnIncompleteConfigToWireGuard(t *testing.T) {
	for name, setup := range map[string]func(*opsScene){
		"empty key":     func(s *opsScene) { s.write(opsDir+"/kipops.key", "") },
		"no interface":  func(s *opsScene) { s.write(opsDir+"/peers.conf", "# kipper-ops generation 9\n") },
		"no peers file": func(s *opsScene) { require.NoError(t, os.Remove(filepath.Join(s.root, opsDir, "peers.conf"))) },
		"unreadable revoked list": func(s *opsScene) {
			s.write(opsDir+"/revoked-keys", testKey(9)+"\n")
			require.NoError(t, os.Chmod(filepath.Join(s.root, opsDir, "revoked-keys"), 0o000))
		},
	} {
		s := newOpsScene(t)
		s.prepared(sampleDevices())
		setup(s)
		out, err := s.script("load")
		assert.Error(t, err, name)
		assert.NotContains(t, s.calls(), "wg syncconf", "%s: %s", name, out)
	}
}

func TestOpsRevokeDeviceIsPersistentAndLiveRemoved(t *testing.T) {
	s := newOpsScene(t)
	s.prepared(sampleDevices())
	_, err := s.script("load")
	require.NoError(t, err)

	out, err := s.script("revoke-device laptop")
	require.NoError(t, err, out)
	assert.Equal(t, testKey(9)+"\n", s.file(opsDir+"/revoked-keys"))
	assert.Contains(t, s.calls(), "wg set kipops peer "+testKey(9)+" remove")
	synced := "sync " + filepath.Join(s.root, opsDir, "revoked-keys")
	require.Contains(t, s.calls(), synced)
	assert.Less(t, strings.Index(s.calls(), synced), strings.Index(s.calls(), "wg set kipops"), "the list is durable before the live peer goes")

	_, err = s.script("revoke-device laptop")
	require.NoError(t, err, "rerunning is harmless")
	assert.Equal(t, testKey(9)+"\n", s.file(opsDir+"/revoked-keys"), "and adds no second line")

	_, err = s.script("load")
	require.NoError(t, err)
	cfg := s.file("wg-config")
	assert.NotContains(t, cfg, testKey(9), "a reload never brings a revoked device back")
	assert.Contains(t, cfg, testKey(10))
}

func TestOpsRevokeDeviceRetriesTheSyncAfterAFailure(t *testing.T) {
	s := newOpsScene(t)
	s.prepared(sampleDevices())
	_, err := s.script("load")
	require.NoError(t, err)

	s.write("sync-fails", "")
	_, err = s.script("revoke-device laptop")
	require.Error(t, err, "a veto that may not be on disk is not reported as done")
	assert.NotContains(t, s.calls(), "wg set", "the live peer stays until the veto is durable")

	require.NoError(t, os.Remove(filepath.Join(s.root, "sync-fails")))
	s.write("calls.log", "")
	_, err = s.script("revoke-device laptop")
	require.NoError(t, err)
	synced := "sync " + filepath.Join(s.root, opsDir, "revoked-keys")
	require.Contains(t, s.calls(), synced, "the retry syncs the line the first run appended")
	assert.Less(t, strings.Index(s.calls(), synced), strings.Index(s.calls(), "wg set kipops"))
	assert.Equal(t, testKey(9)+"\n", s.file(opsDir+"/revoked-keys"))
}

func TestOpsRevokeDeviceByKeyAndUnknownNames(t *testing.T) {
	s := newOpsScene(t)
	s.prepared(sampleDevices())
	_, err := s.script("revoke-device " + testKey(10))
	require.NoError(t, err)
	assert.Equal(t, testKey(10)+"\n", s.file(opsDir+"/revoked-keys"))
	assert.NotContains(t, s.calls(), "wg set", "no interface, nothing live to remove")

	_, err = s.script("revoke-device " + testKey(3))
	require.NoError(t, err, "a key the node does not list is still recorded")
	out, err := s.script("revoke-device phone")
	require.Error(t, err)
	assert.Contains(t, out, "no device called phone")
}

func TestOpsRemoveTakesTheInterfaceDownBeforeTheTable(t *testing.T) {
	s := newOpsScene(t)
	s.prepared(sampleDevices())
	_, err := s.script("load")
	require.NoError(t, err)
	writeUFWStub(t, s.bin, `echo "ufw $*" >> `+filepath.Join(s.root, "calls.log"))
	require.NoError(t, removeOpsNode(s, s.root))
	calls := s.calls()
	assert.NotContains(t, calls, "systemd-run", "reset waits for the removal")
	assert.Less(t, strings.Index(calls, "ip link del kipops"), strings.Index(calls, "nft delete table inet kipper_ops"), calls)
	assert.Contains(t, calls, "ufw delete allow 443/udp")
	assert.False(t, s.exists(opsDir), "the ops directory is gone")
	assert.False(t, s.exists("ops-kernel"))
	assert.False(t, s.exists("run/kipper-ops-remove.sh"), "the script removes itself")

	s.write("calls.log", "")
	require.NoError(t, removeOpsNode(s, s.root), "a node without ops state is left alone")
	assert.NotContains(t, s.calls(), "systemd-run")
}

func TestOpsNodeKeyAndApplyBringUpTheInterface(t *testing.T) {
	s := newOpsScene(t)
	require.NoError(t, os.WriteFile(filepath.Join(s.bin, "wg-present"), nil, 0o600))
	key, err := opsNodeKey(s, io.Discard, s.root)
	require.NoError(t, err)
	assert.Equal(t, "cHViY3B1YmNwdWJjcHViY3B1YmNwdWJjcHViY3B1YmM=", key)
	assert.Contains(t, s.calls(), "wg genkey")

	_, err = opsNodeKey(s, io.Discard, s.root)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(s.calls(), "wg genkey"), "the key is created once")

	peers, err := RenderOpsPeers(9, 443, sampleDevices())
	require.NoError(t, err)
	require.NoError(t, applyOpsNode(s, sampleOpsIntent(), peers, s.root))
	assert.Equal(t, peers, s.file(opsDir+"/peers.conf"))
	assert.True(t, s.exists("enabled-"+opsUnitName))
	assert.True(t, s.exists("link"))
	assert.Contains(t, s.file("ops-kernel"), "table inet kipper_ops")
}

func TestOpsNodeKeyRetriesTheSyncAfterAFailure(t *testing.T) {
	s := newOpsScene(t)
	require.NoError(t, os.WriteFile(filepath.Join(s.bin, "wg-present"), nil, 0o600))
	s.write("sync-fails", "")
	_, err := opsNodeKey(s, io.Discard, s.root)
	require.Error(t, err, "a key that may not be on disk is not handed out")

	require.NoError(t, os.Remove(filepath.Join(s.root, "sync-fails")))
	s.write("calls.log", "")
	key, err := opsNodeKey(s, io.Discard, s.root)
	require.NoError(t, err)
	assert.NotEmpty(t, key)
	assert.NotContains(t, s.calls(), "wg genkey", "the retry keeps the key the first run created")
	synced := "sync " + filepath.Join(s.root, opsDir, "kipops.key")
	require.Contains(t, s.calls(), synced, "and syncs it before reporting its public key")
	assert.Less(t, strings.Index(s.calls(), synced), strings.Index(s.calls(), "wg pubkey"))
}

func TestOpsNodeKeyRefusesAKernelWithoutWireGuard(t *testing.T) {
	s := newOpsScene(t)
	require.NoError(t, os.RemoveAll(filepath.Join(s.root, "sys", "module", "wireguard")))
	_, err := opsNodeKey(s, io.Discard, s.root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no WireGuard module")
}

func TestApplyOpsNodeLoadsNothingWhenPreparationFails(t *testing.T) {
	s := newOpsScene(t)
	s.prepared(sampleDevices())
	s.write("sync-fails", "")
	peers, err := RenderOpsPeers(10, 443, sampleDevices()[:1])
	require.NoError(t, err)
	err = applyOpsNode(s, sampleOpsIntent(), peers, s.root)
	require.Error(t, err, "an unsynced peers file is a failed apply")
	assert.NotContains(t, s.calls(), "wg syncconf", "the old files are never loaded in its place")
}

func TestApplyOpsNodeRefusesATableNftRejects(t *testing.T) {
	s := newOpsScene(t)
	s.write("nft-fails", "")
	err := applyOpsNode(s, sampleOpsIntent(), "x", s.root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing changed")
	assert.False(t, s.exists(opsDir+"/ops.nft"))
}

func TestRenderAccessTableAdmitsDevicesOnlyWhenAsked(t *testing.T) {
	in := AccessIntent{Generation: 3, Admin: prefixes(t, "198.51.100.7/32"), Ports: []int{22}, Cluster4: prefixes(t, "10.42.0.0/16")}
	plain, err := RenderAccessTable(in)
	require.NoError(t, err)
	assert.NotContains(t, plain, "kipops")
	assert.Equal(t, accessChainRules, expectedAccessRules(plain))

	in.Devices = true
	table, err := RenderAccessTable(in)
	require.NoError(t, err)
	assert.Contains(t, table, `iifname "kipops" tcp dport { 22, 6443 } accept`)
	assert.Less(t, strings.Index(table, `iifname "kipops"`), strings.Index(table, "counter drop"), "before the final drop")
	assert.Equal(t, accessChainRules+1, expectedAccessRules(table))

	in.Admin = nil
	_, err = RenderAccessTable(in)
	assert.NoError(t, err, "an empty list is valid when devices are the way in")
	in.Devices = false
	_, err = RenderAccessTable(in)
	assert.Error(t, err, "and refused without them")
}

func TestAccessDriftExpectsTheDeviceRuleFromTheCommittedTable(t *testing.T) {
	in := AccessIntent{Generation: 3, Admin: prefixes(t, "198.51.100.7/32"), Ports: []int{22}, Cluster4: prefixes(t, "10.42.0.0/16"), Devices: true}
	table, err := RenderAccessTable(in)
	require.NoError(t, err)
	operators, err := RenderAccessOperators(in)
	require.NoError(t, err)
	st := AccessHostState{Table: table, Operators: operators, Live: true, UnitEnabled: "enabled"}
	live := AccessLive{
		Sets:  map[string][]netip.Prefix{"admin4": prefixes(t, "198.51.100.7/32"), "cluster4": prefixes(t, "10.42.0.0/16")},
		Chain: accessChainShape{Hook: "input", Priority: -10, Policy: "accept", Rules: accessChainRules + 1},
	}
	assert.Empty(t, AccessDrift(st, live))
	live.Chain.Rules = accessChainRules
	assert.NotEmpty(t, AccessDrift(st, live), "a live chain missing the device rule is drift")
}

func TestAccessHeaderCountsThePeersGeneration(t *testing.T) {
	s := newAccessScene(t)
	s.commit("203.0.113.0/24")
	require.NoError(t, os.MkdirAll(filepath.Join(s.root, opsDir), 0o700))
	s.write(opsDir+"/peers.conf", "# kipper-ops generation 12\n[Interface]\nListenPort = 443\n")
	st, err := parseAccessState(mustShell(t, s, accessStateCommand(s.root)))
	require.NoError(t, err)
	assert.Equal(t, 12, st.PeersGeneration)
	h := st.Header("server")
	assert.Equal(t, 12, h.Generation, "a device removal newer than the tables counts")
	assert.Error(t, StaleAccessRecord(11, []AccessNodeHeader{h}), "so restoring an older record that still lists the device is refused")
}

func TestALeftoverOperatorInterfaceIsNotCalledARestriction(t *testing.T) {
	s := newAccessScene(t)
	require.NoError(t, os.MkdirAll(filepath.Join(s.root, opsDir), 0o700))
	s.write(opsDir+"/peers.conf", "# kipper-ops generation 12\n[Interface]\nListenPort = 443\n")
	st, err := parseAccessState(mustShell(t, s, accessStateCommand(s.root)))
	require.NoError(t, err)

	err = OrphanedAccessTable([]AccessNodeHeader{st.Header("server")})

	require.Error(t, err, "reset still has an interface to remove")
	assert.NotContains(t, err.Error(), "restricts SSH")
	assert.Contains(t, err.Error(), "operator interface")
	assert.Contains(t, err.Error(), "kip cluster access reset")
}

func TestOpsNodeKeyRefusesANodeRunningFirewalld(t *testing.T) {
	s := newOpsScene(t)
	s.write("active-firewalld", "")
	_, err := opsNodeKey(s, io.Discard, s.root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "firewalld")
	assert.NotContains(t, s.calls(), "wg genkey", "nothing is set up on a node the tunnel cannot reach")
}

func TestApplyOpsNodeOpensTheTunnelPortInUFW(t *testing.T) {
	s := newOpsScene(t)
	s.prepared(sampleDevices())
	writeUFWStub(t, s.bin, `echo "ufw $*" >> `+filepath.Join(s.root, "calls.log")+`; [ "$1" = status ] && echo "Status: active"; exit 0`)
	peers, err := RenderOpsPeers(9, 443, sampleDevices())
	require.NoError(t, err)
	require.NoError(t, applyOpsNode(s, sampleOpsIntent(), peers, s.root))
	allow := "ufw allow 443/udp comment kipper-ops"
	require.Contains(t, s.calls(), allow)
	assert.Less(t, strings.Index(s.calls(), allow), strings.Index(s.calls(), "wg syncconf"), "the port opens before the interface comes up")
}

func TestApplyOpsNodeSavesTheTunnelPortInAnInactiveUFW(t *testing.T) {
	s := newOpsScene(t)
	s.prepared(sampleDevices())
	writeUFWStub(t, s.bin, `echo "ufw $*" >> `+filepath.Join(s.root, "calls.log")+`; [ "$1" = status ] && echo "Status: inactive"; exit 0`)
	peers, err := RenderOpsPeers(9, 443, sampleDevices())
	require.NoError(t, err)
	require.NoError(t, applyOpsNode(s, sampleOpsIntent(), peers, s.root))
	assert.Contains(t, s.calls(), "ufw allow 443/udp comment kipper-ops", "enabling UFW later keeps the port")
	assert.NotContains(t, s.calls(), "ufw enable")
}

func TestOpsUFWRulesFollowThePeersFile(t *testing.T) {
	s := newOpsScene(t)
	rules, err := opsUFWRules(opsRootRunner{s})
	require.NoError(t, err)
	assert.Empty(t, rules, "a host without an operator interface gets no rule")

	peers, err := RenderOpsPeers(9, 51820, sampleDevices())
	require.NoError(t, err)
	s.write(opsDir+"/peers.conf", peers)
	rules, err = opsUFWRules(opsRootRunner{s})
	require.NoError(t, err)
	assert.Equal(t, []string{"ufw allow 51820/udp comment kipper-ops"}, rules)
}

// opsRootRunner runs commands with host paths redirected into the scene.
type opsRootRunner struct{ s *opsScene }

func (r opsRootRunner) Run(command string) (string, error) {
	return r.s.Run(strings.ReplaceAll(command, opsDir, r.s.root+opsDir))
}

func (r opsRootRunner) RunStdin(command string, stdin io.Reader) (string, error) {
	return r.s.RunStdin(strings.ReplaceAll(command, opsDir, r.s.root+opsDir), stdin)
}

func writeUFWStub(t *testing.T, bin, body string) {
	t.Helper()
	//nolint:gosec // the stand-in has to be executable
	require.NoError(t, os.WriteFile(filepath.Join(bin, "ufw"), []byte("#!/bin/sh\n"+body+"\n"), 0o755))
}

func TestOpsNodeRoutesListsEveryTableExceptDefaultsAndTheTunnel(t *testing.T) {
	out := `default via 172.31.0.1 dev ens5 proto dhcp src 172.31.5.10 metric 100
10.42.0.0/24 dev cni0 proto kernel scope link src 10.42.0.1
10.99.0.128/25 dev kipops scope link
172.31.0.0/20 dev ens5 proto kernel scope link src 172.31.5.10 metric 100
10.0.0.0/8 table 200 proto static
	nexthop via 10.0.0.1 dev eth1 weight 1
local 172.31.5.10 dev ens5 table local proto kernel scope host src 172.31.5.10
broadcast 172.31.15.255 dev ens5 table local proto kernel scope link src 172.31.5.10
unreachable 192.0.2.0/24
`
	got := parseOpsRoutes(out)

	var s []string
	for _, p := range got {
		s = append(s, p.String())
	}
	assert.Equal(t, []string{"10.42.0.0/24", "172.31.0.0/20", "10.0.0.0/8", "172.31.5.10/32", "172.31.15.255/32", "192.0.2.0/24"}, s)
}

func TestOpsRemoveKeepsTheTableWhileTheInterfaceStays(t *testing.T) {
	s := newOpsScene(t)
	s.prepared(sampleDevices())
	_, err := s.script("load")
	require.NoError(t, err)
	s.write("link-del-fails", "")

	require.Error(t, removeOpsNode(s, s.root))

	assert.True(t, s.exists("ops-kernel"), "the isolation table stays while the interface is up")
	assert.True(t, s.exists(opsDir+"/peers.conf"), "a rerun still has what it needs")
}

func TestScheduledOpsRemovalDisablesTheBootUnitAtOnce(t *testing.T) {
	s := newOpsScene(t)
	s.prepared(sampleDevices())
	_, err := s.script("load")
	require.NoError(t, err)
	writeUFWStub(t, s.bin, `echo "ufw $*" >> `+filepath.Join(s.root, "calls.log"))

	require.NoError(t, scheduleOpsRemoval(s, s.root))

	calls := s.calls()
	assert.Contains(t, calls, "systemd-run --quiet --collect --on-active=10", "the removal waits until the session returns")
	require.Contains(t, calls, "systemctl disable "+opsUnitName)
	assert.Less(t, strings.Index(calls, "systemctl disable "+opsUnitName), strings.Index(calls, "systemd-run"), "a reboot before the timer fires cannot load the interface again")
	assert.False(t, s.exists(opsDir), "the stub runs the timer at once")
	assert.Contains(t, calls, "ufw delete allow 443/udp")
}
