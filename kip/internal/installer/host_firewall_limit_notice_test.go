package installer

import (
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unsharedUFWHost models a UFW host reached through a new SSH connection per command.
type unsharedUFWHost struct{ *ufwHost }

func (unsharedUFWHost) SurvivesSSHRateLimit() bool { return false }

func stdoutOf(t *testing.T, run func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()
	run()
	require.NoError(t, w.Close())
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(out)
}

// A new SSH connection per command can hit UFW's rate limit, so kip allows
// SSH without rate limiting and notifies the operator.
func TestConfigureFirewallSaysWhenSSHIsNotRateLimited(t *testing.T) {
	noUFWRetryDelay(t)
	h := unsharedUFWHost{newUFWHost()}

	out := stdoutOf(t, func() {
		require.NoError(t, configureFirewallWith(h, true, FirewallConsent{}, FirewallMesh{}))
	})

	assert.True(t, h.live["ufw allow 22/tcp"])
	assert.Contains(t, out, "without rate limiting")
}

// An operator who turned rate limiting off needs no notice about it.
func TestConfigureFirewallSaysNothingWhenRateLimitingIsOff(t *testing.T) {
	noUFWRetryDelay(t)
	h := unsharedUFWHost{newUFWHost()}

	out := stdoutOf(t, func() {
		require.NoError(t, configureFirewallWith(h, false, FirewallConsent{}, FirewallMesh{}))
	})

	assert.NotContains(t, out, "without rate limiting")
}
