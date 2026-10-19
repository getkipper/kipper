package cmd

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDumpCheck(t *testing.T) {
	results, present, err := parseDumpCheck("KIPPER_DUMP_OK\tapp\t2\nKIPPER_DUMP_BAD\todd name\tthe dump does not match its recorded checksum\n")
	require.NoError(t, err)
	assert.True(t, present)
	assert.Equal(t, []dumpCheck{
		{Database: "app", OK: true, Detail: "2"},
		{Database: "odd name", OK: false, Detail: "the dump does not match its recorded checksum"},
	}, results)

	_, present, err = parseDumpCheck("KIPPER_DUMP_NONE\n")
	require.NoError(t, err)
	assert.False(t, present, "a service without consistent backups has nothing to check")

	_, _, err = parseDumpCheck("KIPPER_DUMP_INCOMPLETE\n")
	assert.Error(t, err, "an incomplete dump is a failed check, not a missing one")

	_, _, err = parseDumpCheck("psql: something unexpected\n")
	assert.Error(t, err)
}

func TestAFailedDumpFailsTheDrill(t *testing.T) {
	exec := func(_ context.Context, _ []string, _ io.Reader, stdout, _ io.Writer) error {
		_, err := io.WriteString(stdout, "KIPPER_DUMP_OK\tapp\t2\nKIPPER_DUMP_BAD\tother\tpg_restore failed: boom\n")
		return err
	}
	assert.Error(t, checkConsistentDump(context.Background(), exec, "db-0"))

	clean := func(_ context.Context, _ []string, _ io.Reader, stdout, _ io.Writer) error {
		_, err := io.WriteString(stdout, "KIPPER_DUMP_NONE\n")
		return err
	}
	assert.NoError(t, checkConsistentDump(context.Background(), clean, "db-0"))
}
