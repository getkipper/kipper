package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/installjob"
)

func TestFirewallAckWriterAnswersTheJobsCheck(t *testing.T) {
	var out bytes.Buffer
	var acked []string
	w := &firewallAckWriter{out: &out, runID: "run-1", ack: func(n string) error { acked = append(acked, n); return nil }}
	nonce := strings.Repeat("ab", 16)
	stream := "  ✔  Installing k3s\n" + installjob.FirewallCheckMarker + " run=run-1 nonce=" + nonce + "\n  ✔  Done\n"

	// One byte at a time, so a marker split across reads is still found.
	for i := 0; i < len(stream); i++ {
		_, err := w.Write([]byte{stream[i]})
		require.NoError(t, err)
	}
	require.NoError(t, w.Flush())

	assert.Equal(t, []string{nonce}, acked)
	assert.Equal(t, "  ✔  Installing k3s\n  ✔  Done\n", out.String(), "the marker itself is not shown")
}

func TestFirewallAckWriterIgnoresOtherMarkers(t *testing.T) {
	var out bytes.Buffer
	w := &firewallAckWriter{out: &out, runID: "run-1", ack: func(string) error { t.Fatal("acknowledged a marker it should not have"); return nil }}
	other := installjob.FirewallCheckMarker + " run=run-2 nonce=" + strings.Repeat("a", 32) + "\n"
	trailing := installjob.FirewallCheckMarker + " run=run-1 nonce=" + strings.Repeat("a", 32) + " x\n"

	_, err := w.Write([]byte(other + trailing))

	require.NoError(t, err)
	assert.Equal(t, other+trailing, out.String())
}

func TestFirewallAckWriterShowsPartialLinesStraightAway(t *testing.T) {
	var out bytes.Buffer
	w := &firewallAckWriter{out: &out, runID: "run-1", ack: func(string) error { return nil }}

	_, err := w.Write([]byte("  Installing k3s..."))

	require.NoError(t, err)
	assert.Equal(t, "  Installing k3s...", out.String())
}

func TestFirewallAckWriterReportsAFailedAcknowledgement(t *testing.T) {
	var out bytes.Buffer
	w := &firewallAckWriter{out: &out, runID: "run-1", ack: func(string) error { return errors.New("connection refused") }}

	_, err := w.Write([]byte(installjob.FirewallCheckMarker + " run=run-1 nonce=" + strings.Repeat("a", 32) + "\n"))

	require.NoError(t, err)
	assert.Contains(t, out.String(), "could not confirm a new SSH connection: connection refused")
}

func TestJobOptionsAskTheDriverToConfirmConnections(t *testing.T) {
	opts := jobOptions(installjob.Request{Version: installjob.RequestVersion, RunID: "run-1", Host: "203.0.113.10", AdoptFirewall: true}, t.TempDir(), &bytes.Buffer{})

	assert.NotNil(t, opts.ConfirmConnection)
	assert.True(t, opts.AdoptFirewall)
	assert.True(t, opts.Local)
}

func TestAdoptFirewallFlagExists(t *testing.T) {
	for _, c := range []*cobra.Command{installCmd, clusterHardenCmd} {
		f := c.Flags().Lookup("adopt-firewall")
		require.NotNil(t, f, c.Name())
		assert.Equal(t, "false", f.DefValue, "adoption is never the default")
	}
}

// Repeating the marker flushes the first one through endWriter's tail buffer.
func TestFirewallAckThroughTheEndWriter(t *testing.T) {
	var out bytes.Buffer
	var acked []string
	acks := &firewallAckWriter{out: &out, runID: "run-1", ack: func(n string) error { acked = append(acked, n); return nil }}
	ends := &endWriter{out: acks}
	marker := installjob.FirewallCheckMarker + " run=run-1 nonce=" + strings.Repeat("c", 32) + "\n"

	_, err := ends.Write([]byte("  Waiting for kip on your machine to open a new SSH connection...\n" + marker))
	require.NoError(t, err)
	_, err = ends.Write([]byte(marker))
	require.NoError(t, err)
	_, err = ends.Write([]byte(marker))
	require.NoError(t, err)

	assert.Equal(t, []string{strings.Repeat("c", 32)}, acked, "answered as soon as a marker got through, and only once")
}

func TestFirewallAckWriterOnlyAnswersAtALineStart(t *testing.T) {
	var out bytes.Buffer
	w := &firewallAckWriter{out: &out, runID: "run-1", ack: func(string) error { t.Fatal("answered a marker in the middle of a line"); return nil }}
	marker := installjob.FirewallCheckMarker + " run=run-1 nonce=" + strings.Repeat("a", 32) + "\n"

	_, err := w.Write([]byte("diagnostic: "))
	require.NoError(t, err)
	_, err = w.Write([]byte(marker))
	require.NoError(t, err)

	assert.Equal(t, "diagnostic: "+marker, out.String())
}
