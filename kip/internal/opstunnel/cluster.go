package opstunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
)

// ClusterPaths creates secure runtime directories and returns the cluster's
// tunnel paths via RuntimePaths, using the current user's home directory.
// Long socket paths use the temporary directory. Home lookup and directory
// preparation errors are returned.
func ClusterPaths(cluster string) (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolving home directory: %w", err)
	}
	return RuntimePaths(home, cluster)
}

// EnsureCluster returns the socket of cluster's tunnel process, starting
// this kip binary's 'cluster access connect' in the background when none
// runs.
func EnsureCluster(ctx context.Context, cluster string) (string, error) {
	p, err := ClusterPaths(cluster)
	if err != nil {
		return "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("finding kip's own binary: %w", err)
	}
	start := func() error {
		return StartDetached(exe, []string{"--cluster", cluster, "cluster", "access", "connect"}, p.Log)
	}
	if err := Ensure(ctx, p, start); err != nil {
		return "", fmt.Errorf("%w (log: %s)", err, p.Log)
	}
	return p.Socket, nil
}

// DialCluster opens a stream to port on the node with machineID through
// cluster's tunnel process, starting the process when needed. A process that
// reports ErrDeviceChanged triggers one retry through EnsureCluster. Startup
// and dial errors are returned. The caller owns the returned connection.
func DialCluster(ctx context.Context, cluster, machineID string, port int) (net.Conn, error) {
	for attempt := 0; ; attempt++ {
		socket, err := EnsureCluster(ctx, cluster)
		if err != nil {
			return nil, err
		}
		conn, err := DialVia(ctx, socket, machineID, port)
		if err == nil || attempt > 0 || !errors.Is(err, ErrDeviceChanged) {
			return conn, err
		}
	}
}
