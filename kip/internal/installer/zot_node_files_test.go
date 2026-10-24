package installer

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registryHost runs the registry file scripts with the machine's own sh against
// a temporary directory standing in for /etc/rancher/k3s. It answers the k3s
// restart and the node readiness check itself and refuses any other kubectl or
// systemctl command, so a test never reaches a real cluster or service.
// It prepends noise to shell output to simulate SSH warnings or login scripts.
type registryHost struct {
	dir         string
	commands    []string
	restarts    int
	failRestart bool

	noise string
}

func newRegistryHost(t *testing.T) *registryHost {
	return &registryHost{dir: t.TempDir()}
}

func (h *registryHost) Run(command string) (string, error) {
	return h.RunStdin(command, strings.NewReader(""))
}

func (h *registryHost) RunStdin(command string, stdin io.Reader) (string, error) {
	h.commands = append(h.commands, command)
	switch {
	case command == "systemctl restart k3s":
		h.restarts++
		if h.failRestart {
			return "", errors.New("k3s did not start")
		}
		return "", nil
	case command == nodeReadyQuery:
		return "", nil
	case strings.Contains(command, "kubectl") || strings.Contains(command, "systemctl"):
		return "", errors.New("unexpected command: " + command)
	}
	cmd := exec.Command("sh", "-c", strings.ReplaceAll(command, "/etc/rancher/k3s", h.dir))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	return h.noise + string(out), err
}

func (h *registryHost) path(name string) string { return filepath.Join(h.dir, name) }

func (h *registryHost) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(h.path(name))
	require.NoError(t, err)
	return string(b)
}

func (h *registryHost) mode(t *testing.T, name string) os.FileMode {
	t.Helper()
	info, err := os.Stat(h.path(name))
	require.NoError(t, err)
	return info.Mode().Perm()
}

const (
	testRegistryCA = "-----BEGIN CERTIFICATE-----\nregistry-ca\n-----END CERTIFICATE-----\n"
	testZotIP      = "10.43.0.10"
	testPullSecret = "pullsecret"
)

func TestConfigureServerRegistry_RestartsK3sWhenTheFilesAreNew(t *testing.T) {
	host := newRegistryHost(t)

	require.NoError(t, configureServerRegistry(host, testRegistryCA, testZotIP, testPullSecret))

	assert.Equal(t, 1, host.restarts)
	assert.Equal(t, testRegistryCA, host.read(t, "zot-ca.crt"))
	assert.Equal(t, renderZotRegistriesConfig(testZotIP, testPullSecret), host.read(t, "registries.yaml"))
	assert.Equal(t, os.FileMode(0o600), host.mode(t, "zot-ca.crt"))
	assert.Equal(t, os.FileMode(0o600), host.mode(t, "registries.yaml"))
	assert.NoFileExists(t, host.path(filepath.Base(zotRestartPendingPath)))
	for _, c := range host.commands {
		assert.NotContains(t, c, testPullSecret, "the pull password must reach the host through stdin")
	}
	entries, err := os.ReadDir(host.dir)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "no staged file may be left behind")
}

func TestConfigureServerRegistry_LeavesK3sRunningWhenNothingChanged(t *testing.T) {
	host := newRegistryHost(t)
	require.NoError(t, configureServerRegistry(host, testRegistryCA, testZotIP, testPullSecret))

	require.NoError(t, configureServerRegistry(host, testRegistryCA, testZotIP, testPullSecret))

	assert.Equal(t, 1, host.restarts, "an unchanged registry configuration must not restart k3s")
	entries, err := os.ReadDir(host.dir)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "no staged file may be left behind")
}

func TestConfigureServerRegistry_RestartsK3sWhenEitherFileChanged(t *testing.T) {
	for name, next := range map[string][3]string{
		"new CA":            {testRegistryCA + "rotated\n", testZotIP, testPullSecret},
		"new pull password": {testRegistryCA, testZotIP, "rotatedsecret"},
		"new ClusterIP":     {testRegistryCA, "10.43.0.11", testPullSecret},
	} {
		t.Run(name, func(t *testing.T) {
			host := newRegistryHost(t)
			require.NoError(t, configureServerRegistry(host, testRegistryCA, testZotIP, testPullSecret))

			require.NoError(t, configureServerRegistry(host, next[0], next[1], next[2]))

			assert.Equal(t, 2, host.restarts)
			assert.Equal(t, next[0], host.read(t, "zot-ca.crt"))
			assert.Equal(t, renderZotRegistriesConfig(next[1], next[2]), host.read(t, "registries.yaml"))
		})
	}
}

func TestConfigureServerRegistry_RestartsAfterARunThatDidNotFinishItsRestart(t *testing.T) {
	host := newRegistryHost(t)
	host.failRestart = true
	require.Error(t, configureServerRegistry(host, testRegistryCA, testZotIP, testPullSecret))
	host.failRestart = false

	require.NoError(t, configureServerRegistry(host, testRegistryCA, testZotIP, testPullSecret))

	assert.Equal(t, 2, host.restarts, "files written by a run whose restart failed must still be loaded")
	assert.NoFileExists(t, host.path(filepath.Base(zotRestartPendingPath)))
}

func TestConfigureServerRegistry_FindsThePendingRestartAmongOtherOutput(t *testing.T) {
	host := newRegistryHost(t)
	host.failRestart = true
	require.Error(t, configureServerRegistry(host, testRegistryCA, testZotIP, testPullSecret))
	host.failRestart = false
	host.noise = "Warning: Permanently added the host key to the list of known hosts.\n"

	require.NoError(t, configureServerRegistry(host, testRegistryCA, testZotIP, testPullSecret))

	assert.Equal(t, 2, host.restarts, "shell noise must not hide a pending restart")
	assert.NoFileExists(t, host.path(filepath.Base(zotRestartPendingPath)))
}

func TestConfigureServerRegistry_TightensAnUnchangedFileWithoutARestart(t *testing.T) {
	host := newRegistryHost(t)
	require.NoError(t, os.WriteFile(host.path("zot-ca.crt"), []byte(testRegistryCA), 0o600))
	require.NoError(t, os.WriteFile(host.path("registries.yaml"), []byte(renderZotRegistriesConfig(testZotIP, testPullSecret)), 0o600))
	//nolint:gosec // Start with broader permissions to verify that kip restricts them.
	require.NoError(t, os.Chmod(host.path("registries.yaml"), 0o644))

	require.NoError(t, configureServerRegistry(host, testRegistryCA, testZotIP, testPullSecret))

	assert.Zero(t, host.restarts)
	assert.Equal(t, os.FileMode(0o600), host.mode(t, "registries.yaml"))
}
