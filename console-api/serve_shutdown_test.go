package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
)

// startServing runs serveUntilStopped on a local listener with a handler that
// answers once release is closed. It returns the address, serving result
// channel and a channel closed when the handler receives a request.
func startServing(t *testing.T, ctx context.Context, managerDone <-chan struct{}, release <-chan struct{}, wait time.Duration) (string, chan error, chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	arrived := make(chan struct{})
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(arrived)
		<-release
		_, _ = io.WriteString(w, "done")
	})}
	result := make(chan error, 1)
	go func() { result <- serveUntilStopped(ctx, srv, ln, managerDone, wait) }()
	return "http://" + ln.Addr().String(), result, arrived
}

func TestShutdownFinishesInFlightRequestsAfterTheManagerStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	managerDone := make(chan struct{})
	release := make(chan struct{})
	url, result, arrived := startServing(t, ctx, managerDone, release, 10*time.Second)

	answer := make(chan string, 1)
	go func() {
		resp, err := http.Get(url) //nolint:noctx // a test request to a local server
		if err != nil {
			answer <- "error: " + err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		answer <- string(body)
	}()
	<-arrived

	cancel()
	close(managerDone)
	select {
	case <-result:
		t.Fatal("serving stopped while a request was still in flight")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	assert.Equal(t, "done", <-answer)
	select {
	case err := <-result:
		assert.NoError(t, err, "a server closed by the shutdown is no failure")
	case <-time.After(5 * time.Second):
		t.Fatal("serving did not stop after the last request finished")
	}
}

func TestShutdownWaitsForTheManagerAndIsBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	managerDone := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	url, result, arrived := startServing(t, ctx, managerDone, release, 100*time.Millisecond)
	go func() {
		if resp, err := http.Get(url); err == nil { //nolint:noctx // a test request to a local server
			_ = resp.Body.Close()
		}
	}()
	<-arrived

	cancel()
	select {
	case <-result:
		t.Fatal("serving stopped before the controller manager did")
	case <-time.After(200 * time.Millisecond):
	}
	close(managerDone)
	select {
	case err := <-result:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("a request that never finishes held the shutdown past its wait")
	}
}

// TestTheControllerLeaseRidesOutAnAPIServerPause checks that the timing allows
// API server pauses lasting tens of seconds, such as a k3s restart during an
// upgrade or server reboot. The gap before Lease expiry gives a leader that
// misses the renewal deadline time to exit before another pod takes over.
func TestTheControllerLeaseRidesOutAnAPIServerPause(t *testing.T) {
	opts := controllerManagerOptions(runtime.NewScheme(), &heldLease{timing: controllerLeaseTiming})
	require.NotNil(t, opts.LeaseDuration)
	require.NotNil(t, opts.RenewDeadline)
	require.NotNil(t, opts.RetryPeriod)
	assert.GreaterOrEqual(t, *opts.RenewDeadline, 40*time.Second, "a pause shorter than this keeps the Lease")
	assert.GreaterOrEqual(t, *opts.LeaseDuration-*opts.RenewDeadline-*opts.RetryPeriod, 15*time.Second, "a leader that gives up has time to exit first")
	assert.LessOrEqual(t, *opts.RetryPeriod, 5*time.Second, "a released Lease is taken over within seconds")
	assert.False(t, opts.LeaderElectionReleaseOnCancel, "client-go's release after a lost term could outlast the Lease")
}
