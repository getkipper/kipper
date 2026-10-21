package opstunnel

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/lockfile"
)

func TestRuntimePathsAreOwnerOnlyAndPerCluster(t *testing.T) {
	home, err := os.MkdirTemp("", "oth")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	a, err := RuntimePaths(home, "prod")
	require.NoError(t, err)
	b, err := RuntimePaths(home, "staging")
	require.NoError(t, err)
	assert.NotEqual(t, a.Socket, b.Socket)
	assert.Equal(t, filepath.Join(home, ".kip", "run"), filepath.Dir(a.Socket))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Dir(a.Socket))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	}
}

func TestRuntimePathsMoveALongSocketPath(t *testing.T) {
	base, err := os.MkdirTemp("", "otl")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	home := filepath.Join(base, strings.Repeat("h", 90))
	p, err := RuntimePaths(home, "prod")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(p.Socket)) })
	assert.LessOrEqual(t, len(p.Socket), socketPathMax)
	assert.True(t, strings.HasPrefix(p.Lock, home), "the lock and log stay under the home directory")
}

func testPaths(t *testing.T) Paths {
	t.Helper()
	socket := shortSocket(t)
	return Paths{Socket: socket, Lock: socket + ".lock"}
}

func TestEnsureStartsAProcessWhenNoneRuns(t *testing.T) {
	p := testPaths(t)
	starts := 0
	err := Ensure(context.Background(), p, func() error {
		starts++
		l, err := net.Listen("unix", p.Socket)
		if err != nil {
			return err
		}
		t.Cleanup(func() { _ = l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, starts)
}

func TestEnsureWaitsForTheLockHolder(t *testing.T) {
	p := testPaths(t)
	release, err := lockfile.TryExclusive(p.Lock)
	require.NoError(t, err)
	go func() {
		time.Sleep(300 * time.Millisecond)
		l, err := net.Listen("unix", p.Socket)
		if err != nil {
			return
		}
		t.Cleanup(func() { _ = l.Close(); release() })
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	err = Ensure(context.Background(), p, func() error {
		return errors.New("a second process must not start while the lock is held")
	})
	require.NoError(t, err)
}

func TestEnsureReportsAStartFailure(t *testing.T) {
	p := testPaths(t)
	err := Ensure(context.Background(), p, func() error { return errors.New("no binary") })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no binary")
}
