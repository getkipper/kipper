package opstunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/getkipper/kipper/kip/internal/lockfile"
)

// IdleExit is how long a tunnel process stays up after its last stream closed.
const IdleExit = 15 * time.Minute

// ErrRunning indicates that another process holds the cluster's tunnel lock.
var ErrRunning = errors.New("a tunnel process already runs for this cluster")

// Source is where a tunnel process reads its manifest. Stamp changes
// whenever the manifest may have changed, such as the config file's
// modification time.
type Source struct {
	Load  func() (Manifest, error)
	Stamp func() string
}

// Run takes the cluster lock, starts the tunnel and listens on p.Socket until
// it has been idle for idle, ctx ends or a manifest reload fails. When it
// stops for any reason but idleness, it closes its open streams. It releases
// the lock only after its tunnel is down, so two processes never run the
// same device at once. It returns ErrRunning if the lock is held, or any
// startup or listener error. src callbacks and logf must be non-nil.
func Run(ctx context.Context, p Paths, src Source, idle time.Duration, logf func(string, ...any)) error {
	release, err := lockfile.TryExclusive(p.Lock)
	if errors.Is(err, lockfile.ErrHeld) {
		return ErrRunning
	}
	if err != nil {
		return err
	}
	defer release()
	stamp := src.Stamp()
	m, err := src.Load()
	if err != nil {
		return err
	}
	tun, err := Up(m, logf)
	if err != nil {
		return err
	}
	defer tun.Close()
	// The lock is held, so a socket file left here belongs to a process that
	// is gone.
	if err := os.Remove(p.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the old tunnel socket: %w", err)
	}
	l, err := net.Listen("unix", p.Socket)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", p.Socket, err)
	}
	if err := SecureFile(p.Socket); err != nil {
		_ = l.Close()
		return fmt.Errorf("securing %s: %w", p.Socket, err)
	}
	d := &reloadingDialer{tun: tun, src: src, stamp: stamp, logf: logf}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go d.watch(ctx, cancel)
	return Serve(ctx, l, d, idle)
}

// watchEvery is how often an idle tunnel process checks its manifest.
var watchEvery = 30 * time.Second

// watch checks for a changed manifest every watchEvery and reloads it when
// needed. A reload failure stops the process so device removal takes effect
// without waiting for another dial or the idle timeout.
func (d *reloadingDialer) watch(ctx context.Context, stop func()) {
	t := time.NewTicker(watchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.reload(); err != nil {
				d.logf("stopping: %v", err)
				stop()
				return
			}
		}
	}
}

// ErrDeviceChanged is a tunnel process's answer when its manifest could not
// be loaded or applied, so it stopped serving and the next command starts a
// new process from the current configuration.
var ErrDeviceChanged = errors.New("the device changed, so this tunnel process is stopping. Run the command again")

// reloadingDialer reloads the manifest before a dial when its stamp moved.
// A manifest it cannot load or apply fails the dial with ErrDeviceChanged,
// which stops Serve, so the next command starts a process from the current
// configuration.
type reloadingDialer struct {
	tun   *Tunnel
	src   Source
	logf  func(string, ...any)
	mu    sync.Mutex
	stamp string
}

func (d *reloadingDialer) Dial(ctx context.Context, machineID string, port int) (net.Conn, error) {
	if err := d.reload(); err != nil {
		d.logf("stopping: %v", err)
		return nil, ErrDeviceChanged
	}
	return d.tun.Dial(ctx, machineID, port)
}

func (d *reloadingDialer) reload() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	stamp := d.src.Stamp()
	if stamp == d.stamp {
		return nil
	}
	m, err := d.src.Load()
	if err != nil {
		return err
	}
	if err := d.tun.Reload(m); err != nil {
		return err
	}
	d.stamp = stamp
	return nil
}

// StartDetached starts exe with args as a background process that outlives
// its parent, with stdout and stderr in logPath. It truncates the log and
// returns errors from opening it, starting the process or releasing its handle.
func StartDetached(exe string, args []string, logPath string) error {
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // logPath is under kip's own runtime directory
	if err != nil {
		return fmt.Errorf("opening the tunnel log: %w", err)
	}
	defer func() { _ = log.Close() }()
	cmd := exec.Command(exe, args...) //nolint:gosec // exe is kip's own binary
	cmd.Stdout = log
	cmd.Stderr = log
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
