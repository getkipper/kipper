package installer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/auth"
)

// An unreachable Dex leaves the session's state unknown, so verification
// reports a transport failure.
func TestVerifyOperatorIdentityReportsAnUnreachableDexAsTransport(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.NoError(t, auth.Mutate(func(s *auth.Store) {
		s.Clusters["c1"] = &auth.Credentials{IDToken: "old", RefreshToken: "ref", ExpiresAt: time.Now().Add(-time.Minute)}
	}))

	result, _, err := VerifyOperatorIdentity(context.Background(), "c1", "127.0.0.1:1", "example.com", "https://127.0.0.1:1", nil)

	assert.Equal(t, ProofTransportError, result)
	assert.True(t, errors.Is(err, auth.ErrDexUnreachable), "%v", err)
}
