package migration

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Database names come out of the source cluster's pg_database and end up
// inside SQL sent to the target, so anything that could carry quoting or
// injection is refused rather than escaped.
func TestValidDBName(t *testing.T) {
	valid := []string{"app", "acme_prod", "shop-db", "Analytics", "_staging", "db$1"}
	for _, name := range valid {
		if !validDBName.MatchString(name) {
			t.Errorf("expected %q to be accepted", name)
		}
	}

	invalid := []string{
		"",
		"1starts-with-digit",
		"has space",
		"quote'break",
		`double"quote`,
		"semi;colon",
		"back`tick",
		"new\nline",
		"dollar$(sub)",
		"paren(",
	}
	for _, name := range invalid {
		if validDBName.MatchString(name) {
			t.Errorf("expected %q to be rejected", name)
		}
	}
}

// TestWaitPostgresReadyChecksThePostgresDatabase guards against repeated FATAL
// logs during migration readiness checks. Without -d, pg_isready defaults to a
// database named after the user, which may not exist.
func TestWaitPostgresReadyChecksThePostgresDatabase(t *testing.T) {
	var probe []string
	h := &Handler{execStream: func(_ context.Context, _, _, _ string, cmd []string, _ io.Reader, _ io.Writer) error {
		probe = cmd
		return nil
	}}

	require.NoError(t, h.waitPostgresReady(context.Background(), "shop-prod", "db-0", "db", "app_user"))

	assert.Equal(t, []string{"pg_isready", "-h", "127.0.0.1", "-p", "5432", "-U", "app_user", "-d", "postgres"}, probe)
}
