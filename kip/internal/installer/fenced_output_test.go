package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testFenceScript = "exec 9>/run/kipper-node-ops.lock; flock -n 9 || exit 75; k3s token create"

// fencedServer fails every command the way the SSH client does under the node
// lease: the error quotes the fence script, carries the command's output and
// wraps cause, an exit status 255 by default.
type fencedServer struct {
	out   string
	cause error
}

func (s fencedServer) Run(command string) (string, error) {
	cause := s.cause
	if cause == nil {
		cause = exitCodeError{code: 255}
	}
	sshErr := fmt.Errorf("running %q on 203.0.113.10: %w: %s", testFenceScript, cause, s.out)
	return fencedResult(s.out, sshErr, testFenceScript, command)
}

func (s fencedServer) RunStdin(command string, _ io.Reader) (string, error) { return s.Run(command) }

func TestWithoutOutputDropsTheOutputAndFenceOfALeasedCommand(t *testing.T) {
	_, err := fencedServer{out: testJoinToken}.Run(createJoinTokenCmd)
	require.Error(t, err)

	got := withoutOutput(err)

	assert.NotContains(t, got.Error(), "secretsecret0000")
	assert.NotContains(t, got.Error(), "flock")
	var exit interface{ ExitCode() int }
	assert.True(t, errors.As(got, &exit), "the exit status stays reachable")
	var leased interface{ ExitCode() int }
	assert.True(t, errors.As(err, &leased), "the leased error still matches the exit status")
}

func TestAJoinTokenMintedOverADroppedLeasedConnectionStaysOutOfTheError(t *testing.T) {
	err := withJoinToken(fencedServer{out: testJoinToken + "\n"}, "203.0.113.10", func(string) error { return nil })

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secretsecret0000")
	assert.NotContains(t, err.Error(), "flock")
	assert.Contains(t, err.Error(), "exit status 255")
}

func TestFetchKubeconfigOverTheLeaseKeepsTheFileOutOfItsErrors(t *testing.T) {
	_, err := FetchKubeconfig(fencedServer{out: "client-key-data: not-a-real-key"}, "203.0.113.10")

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "not-a-real-key")
	assert.NotContains(t, err.Error(), "flock")
}

// leaseLostServer is a leased runner whose lease ended before the command ran.
type leaseLostServer struct{}

func (leaseLostServer) Run(string) (string, error)                 { return "", ErrNodeLeaseLost }
func (leaseLostServer) RunStdin(string, io.Reader) (string, error) { return "", ErrNodeLeaseLost }

func TestFetchKubeconfigOverTheLeaseKeepsItsCauseMatchable(t *testing.T) {
	timedOut := fmt.Errorf("ssh command did not finish within 1s: %w", errors.Join(exitCodeError{code: -1}, context.DeadlineExceeded))
	_, err := FetchKubeconfig(fencedServer{out: "client-key-data: not-a-real-key", cause: timedOut}, "203.0.113.10")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	_, err = FetchKubeconfig(fencedServer{out: "client-key-data: not-a-real-key"}, "203.0.113.10")
	var exit interface{ ExitCode() int }
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 255, exit.ExitCode())

	_, err = FetchKubeconfig(leaseLostServer{}, "203.0.113.10")
	assert.ErrorIs(t, err, ErrNodeLeaseLost)
	assert.Contains(t, err.Error(), "no longer holds the node lifecycle lease", "the refusal keeps its explanation")
}

func TestAJoinTokenRefusedByTheLeaseSaysSo(t *testing.T) {
	err := withJoinToken(leaseLostServer{}, "203.0.113.10", func(string) error { return nil })
	assert.ErrorIs(t, err, ErrNodeLeaseLost)
}

func TestWithoutOutputKeepsTheBusyHostRefusal(t *testing.T) {
	_, err := fencedResult(leaseHostBusy, errors.New("exit status 75"), testFenceScript, createJoinTokenCmd)
	require.Error(t, err)
	assert.Equal(t, err.Error(), withoutOutput(err).Error())
}
