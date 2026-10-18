package cmd

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/auth"
)

func TestSessionStatusTellsAnOutageFromAnExpiredSession(t *testing.T) {
	assert.Equal(t, "authenticated", sessionStatus(nil))
	assert.Contains(t, sessionStatus(auth.ErrSessionExpired), "kip auth login")
	outage := fmt.Errorf("%w at dex.example.com (connection refused)", auth.ErrDexUnreachable)
	assert.Contains(t, sessionStatus(outage), "could not be refreshed with Dex")
	assert.NotContains(t, sessionStatus(outage), "kip auth login")
}
