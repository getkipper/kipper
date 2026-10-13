package installjob

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFirewallAckRoundTrip(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "run-1")
	require.NoError(t, os.Mkdir(dir, 0o700))

	var log syncBuffer
	done := make(chan error, 1)
	go func() { done <- AwaitFirewallAck(dir, "run-1", &log, 5*time.Second, 10*time.Millisecond) }()

	var nonce string
	require.Eventually(t, func() bool {
		for _, line := range strings.Split(log.String(), "\n") {
			if m := FirewallCheckLine.FindStringSubmatch(line); m != nil {
				nonce = m[2]
				return m[1] == "run-1"
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond)

	require.NoError(t, AckFirewall(root, "run-1", strings.Repeat("0", 32)))
	select {
	case err := <-done:
		t.Fatalf("an acknowledgement of another nonce ended the wait: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(t, AckFirewall(root, "run-1", nonce))
	require.NoError(t, <-done)
}

func TestAwaitFirewallAckTimesOut(t *testing.T) {
	err := AwaitFirewallAck(t.TempDir(), "run-1", &bytes.Buffer{}, 50*time.Millisecond, 10*time.Millisecond)
	assert.ErrorContains(t, err, "did not confirm a new SSH connection")
}

func TestAwaitFirewallAckIgnoresAnEarlierAcknowledgement(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, firewallAckPrefix+strings.Repeat("e", 32)), []byte(strings.Repeat("e", 32)+"\n"), 0o600))

	err := AwaitFirewallAck(dir, "run-1", &bytes.Buffer{}, 50*time.Millisecond, 10*time.Millisecond)

	assert.Error(t, err)
}

func TestAckFirewallValidatesItsInput(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "run-1"), 0o700))

	assert.Error(t, AckFirewall(root, "../etc", strings.Repeat("a", 32)))
	assert.Error(t, AckFirewall(root, "run-1", "not-a-nonce"))
	assert.Error(t, AckFirewall(root, "run-2", strings.Repeat("a", 32)), "no such job")
	assert.NoError(t, AckFirewall(root, "run-1", strings.Repeat("a", 32)))
}

func TestFirewallCheckLineIsExact(t *testing.T) {
	ok := FirewallCheckMarker + " run=run-1 nonce=" + strings.Repeat("a", 32)
	assert.NotNil(t, FirewallCheckLine.FindStringSubmatch(ok))
	for _, bad := range []string{
		ok + "; rm -rf /",
		FirewallCheckMarker + " run=run-1 nonce=" + strings.Repeat("a", 31),
		"  " + ok,
	} {
		assert.Nil(t, FirewallCheckLine.FindStringSubmatch(bad), bad)
	}
}

// syncBuffer synchronizes log writes with reads from the test goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestAwaitFirewallAckRepeatsItsMarker(t *testing.T) {
	var log syncBuffer
	_ = awaitFirewallAck(t.TempDir(), "run-1", &log, 200*time.Millisecond, 5*time.Millisecond, 40*time.Millisecond)

	markers := strings.Count(log.String(), FirewallCheckMarker+" run=run-1 nonce=")
	assert.GreaterOrEqual(t, markers, 3)
	assert.Equal(t, 1, strings.Count(log.String(), "Waiting for kip"), "the explanation is printed once")
}

// Reconnecting clients may replay an earlier check after the current one is answered.
func TestAStaleAcknowledgementLeavesTheCurrentOneAlone(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "run-1")
	require.NoError(t, os.Mkdir(dir, 0o700))
	var log syncBuffer
	done := make(chan error, 1)
	go func() { done <- AwaitFirewallAck(dir, "run-1", &log, 5*time.Second, 50*time.Millisecond) }()
	var nonce string
	require.Eventually(t, func() bool {
		for _, line := range strings.Split(log.String(), "\n") {
			if m := FirewallCheckLine.FindStringSubmatch(line); m != nil {
				nonce = m[2]
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond)

	require.NoError(t, AckFirewall(root, "run-1", nonce))
	require.NoError(t, AckFirewall(root, "run-1", strings.Repeat("0", 32)))

	require.NoError(t, <-done)
}
