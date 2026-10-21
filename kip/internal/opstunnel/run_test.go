package opstunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSource serves a manifest the test can swap.
type fakeSource struct {
	mu    sync.Mutex
	m     Manifest
	err   error
	stamp int
}

// fail makes the manifest unreadable, as when the device is removed.
func (f *fakeSource) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
	f.stamp++
}

func (f *fakeSource) set(m Manifest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m = m
	f.stamp++
}

func (f *fakeSource) source() Source {
	return Source{
		Load: func() (Manifest, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.m, f.err
		},
		Stamp: func() string {
			f.mu.Lock()
			defer f.mu.Unlock()
			return fmt.Sprint(f.stamp)
		},
	}
}

func TestRunServesTheTunnelOnItsSocket(t *testing.T) {
	node, workstation := newKeyPair(t), newKeyPair(t)
	port := startNode(t, node, workstation)
	m := Manifest{PrivateKey: b64(workstation.private), Address: deviceAddr, Nodes: []Node{{
		MachineID: "m-1", Name: "node-1", Endpoint: fmt.Sprintf("127.0.0.1:%d", port),
		PublicKey: b64(node.public), Address: nodeAddr, SSHPorts: []int{22},
	}}}
	src := &fakeSource{m: m}
	p := testPaths(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, p, src.source(), time.Minute, t.Logf) }()
	require.Eventually(t, func() bool { return Reachable(p.Socket) }, 5*time.Second, 20*time.Millisecond)

	assert.ErrorIs(t, Run(ctx, p, src.source(), time.Minute, t.Logf), ErrRunning, "one process per cluster")

	dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
	defer dcancel()
	c, err := DialVia(dctx, p.Socket, "m-1", 22)
	require.NoError(t, err)
	_, err = c.Write([]byte("hi"))
	require.NoError(t, err)
	buf := make([]byte, 2)
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)
	assert.Equal(t, "hi", string(buf))
	_ = c.Close()

	m.Nodes[0].MachineID = "m-renamed"
	src.set(m)
	_, err = DialVia(dctx, p.Socket, "m-1", 22)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a node this device reaches", "the changed manifest was loaded")

	m.Address = netip.MustParseAddr("10.99.0.130")
	src.set(m)
	_, err = DialVia(dctx, p.Socket, "m-renamed", 22)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the device changed")
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the process kept serving a device it no longer carries")
	}
}

func TestRunStopsSoonAfterItsDeviceIsRemoved(t *testing.T) {
	defer func(d time.Duration) { watchEvery = d }(watchEvery)
	watchEvery = 20 * time.Millisecond
	node, workstation := newKeyPair(t), newKeyPair(t)
	port := startNode(t, node, workstation)
	src := &fakeSource{m: Manifest{PrivateKey: b64(workstation.private), Address: deviceAddr, Nodes: []Node{{
		MachineID: "m-1", Name: "node-1", Endpoint: fmt.Sprintf("127.0.0.1:%d", port),
		PublicKey: b64(node.public), Address: nodeAddr, SSHPorts: []int{22},
	}}}}
	p := testPaths(t)
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), p, src.source(), time.Hour, t.Logf) }()
	require.Eventually(t, func() bool { return Reachable(p.Socket) }, 5*time.Second, 20*time.Millisecond)

	src.fail(errors.New("this workstation has no device for the cluster"))

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the tunnel process kept running with a removed device")
	}
}

func TestOnlyTheProcessThatRunsStartsTheLogAfresh(t *testing.T) {
	node, workstation := newKeyPair(t), newKeyPair(t)
	m := Manifest{PrivateKey: b64(workstation.private), Address: deviceAddr, Nodes: []Node{{
		MachineID: "m-1", Name: "node-1", Endpoint: "127.0.0.1:9", PublicKey: b64(node.public), Address: nodeAddr, SSHPorts: []int{22},
	}}}
	src := &fakeSource{m: m}
	p := testPaths(t)
	p.Log = p.Socket + ".log"
	require.NoError(t, os.WriteFile(p.Log, []byte("an earlier process\n"), 0o600))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	go func() { done <- Run(ctx, p, src.source(), time.Minute, func(string, ...any) {}) }()
	require.Eventually(t, func() bool { return Reachable(p.Socket) }, 5*time.Second, 20*time.Millisecond)
	log, err := os.ReadFile(p.Log)
	require.NoError(t, err)
	assert.NotContains(t, string(log), "an earlier process")

	require.NoError(t, os.WriteFile(p.Log, []byte("the running process\n"), 0o600))
	assert.ErrorIs(t, Run(ctx, p, src.source(), time.Minute, func(string, ...any) {}), ErrRunning)
	log, err = os.ReadFile(p.Log)
	require.NoError(t, err)
	assert.Contains(t, string(log), "the running process", "a process that loses the race leaves the log alone")
}

func TestStartDetachedAppendsToTheLog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	log := shortSocket(t) + ".log"
	require.NoError(t, os.WriteFile(log, []byte("the running process\n"), 0o600))

	require.NoError(t, StartDetached("/bin/sh", []string{"-c", "echo a second starter"}, log))

	require.Eventually(t, func() bool {
		b, _ := os.ReadFile(log)
		return strings.Contains(string(b), "a second starter")
	}, 5*time.Second, 20*time.Millisecond)
	b, err := os.ReadFile(log)
	require.NoError(t, err)
	assert.Contains(t, string(b), "the running process")
}
