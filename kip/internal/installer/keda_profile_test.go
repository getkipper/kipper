package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallKEDASizesTheInterceptorsByProfile(t *testing.T) {
	for profile, want := range map[string]string{ProfileNano: "min: 1", ProfileSmall: "min: 3"} {
		t.Run(profile, func(t *testing.T) {
			host := &hostScript{}
			require.NoError(t, InstallKEDA(host, profile))
			all := strings.Join(host.commands, "\n")
			assert.Contains(t, all, "    interceptor:\n      replicas:\n        "+want+"\n")
		})
	}
}
