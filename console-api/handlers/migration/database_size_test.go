package migration

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSizeExec answers a logical size query and a du with fixed outputs.
func fakeSizeExec(logical string, logicalErr error, du string) func([]string) (string, error) {
	return func(cmd []string) (string, error) {
		if strings.Contains(strings.Join(cmd, " "), "du -sb") {
			return du, nil
		}
		return logical, logicalErr
	}
}

// A data directory holds more than a dump carries: binary logs, redo logs and
// WAL. Measured with du, a MySQL service holding a few hundred MB of tables
// read as nearly 800 MB and was refused for automatic transfer.
func TestEstimateSize_MeasuresWhatADumpCarries(t *testing.T) {
	for _, svcType := range []string{"mysql", "postgres"} {
		t.Run(svcType, func(t *testing.T) {
			size, err := estimateSize(svcType, fakeSizeExec("314572800\n", nil, "832569344"))

			require.NoError(t, err)
			assert.Equal(t, int64(314572800), size)
		})
	}
}

func TestEstimateSize_FallsBackToTheDataDirectory(t *testing.T) {
	size, err := estimateSize("mysql", fakeSizeExec("", errors.New("access denied"), "832569344\n"))
	require.NoError(t, err)
	assert.Equal(t, int64(832569344), size, "an engine that does not answer is measured on disk")

	size, err = estimateSize("mysql", fakeSizeExec("NULL\n", nil, "832569344\n"))
	require.NoError(t, err)
	assert.Equal(t, int64(832569344), size, "an answer that is not a number is measured on disk")

	size, err = estimateSize("redis", fakeSizeExec("1", nil, "4096\n"))
	require.NoError(t, err)
	assert.Equal(t, int64(4096), size, "a type without a size query is measured on disk")
}

func TestLogicalSizeCommand_LeavesTheSystemSchemasOut(t *testing.T) {
	cmd := strings.Join(logicalSizeCommand("mysql"), " ")
	for _, schema := range []string{"mysql", "information_schema", "performance_schema", "sys"} {
		assert.Contains(t, cmd, "'"+schema+"'", "only the user's own tables are summed")
	}
	assert.Contains(t, strings.Join(logicalSizeCommand("postgres"), " "), "NOT datistemplate", "template databases are not migrated")
}

// MySQL serves table sizes from statistics it caches for a day, which a
// database loaded since then has outgrown.
func TestLogicalSizeCommand_ReadsCurrentMySQLStatistics(t *testing.T) {
	assert.Contains(t, strings.Join(logicalSizeCommand("mysql"), " "), "SET SESSION information_schema_stats_expiry = 0;")
}
