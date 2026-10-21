package opstunnel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/getkipper/kipper/kip/internal/lockfile"
)

// socketPathMax leaves room below the smallest AF_UNIX limit (104 bytes on
// macOS, 108 on Linux and Windows).
const socketPathMax = 100

// startWait is how long a starter waits for a tunnel process to listen.
const startWait = 5 * time.Second

// Paths are one cluster's runtime files.
type Paths struct {
	Socket string
	Lock   string
	Log    string
}

// RuntimePaths returns the socket, lock and log paths for cluster under
// home/.kip/run, after creating that directory owner-only. A socket path
// too long for AF_UNIX moves to an owner-only directory under the user's
// temporary directory.
func RuntimePaths(home, cluster string) (Paths, error) {
	sum := sha256.Sum256([]byte(cluster))
	name := "ops-" + hex.EncodeToString(sum[:])[:12]
	dir := filepath.Join(home, ".kip", "run")
	if err := secureDir(dir); err != nil {
		return Paths{}, err
	}
	p := Paths{
		Socket: filepath.Join(dir, name+".sock"),
		Lock:   filepath.Join(dir, name+".lock"),
		Log:    filepath.Join(dir, name+".log"),
	}
	if len(p.Socket) > socketPathMax {
		hsum := sha256.Sum256([]byte(home))
		short := filepath.Join(os.TempDir(), "kip-"+hex.EncodeToString(hsum[:])[:8])
		if err := secureDir(short); err != nil {
			return Paths{}, err
		}
		p.Socket = filepath.Join(short, name+".sock")
		if len(p.Socket) > socketPathMax {
			return Paths{}, fmt.Errorf("the tunnel's socket path %s is longer than the system allows", p.Socket)
		}
	}
	return p, nil
}

// Reachable reports whether a connection to socket succeeds within one second.
// It closes the probe connection without checking the tunnel protocol.
func Reachable(socket string) bool {
	c, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// Ensure returns once a tunnel process listens on p.Socket, starting one
// with start when none does. While another process holds the lock it keeps
// trying the socket and the lock for up to five seconds, so it notices a
// process that is just exiting and starts its successor.
func Ensure(ctx context.Context, p Paths, start func() error) error {
	if Reachable(p.Socket) {
		return nil
	}
	deadline := time.Now().Add(startWait)
	started := false
	for {
		if Reachable(p.Socket) {
			return nil
		}
		if !started {
			release, err := lockfile.TryExclusive(p.Lock)
			if err != nil && !errors.Is(err, lockfile.ErrHeld) {
				return err
			}
			if err == nil {
				// Release the lock so the new process can acquire it.
				release()
				if err := start(); err != nil {
					return fmt.Errorf("starting the tunnel process: %w", err)
				}
				started = true
			}
		}
		if time.Now().After(deadline) {
			return errors.New("the tunnel process did not start within 5 seconds. Run 'kip cluster access connect' to see why")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
