package cmd

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installjob"
)

// fakeRemote plays the server: a log the follow command streams in chunks, and
// a connection that can drop partway.
type fakeRemote struct {
	log       string
	dropAfter []int // bytes to send before dropping, per follow call
	phase     string
	follows   []string
	stderr    string
	noEnd     bool // the stream ends cleanly without its end line
}

func (f *fakeRemote) Run(cmd string) (string, error)                   { return "", nil }
func (f *fakeRemote) RunStdin(cmd string, _ io.Reader) (string, error) { return "", nil }
func (f *fakeRemote) RunStreamTo(cmd string, stdout, stderr io.Writer) error {
	f.follows = append(f.follows, cmd)
	var offset int
	_, _ = fmt.Sscanf(cmd[strings.Index(cmd, "--offset ")+len("--offset "):], "%d", &offset)
	_, _ = io.WriteString(stderr, f.stderr)
	rest := f.log[offset:]
	if len(f.dropAfter) > 0 {
		n := f.dropAfter[0]
		f.dropAfter = f.dropAfter[1:]
		_, _ = io.WriteString(stdout, rest[:n])
		return errors.New("client_loop: send disconnect: Broken pipe")
	}
	_, _ = io.WriteString(stdout, rest)
	if f.noEnd {
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "\n%s offset=%d phase=%s\n", installjob.EndMarker, len(f.log), f.phase)
	return nil
}

func TestFollowJobStreamsTheLogAndReturnsThePhase(t *testing.T) {
	r := &fakeRemote{log: "step one\nstep two\n", phase: installjob.PhaseSucceeded}
	var out bytes.Buffer

	phase, err := followJob(r, "kip", "run-1", &out, 3, time.Millisecond)

	require.NoError(t, err)
	assert.Equal(t, installjob.PhaseSucceeded, phase)
	assert.Equal(t, "step one\nstep two\n", out.String(), "the end marker is not shown")
}

func TestFollowJobReattachesFromTheLastLogByte(t *testing.T) {
	r := &fakeRemote{log: "step one\nstep two\nstep three\n", dropAfter: []int{9}, phase: installjob.PhaseFailed,
		stderr: "Timeout, server not responding.\n"}
	var out bytes.Buffer

	phase, err := followJob(r, "kip", "run-1", &out, 3, time.Millisecond)

	require.NoError(t, err)
	assert.Equal(t, installjob.PhaseFailed, phase)
	assert.Equal(t, "step one\nstep two\nstep three\n", out.String(), "nothing lost, nothing repeated")
	require.Len(t, r.follows, 2)
	assert.Contains(t, r.follows[1], "--offset 8", "a trailing newline may open the end line, so it is held back and fetched again")
}

func TestFollowJobGivesUpAfterTheRetries(t *testing.T) {
	r := &fakeRemote{log: "step one\n", dropAfter: []int{0, 0, 0, 0}}

	_, err := followJob(r, "kip", "run-1", io.Discard, 2, time.Millisecond)

	assert.ErrorContains(t, err, "run the same command again to reattach")
}

func TestLastJSONLineSkipsSSHWarnings(t *testing.T) {
	out := "Warning: Permanently added '203.0.113.10' (ED25519) to the list of known hosts.\r\n{\"action\":\"started\",\"run_id\":\"run-1\"}\n"
	assert.Equal(t, `{"action":"started","run_id":"run-1"}`, lastJSONLine(out))
	assert.Empty(t, lastJSONLine("no json here\n"))
}

func TestLastLineSkipsSSHWarnings(t *testing.T) {
	out := "Warning: Permanently added '203.0.113.10' (ED25519) to the list of known hosts.\r\ndiscarded\r\n"
	assert.Equal(t, "discarded", lastLine(out))
	assert.Empty(t, lastLine(""))
}

// runFetch substitutes a curl stub to test checksum verification without network access.
func runFetch(t *testing.T, body, sum, target string) error {
	t.Helper()
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum is not installed")
	}
	bin := t.TempDir()
	curl := "#!/bin/sh\nwhile [ \"$1\" != -o ]; do shift; done\nprintf %s '" + body + "' > \"$2\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0o755)) //nolint:gosec // an executable test stub
	if _, err := exec.LookPath("flock"); err != nil {
		// Without flock (macOS) the lock is a no-op; TestFetchKipCommandHoldsTheKipLock covers it where flock exists.
		require.NoError(t, os.WriteFile(filepath.Join(bin, "flock"), []byte("#!/bin/sh\nexit 0\n"), 0o755)) //nolint:gosec // an executable test stub
	}
	c := exec.Command("bash", "-c", fetchKipCommand("https://example.com/kip-linux-amd64", sum, target)) //nolint:gosec // the command under test
	c.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return c.Run()
}

func TestFetchKipCommandPublishesOnlyAVerifiedBinary(t *testing.T) {
	lib := filepath.Join(t.TempDir(), "lib")
	good := fmt.Sprintf("%x", sha256.Sum256([]byte("new")))
	target := filepath.Join(lib, "kip-"+good)

	require.Error(t, runFetch(t, "tampered", good, target))
	_, err := os.Stat(target)
	assert.True(t, os.IsNotExist(err), "a download that fails its checksum is never published")

	require.NoError(t, runFetch(t, "new", good, target))
	kip, err := os.ReadFile(target) //nolint:gosec // inside the test's temp dir
	require.NoError(t, err)
	assert.Equal(t, "new", string(kip))
	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	left, err := os.ReadDir(lib)
	require.NoError(t, err)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{"kip-" + good, installjob.KipLockName}, names, "no staging file is left behind")

	past := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(target, past, past))
	require.NoError(t, runFetch(t, "tampered", good, target), "a verified kip already in place is used as it is")
	info, err = os.Stat(target)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), info.ModTime(), time.Minute, "reuse refreshes its time, so cleanup keeps it")
	kip, err = os.ReadFile(target) //nolint:gosec // inside the test's temp dir
	require.NoError(t, err)
	assert.Equal(t, "new", string(kip))
}

// A driver verifying, refreshing or publishing a kip holds the lock cleanup
// takes, so cleanup never deletes a binary between its check and its use.
func TestFetchKipCommandHoldsTheKipLock(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock is not installed")
	}
	lib := filepath.Join(t.TempDir(), "lib")
	require.NoError(t, os.MkdirAll(lib, 0o700))
	held := exec.Command("flock", filepath.Join(lib, installjob.KipLockName), "sleep", "1") //nolint:gosec // a test lock in a temp dir
	require.NoError(t, held.Start())
	time.Sleep(200 * time.Millisecond)
	good := fmt.Sprintf("%x", sha256.Sum256([]byte("new")))

	start := time.Now()
	require.NoError(t, runFetch(t, "new", good, filepath.Join(lib, "kip-"+good)))

	assert.GreaterOrEqual(t, time.Since(start), 500*time.Millisecond, "the download waited for the lock")
	_ = held.Wait()
}

func TestFetchKipCommandStagesInAFileOfItsOwn(t *testing.T) {
	cmd := fetchKipCommand("https://example.com/kip-linux-amd64", strings.Repeat("a", 64), "/usr/local/lib/kipper/kip-x")
	assert.Contains(t, cmd, "mktemp")
	assert.Contains(t, cmd, "--max-time")
}

func TestServerKipPathIsNamedAfterItsHash(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	assert.Equal(t, "/usr/local/lib/kipper/kip-"+sum, serverKipFor(sum))
}

// Publishing a binary also replaces a regular file left by an earlier install.
func TestLinkKipCommand(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "kip-abc")
	link := filepath.Join(dir, "kip")
	require.NoError(t, os.WriteFile(target, []byte("new"), 0o755)) //nolint:gosec // stands in for a verified binary
	require.NoError(t, os.WriteFile(link, []byte("old"), 0o755))   //nolint:gosec // stands in for an older install

	require.NoError(t, exec.Command("bash", "-c", linkKipCommand(target, link)).Run()) //nolint:gosec // the command under test

	got, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, target, got)
	left, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, left, 2, "no temporary link is left behind")
}

func TestReleaseTag(t *testing.T) {
	tag, ok := releaseTag("0.29.0")
	assert.True(t, ok)
	assert.Equal(t, "v0.29.0", tag)
	for _, v := range []string{"dev", "", "0.28.2-local", "v0.27.0-3-gabc1234-dirty"} {
		_, ok := releaseTag(v)
		assert.False(t, ok, v)
	}
}

func TestChecksumFor(t *testing.T) {
	a, b, c := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	sums := a + "  kip-darwin-arm64\n" + b + "  kip-linux-amd64\n" + c + "  kip-linux-arm64\n"
	sum, err := checksumFor(sums, "amd64")
	require.NoError(t, err)
	assert.Equal(t, b, sum)
	_, err = checksumFor(sums, "riscv64")
	assert.Error(t, err)
	_, err = checksumFor("not-a-hash  kip-linux-amd64\n", "amd64")
	assert.Error(t, err, "only a sha256 reaches the server's command line")
}

func TestServerArch(t *testing.T) {
	arch, err := serverArch("x86_64\n")
	require.NoError(t, err)
	assert.Equal(t, "amd64", arch)
	arch, err = serverArch("aarch64")
	require.NoError(t, err)
	assert.Equal(t, "arm64", arch)
	_, err = serverArch("x86_64; rm -rf /")
	assert.Error(t, err, "only the known architectures reach a download URL")
}

func shopResult() installjob.Result {
	return installjob.Result{
		RunID: "run-1", Outcome: installjob.PhaseSucceeded, AdminPassword: "pw",
		Cluster: &config.Cluster{
			Name: "shop", Provider: "baremetal", Host: "203.0.113.10", Domain: "shop.example",
			Kubeconfig: "/root/.kip/clusters/shop.yaml", GatewayToken: "tok", MachineID: "0123456789abcdef0123456789abcdef",
			BackupStorage: &config.BackupStorageRef{Mode: "external", Bucket: "b"},
		},
		Kubeconfig: resultKubeconfig,
	}
}

func addLocalCluster(t *testing.T, c config.Cluster) {
	t.Helper()
	require.NoError(t, config.Update(func(cfg *config.Config) error {
		cfg.AddCluster(c)
		return nil
	}))
}

func TestSaveDrivenResult(t *testing.T) {
	home := withFakeHome(t)

	require.NoError(t, saveDrivenResult(shopResult(), "203.0.113.10", `C:\Users\ops\.ssh\id_ed25519`))

	entry := loadShop(t)
	assert.Equal(t, `C:\Users\ops\.ssh\id_ed25519`, entry.SSHKey)
	assert.Equal(t, "tok", entry.GatewayToken)
	assert.Equal(t, "b", entry.BackupStorage.Bucket)
	assert.Equal(t, filepath.Join(home, ".kip", "clusters", "shop.yaml"), entry.Kubeconfig)
	kc, err := os.ReadFile(entry.Kubeconfig) //nolint:gosec // inside the test's temp home
	require.NoError(t, err)
	assert.NotContains(t, string(kc), "/root/.kip/jobs", "the server's binary path never reaches this machine")
	saved, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "shop", saved.CurrentCluster)
}

func TestSaveDrivenResultRefusesAnotherHostsEntry(t *testing.T) {
	withFakeHome(t)
	res := shopResult()
	res.Cluster.Host = "198.51.100.7"

	err := saveDrivenResult(res, "203.0.113.10", "")

	assert.ErrorContains(t, err, "198.51.100.7")
	saved, err := config.Load()
	require.NoError(t, err)
	assert.Nil(t, saved.GetCluster("shop"))
}

func TestSaveDrivenResultLeavesAnotherClusterOfTheSameNameAlone(t *testing.T) {
	home := withFakeHome(t)
	path := stageExistingKubeconfig(t, home, kipRenderedKubeconfig(t, "https://198.51.100.7:6443"))
	addLocalCluster(t, config.Cluster{Name: "shop", Host: "198.51.100.7", Kubeconfig: path})

	err := saveDrivenResult(shopResult(), "203.0.113.10", "")

	assert.ErrorContains(t, err, "kip cluster rename")
	assert.Equal(t, "198.51.100.7", loadShop(t).Host)
	kc, err := os.ReadFile(path) //nolint:gosec // inside the test's temp home
	require.NoError(t, err)
	assert.Contains(t, string(kc), "198.51.100.7")
}

func TestSaveDrivenResultKeepsWhatThisMachineChoseForTheCluster(t *testing.T) {
	withFakeHome(t)
	addLocalCluster(t, config.Cluster{Name: "shop", Host: "203.0.113.10", CurrentProject: "web", CurrentEnvironment: "staging"})

	require.NoError(t, saveDrivenResult(shopResult(), "203.0.113.10", ""))

	entry := loadShop(t)
	assert.Equal(t, "web", entry.CurrentProject)
	assert.Equal(t, "staging", entry.CurrentEnvironment)
}

func TestSaveDrivenResultKeepsAnAdminCertificate(t *testing.T) {
	home := withFakeHome(t)
	path := stageExistingKubeconfig(t, home, adminCertKubeconfig)

	err := saveDrivenResult(shopResult(), "203.0.113.10", "")

	assert.Error(t, err)
	kc, err := os.ReadFile(path) //nolint:gosec // inside the test's temp home
	require.NoError(t, err)
	assert.Equal(t, adminCertKubeconfig, string(kc))
}

func TestSaveDrivenResultRefusesAnotherClustersKubeconfigPath(t *testing.T) {
	home := withFakeHome(t)
	addLocalCluster(t, config.Cluster{Name: "Shop", Host: "198.51.100.7", Kubeconfig: filepath.Join(home, ".kip", "clusters", "Shop.yaml")})

	err := saveDrivenResult(shopResult(), "203.0.113.10", "")

	assert.ErrorContains(t, err, "Shop")
}

func TestParsePreflight(t *testing.T) {
	arch, err := parsePreflight("Warning: Permanently added '203.0.113.10' to the list of known hosts.\nuid:0\narch:x86_64\n")
	require.NoError(t, err)
	assert.Equal(t, "amd64", arch)

	_, err = parsePreflight("uid:1000\narch:x86_64\n")
	assert.ErrorContains(t, err, "as root")
	_, err = parsePreflight("uid:0\narch:x86_64\nmissing:systemd-run\n")
	assert.ErrorContains(t, err, "systemd-run")
	_, err = parsePreflight("uid:0\narch:mips\n")
	assert.Error(t, err)
	_, err = parsePreflight("uid:0\narch:x86_64\nno-systemd\n")
	assert.ErrorContains(t, err, "systemd")
}

func TestUsesDriver(t *testing.T) {
	for _, name := range []string{"via-server", "server-kip", "cancel"} {
		assert.NotNil(t, installCmd.Flags().Lookup(name), "--%s", name)
	}
	t.Cleanup(func() {
		_ = installCmd.Flags().Set("via-server", "false")
		installCmd.Flags().Lookup("via-server").Changed = false
	})
	assert.True(t, usesDriver(installCmd, "windows"), "the default follows the platform")
	assert.False(t, usesDriver(installCmd, "linux"))
	require.NoError(t, installCmd.Flags().Set("via-server", "true"))
	assert.True(t, usesDriver(installCmd, "linux"))
	require.NoError(t, installCmd.Flags().Set("via-server", "false"))
	assert.False(t, usesDriver(installCmd, "windows"))
}

// --local overrides the Windows default when --via-server is unset.
func TestUsesDriverNeverWithLocal(t *testing.T) {
	require.NoError(t, installCmd.Flags().Set("local", "true"))
	t.Cleanup(func() {
		_ = installCmd.Flags().Set("local", "false")
		installCmd.Flags().Lookup("local").Changed = false
	})
	assert.False(t, usesDriver(installCmd, "windows"))
}

func TestFollowJobSaysWhenTheStreamEndsWithoutItsEndLine(t *testing.T) {
	r := &fakeRemote{log: "step one\n", noEnd: true}

	_, err := followJob(r, "kip", "run-1", io.Discard, 1, time.Millisecond)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "without")
	assert.NotContains(t, err.Error(), "<nil>")
}

func TestProbeFailure(t *testing.T) {
	home := withFakeHome(t)
	knownHosts := filepath.Join(home, ".ssh", "known_hosts")

	offer, err := probeFailure("203.0.113.10", "@@@@@@@@\n@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @\nHost key verification failed.\n")
	assert.False(t, offer)
	assert.ErrorContains(t, err, knownHosts, "a changed host key names the file to fix")

	offer, err = probeFailure("203.0.113.10", "root@203.0.113.10: Permission denied (publickey,password).\n")
	assert.True(t, offer, "only a refused key is worth offering to add")
	assert.NoError(t, err)

	offer, err = probeFailure("203.0.113.10", "ssh: connect to host 203.0.113.10 port 22: Connection timed out\n")
	assert.False(t, offer)
	assert.ErrorContains(t, err, "Connection timed out")
}

func writeChunks(w io.Writer, chunks ...string) {
	for _, c := range chunks {
		_, _ = io.WriteString(w, c)
	}
}

func TestEndWriterWaitsForTheWholeEndLine(t *testing.T) {
	end := "\n" + installjob.EndMarker + " offset=9 phase=succeeded\n"
	for _, split := range []int{1, len("\n" + installjob.EndMarker), len(end) - 6, len(end) - 1} {
		var out bytes.Buffer
		w := &endWriter{out: &out}
		writeChunks(w, "step one\n", end[:split], end[split:])
		w.finish()

		assert.True(t, w.done, "split at %d", split)
		assert.Equal(t, installjob.PhaseSucceeded, w.phase, "split at %d", split)
		assert.Equal(t, "step one\n", out.String())
		assert.Equal(t, int64(9), w.written)
	}
}

func TestEndWriterDropsAHalfEndLineOnDisconnect(t *testing.T) {
	var out bytes.Buffer
	w := &endWriter{out: &out}
	writeChunks(w, "step one\n", "\n"+installjob.EndMarker+" offs")
	w.finish()

	assert.False(t, w.done)
	assert.Equal(t, "step one\n", out.String())
	assert.Equal(t, int64(9), w.written)
}

func TestEndWriterTreatsAnInvalidEndLineAsLog(t *testing.T) {
	bogus := "\n" + installjob.EndMarker + " offset=0 phase=bogus\n"
	var out bytes.Buffer
	w := &endWriter{out: &out}
	writeChunks(w, bogus, "more\n", fmt.Sprintf("\n%s offset=%d phase=succeeded\n", installjob.EndMarker, len(bogus)+5))

	assert.True(t, w.done)
	assert.Equal(t, installjob.PhaseSucceeded, w.phase)
	assert.Equal(t, bogus+"more\n", out.String())
}

func TestEndWriterChecksTheEndLineOffset(t *testing.T) {
	var out bytes.Buffer
	w := &endWriter{out: &out, base: 4}
	writeChunks(w, "step\n", "\n"+installjob.EndMarker+" offset=99 phase=succeeded\n", "late\n")
	w.finish()
	assert.False(t, w.done)
	assert.Equal(t, "step\n", out.String(), "the end line neither shows nor counts, and nothing after it does")
	assert.Equal(t, int64(5), w.written)

	w = &endWriter{out: io.Discard, base: 4}
	writeChunks(w, "step\n", "\n"+installjob.EndMarker+" offset=9 phase=succeeded\n")
	assert.True(t, w.done, "base plus what arrived matches")
}

func TestKeyHasPassphrase(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen is not installed")
	}
	dir := t.TempDir()
	plain, locked := filepath.Join(dir, "plain"), filepath.Join(dir, "locked")
	require.NoError(t, exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", plain).Run())               //nolint:gosec // a test key in a temp dir
	require.NoError(t, exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "correct horse", "-f", locked).Run()) //nolint:gosec // a test key in a temp dir

	has, err := keyHasPassphrase(plain)
	require.NoError(t, err)
	assert.False(t, has)
	has, err = keyHasPassphrase(locked)
	require.NoError(t, err)
	assert.True(t, has)
}

func TestKeyCheckFailure(t *testing.T) {
	has, err := keyCheckFailure("/k", `Load key "/k": incorrect passphrase supplied to decrypt private key`+"\n")
	assert.True(t, has)
	assert.NoError(t, err)

	has, err = keyCheckFailure("/k", "Permissions 0644 for '/k' are too open.\nThis private key will be ignored.\nLoad key \"/k\": bad permissions\n")
	assert.False(t, has)
	assert.ErrorContains(t, err, "bad permissions")

	_, err = keyCheckFailure("/k", `Load key "/k": invalid format`+"\n")
	assert.ErrorContains(t, err, "invalid format")
}

func TestPlacementError(t *testing.T) {
	err := placementError("203.0.113.10", "ssh: connect to host 203.0.113.10 port 22: Connection refused\n", errors.New("exit status 255"))
	assert.ErrorContains(t, err, "Connection refused")
	assert.ErrorContains(t, placementError("203.0.113.10", "", errors.New("exit status 255")), "exit status 255")
}

func TestPlacementErrorNamesARefusedPassword(t *testing.T) {
	for _, stderr := range []string{
		"root@203.0.113.10: Permission denied (publickey,password).\n",
		"Permission denied, please try again.\n",
	} {
		err := placementError("203.0.113.10", stderr, errors.New("exit status 255"))
		assert.ErrorContains(t, err, "refused the root password", stderr)
		assert.ErrorContains(t, err, "provider", stderr)
	}
}

func TestPlacementErrorNamesAServerWithoutPasswordLogins(t *testing.T) {
	err := placementError("203.0.113.10", "root@203.0.113.10: Permission denied (publickey).\n", errors.New("exit status 255"))

	assert.ErrorContains(t, err, "accepts no password")
	assert.NotContains(t, err.Error(), "reset it")
}

func TestPlacementErrorKeepsOtherPermissionErrors(t *testing.T) {
	err := placementError("203.0.113.10", "bash: /root/.ssh/authorized_keys: Permission denied\n", errors.New("exit status 1"))

	assert.ErrorContains(t, err, "/root/.ssh/authorized_keys: Permission denied")
	assert.NotContains(t, err.Error(), "root password")
}

func TestAgentHolds(t *testing.T) {
	key := "256 SHA256:abcDEF123 ops@laptop (ED25519)\n"
	agent := "3072 SHA256:zzz other (RSA)\n256 SHA256:abcDEF123 ops@laptop (ED25519)\n"

	assert.True(t, agentHolds(key, agent))
	assert.False(t, agentHolds(key, "3072 SHA256:zzz other (RSA)\n"))
	assert.False(t, agentHolds(key, "The agent has no identities.\n"))
	assert.False(t, agentHolds("", agent))
}

func TestInstallErrorsPrintNoUsage(t *testing.T) {
	assert.True(t, installCmd.SilenceUsage)
}
