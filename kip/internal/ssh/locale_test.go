package ssh

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// SSH configurations such as macOS's "SendEnv LANG LC_*" forward locale variables.
// An unavailable locale on the host can cause shell warnings in output kip parses.
func TestSSHProcessesCarryNoLocaleVariables(t *testing.T) {
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_ALL", "en_US.UTF-8")
	t.Setenv("LC_CTYPE", "en_US.UTF-8")
	t.Setenv("LANGUAGE", "en_US")
	t.Setenv("KIP_LOCALE_TEST_KEEP", "1")

	for _, timeout := range []time.Duration{0, time.Minute} {
		c := &Client{cfg: Config{Host: "203.0.113.10", User: "root", CommandTimeout: timeout}}
		cmd, _, cancel := c.command("true")
		cancel()
		assert.NotNil(t, cmd.Env, "an unset Env inherits the locale")
		for _, kv := range cmd.Env {
			name, _, _ := strings.Cut(kv, "=")
			assert.False(t, name == "LANG" || name == "LANGUAGE" || strings.HasPrefix(name, "LC_"), "ssh must not receive %s", name)
		}
		assert.True(t, slices.Contains(cmd.Env, "KIP_LOCALE_TEST_KEEP=1"), "other variables, such as SSH_AUTH_SOCK, stay")
	}
}

func TestLocaleVariablesAreDroppedWhateverTheirCase(t *testing.T) {
	t.Setenv("Lc_All", "en_US.UTF-8")
	t.Setenv("language", "en_US")
	for _, kv := range EnvironWithoutLocale() {
		name, _, _ := strings.Cut(kv, "=")
		assert.NotContains(t, []string{"Lc_All", "language"}, name)
	}
}
