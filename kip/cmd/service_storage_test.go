package cmd

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/service"
)

func TestMoveConsentNeedsAnAnswer(t *testing.T) {
	plan := installer.MovePlan{Namespace: "shop", Service: "db", UsedBytes: 3 << 30, TargetSize: "4Gi", DumpClaim: "db-dump", BackupAge: 4 * time.Hour}
	never := func(string) (bool, error) { t.Fatal("no prompt expected"); return false, nil }

	out := &bytes.Buffer{}
	ok, err := moveConsent(out, never, false, true)(plan)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Contains(t, out.String(), "3Gi of data")
	assert.Contains(t, out.String(), "4h0m0s ago")
	assert.Contains(t, out.String(), "db-dump")
	assert.Contains(t, out.String(), "stays stopped until the copy is verified")

	_, err = moveConsent(&bytes.Buffer{}, never, false, false)(plan)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--yes")

	asked := ""
	ok, err = moveConsent(&bytes.Buffer{}, func(p string) (bool, error) { asked = p; return false, nil }, true, false)(plan)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Contains(t, asked, "Stop db")

	out.Reset()
	plan.NoBackup = true
	_, _ = moveConsent(out, never, false, true)(plan)
	assert.Contains(t, out.String(), "no backups")
}

func TestMoveRoomCheckRefusesWhatItCannotConfirm(t *testing.T) {
	var seen []service.NewVolume
	check := func(warning string, err error) func([]service.NewVolume) (string, error) {
		return func(v []service.NewVolume) (string, error) { seen = v; return warning, err }
	}
	require.NoError(t, refuseMoveWithoutRoom(check("", nil), "shop", "db", 4<<30, 4<<30))
	require.Len(t, seen, 2, "the replacement dump volume needs room too")
	for _, v := range seen {
		assert.Empty(t, v.Claim, "the existing local claims must not make the check skip the new volumes")
		assert.Equal(t, int64(4<<30), v.Bytes)
	}
	require.NoError(t, refuseMoveWithoutRoom(check("", nil), "shop", "db", 4<<30, 0))
	assert.Len(t, seen, 1)

	err := refuseMoveWithoutRoom(check("storage room was not checked: timeout", nil), "shop", "db", 1, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not checked")
	assert.Error(t, refuseMoveWithoutRoom(check("", errors.New("not enough room")), "shop", "db", 1, 0))
}
