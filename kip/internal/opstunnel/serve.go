package opstunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Dialer opens a connection through the tunnel to a node identified by its
// machine ID. The caller owns the returned connection.
type Dialer interface {
	Dial(ctx context.Context, host string, port int) (net.Conn, error)
}

// dialTimeout limits request reading and upstream connection setup separately.
const dialTimeout = 10 * time.Second

// Serve answers "dial <machine-id> <port>" requests on l by opening the
// connection through d and splicing it to the client. It returns once no
// stream has been open for idle, or when ctx ends. Either way it closes l
// first, so a later client is refused rather than left waiting. When ctx
// ends, it also closes every open stream: a successor must never run the
// same device beside this process. It returns only after every stream has
// ended. A dial that fails with ErrDeviceChanged stops it the same way,
// once that client has its answer. Listener errors are returned. Context
// cancellation returns nil.
func Serve(ctx context.Context, l net.Listener, d Dialer, idle time.Duration) error {
	s := &streams{open: map[io.Closer]struct{}{}}
	timer := time.NewTimer(idle)
	defer timer.Stop()
	conns := make(chan net.Conn)
	acceptErr := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				acceptErr <- err
				return
			}
			select {
			case conns <- c:
			case <-stop:
				_ = c.Close()
				return
			}
		}
	}()
	shutdown := func() {
		_ = l.Close()
		s.closeAll()
		s.wg.Wait()
	}
	finished := make(chan struct{}, 1)
	changed := make(chan struct{}, 1)
	for {
		select {
		case <-ctx.Done():
			shutdown()
			return nil
		case <-changed:
			shutdown()
			return nil
		case err := <-acceptErr:
			shutdown()
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		case <-timer.C:
			if s.count() == 0 {
				_ = l.Close()
				s.wg.Wait()
				return nil
			}
		case <-finished:
			if s.count() == 0 {
				timer.Reset(idle)
			}
		case c := <-conns:
			untrack := s.track(c)
			timer.Stop()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				err := handle(ctx, c, d, s)
				untrack()
				done := finished
				if errors.Is(err, ErrDeviceChanged) {
					done = changed
				}
				select {
				case done <- struct{}{}:
				default:
				}
			}()
		}
	}
}

// streams is the set of open client and upstream connections.
type streams struct {
	mu      sync.Mutex
	open    map[io.Closer]struct{}
	clients int
	closed  bool
	wg      sync.WaitGroup
}

// track adds a client connection and returns the function that removes it.
func (s *streams) track(c io.Closer) func() {
	s.mu.Lock()
	s.clients++
	s.mu.Unlock()
	untrack := s.add(c)
	return func() {
		untrack()
		s.mu.Lock()
		s.clients--
		s.mu.Unlock()
	}
}

// add registers c to be closed on shutdown. A connection added after
// shutdown began is closed at once.
func (s *streams) add(c io.Closer) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = c.Close()
		return func() {}
	}
	s.open[c] = struct{}{}
	return func() {
		s.mu.Lock()
		delete(s.open, c)
		s.mu.Unlock()
	}
}

func (s *streams) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clients
}

func (s *streams) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for c := range s.open {
		_ = c.Close()
	}
}

// handle serves one client and returns the dial error it answered with,
// after the answer was written.
func handle(ctx context.Context, c net.Conn, d Dialer, s *streams) error {
	_ = c.SetReadDeadline(time.Now().Add(dialTimeout))
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil {
		_ = c.Close()
		return nil
	}
	_ = c.SetReadDeadline(time.Time{})
	host, port, err := parseRequest(line)
	if err != nil {
		reply(c, err)
		return nil
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	upstream, err := d.Dial(dctx, host, port)
	cancel()
	if err != nil {
		reply(c, err)
		return err
	}
	defer s.add(upstream)()
	if _, err := c.Write([]byte("ok\n")); err != nil {
		_ = c.Close()
		_ = upstream.Close()
		return nil
	}
	splice(&bufferedConn{Conn: c, r: r}, upstream)
	return nil
}

func reply(c net.Conn, err error) {
	_ = c.SetWriteDeadline(time.Now().Add(dialTimeout))
	_, _ = fmt.Fprintf(c, "error %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
	_ = c.Close()
}

func parseRequest(line string) (string, int, error) {
	f := strings.Fields(line)
	if len(f) != 3 || f[0] != "dial" {
		return "", 0, errors.New("the tunnel accepts only dial requests")
	}
	port, err := strconv.Atoi(f[2])
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("%q is not a port", f[2])
	}
	return f[1], port, nil
}

// bufferedConn reads what the request reader already buffered first.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// CloseWrite half-closes the underlying connection when supported and closes
// it fully otherwise.
func (b *bufferedConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return b.Close()
}

// DialVia asks the tunnel process listening on socket for a connection to
// port on the node with machineID. The returned connection carries the
// stream once the process answers ok. The request uses the context deadline,
// or 15 seconds if none is set. The deadline is cleared before returning.
// Connection, protocol and remote dial errors are returned to the caller.
func DialVia(ctx context.Context, socket, machineID string, port int) (net.Conn, error) {
	var nd net.Dialer
	c, err := nd.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	} else {
		_ = c.SetDeadline(time.Now().Add(dialTimeout + 5*time.Second))
	}
	if _, err := fmt.Fprintf(c, "dial %s %d\n", machineID, port); err != nil {
		_ = c.Close()
		return nil, err
	}
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("the tunnel process closed the connection: %w", err)
	}
	_ = c.SetDeadline(time.Time{})
	line = strings.TrimSpace(line)
	if line != "ok" {
		_ = c.Close()
		msg := strings.TrimPrefix(line, "error ")
		if msg == ErrDeviceChanged.Error() {
			return nil, ErrDeviceChanged
		}
		return nil, errors.New(msg)
	}
	return &bufferedConn{Conn: c, r: r}, nil
}
