package installer

import (
	"fmt"
	"time"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// nodeReadyQuery blocks until every node reports Ready. Its own timeout is
// short because waitForNodeReady owns the overall deadline: the command has to
// return so the poll can retry while the node is still absent.
const nodeReadyQuery = "kubectl wait --for=condition=Ready node --all --timeout=30s"

// WaitForNodeReady waits for k3s to register a node and report it Ready.
func WaitForNodeReady(client *ssh.Client, timeout time.Duration) error {
	return waitForNodeReady(client.Run, timeout, 3*time.Second)
}

// waitForNodeReady is the pollable core, split out so the poll behaviour is
// testable without a live SSH host.
//
// `kubectl wait --all` treats an empty match as an error rather than something
// to wait for, so it returns "no matching resources found" the moment it runs
// against a k3s that has not registered its node yet. Passing a longer
// --timeout does not help, which is why this retries instead of asking once.
func waitForNodeReady(run func(command string) (string, error), timeout, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if _, err := run(nodeReadyQuery); err == nil {
			return nil
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("k3s node did not become ready within %s: %w", timeout, last)
		}
		time.Sleep(interval)
	}
}
