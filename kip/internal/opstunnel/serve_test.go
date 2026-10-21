package opstunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// echoDialer answers every dial with one end of a pipe whose other end
// echoes, and refuses port 1.
type echoDialer struct{ dialed chan string }

func (d *echoDialer) Dial(_ context.Context, host string, port int) (net.Conn, error) {
	if port == 1 {
		return nil, errors.New("port 1 is closed\non two lines")
	}
	if d.dialed != nil {
		d.dialed <- host
	}
	a, b := net.Pipe()
	go func() { _, _ = io.Copy(b, b); _ = b.Close() }()
	return a, nil
}

// shortSocket returns a socket path short enough for AF_UNIX on macOS.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ot")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

func serve(t *testing.T, d Dialer, idle time.Duration) (string, chan error) {
	t.Helper()
	socket := shortSocket(t)
	l, err := net.Listen("unix", socket)
	require.NoError(t, err)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { done <- Serve(ctx, l, d, idle) }()
	return socket, done
}

func TestDialViaCarriesAStream(t *testing.T) {
	dialed := make(chan string, 1)
	socket, _ := serve(t, &echoDialer{dialed: dialed}, time.Minute)
	c, err := DialVia(context.Background(), socket, "node-1", 22)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	assert.Equal(t, "node-1", <-dialed)
	_, err = c.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(buf))
}

func TestDialViaReportsTheDialError(t *testing.T) {
	socket, _ := serve(t, &echoDialer{}, time.Minute)
	_, err := DialVia(context.Background(), socket, "node-1", 1)
	require.Error(t, err)
	assert.Equal(t, "port 1 is closed on two lines", err.Error())
}

func TestServeRefusesAnythingButDial(t *testing.T) {
	socket, _ := serve(t, &echoDialer{}, time.Minute)
	for _, req := range []string{"connect node-1 22\n", "dial node-1\n", "dial node-1 70000\n", "dial node-1 ssh\n"} {
		c, err := net.Dial("unix", socket)
		require.NoError(t, err)
		_, err = c.Write([]byte(req))
		require.NoError(t, err)
		reply, _ := io.ReadAll(c)
		_ = c.Close()
		assert.Contains(t, string(reply), "error ", req)
	}
}

func TestServeExitsWhenIdle(t *testing.T) {
	socket, done := serve(t, &echoDialer{}, 100*time.Millisecond)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve kept running with nothing open")
	}
	assert.False(t, Reachable(socket), "the listener is closed once Serve returns")
}

func TestServeStaysWhileAStreamIsOpen(t *testing.T) {
	socket, done := serve(t, &echoDialer{}, 100*time.Millisecond)
	c, err := DialVia(context.Background(), socket, "node-1", 22)
	require.NoError(t, err)
	select {
	case <-done:
		t.Fatal("Serve exited with a stream open")
	case <-time.After(400 * time.Millisecond):
	}
	_ = c.Close()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve kept running after the last stream closed")
	}
}

func TestServeStopsWithItsContext(t *testing.T) {
	socket := shortSocket(t)
	l, err := net.Listen("unix", socket)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, &echoDialer{}, time.Minute) }()
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve ignored its context")
	}
}

func TestServeClosesOpenStreamsWhenStopped(t *testing.T) {
	socket := shortSocket(t)
	l, err := net.Listen("unix", socket)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, &echoDialer{}, time.Minute) }()
	c, err := DialVia(context.Background(), socket, "m-1", 22)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve waited for the open stream instead of closing it")
	}
	assert.False(t, Reachable(socket), "a stopped process accepts nothing new")
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = c.Read(make([]byte, 1))
	assert.Error(t, err, "the open stream was closed, so no successor shares the device with it")
}

// changedDialer carries streams like echoDialer until its device changes.
type changedDialer struct {
	echoDialer
	changed atomic.Bool
}

func (d *changedDialer) Dial(ctx context.Context, host string, port int) (net.Conn, error) {
	if d.changed.Load() {
		return nil, ErrDeviceChanged
	}
	return d.echoDialer.Dial(ctx, host, port)
}

func TestServeAnswersADeviceChangeBeforeStopping(t *testing.T) {
	d := &changedDialer{}
	socket, done := serve(t, d, time.Minute)
	open, err := DialVia(context.Background(), socket, "m-1", 22)
	require.NoError(t, err)
	defer func() { _ = open.Close() }()

	d.changed.Store(true)
	_, err = DialVia(context.Background(), socket, "m-1", 22)
	assert.ErrorIs(t, err, ErrDeviceChanged, "the client hears why, so it starts a successor")
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve kept serving a device it no longer carries")
	}
	_ = open.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = open.Read(make([]byte, 1))
	assert.Error(t, err, "the stream of the old device was closed")
}
