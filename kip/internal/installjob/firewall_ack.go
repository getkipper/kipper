package installjob

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// FirewallCheckMarker starts the log line a job prints when it waits for the
// driver to prove that new SSH connections still reach the server.
const FirewallCheckMarker = "--- kipper firewall check"

// FirewallCheckLine matches a complete marker line and captures the run ID and nonce.
var FirewallCheckLine = regexp.MustCompile(`^` + regexp.QuoteMeta(FirewallCheckMarker) + ` run=([a-z0-9][a-z0-9-]{0,62}) nonce=([0-9a-f]{32})$`)

var noncePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// firewallAckPrefix names one acknowledgement file per nonce, so an answer to
// an earlier check never replaces the answer to the current one.
const firewallAckPrefix = "firewall-ack-"

// firewallCheckRepeat allows clients to receive a complete marker after
// reconnecting mid-line or buffering the end of the log stream.
const firewallCheckRepeat = 10 * time.Second

// AwaitFirewallAck writes a fresh nonce to w and waits for the driver to
// acknowledge it over a new SSH connection by writing a file in dir.
func AwaitFirewallAck(dir, runID string, w io.Writer, timeout, poll time.Duration) error {
	return awaitFirewallAck(dir, runID, w, timeout, poll, firewallCheckRepeat)
}

func awaitFirewallAck(dir, runID string, w io.Writer, timeout, poll, repeat time.Duration) error {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("creating the firewall check: %w", err)
	}
	nonce := hex.EncodeToString(b)
	ackPath := filepath.Join(dir, firewallAckPrefix+nonce)
	old, _ := filepath.Glob(filepath.Join(dir, firewallAckPrefix+"*"))
	for _, f := range old {
		_ = os.Remove(f)
	}

	if _, err := fmt.Fprintf(w, "  Waiting for kip on your machine to open a new SSH connection...\n"); err != nil {
		return fmt.Errorf("asking for the firewall check: %w", err)
	}
	deadline := time.Now().Add(timeout)
	var asked time.Time
	for {
		if time.Since(asked) >= repeat {
			if _, err := fmt.Fprintf(w, "%s run=%s nonce=%s\n", FirewallCheckMarker, runID, nonce); err != nil {
				return fmt.Errorf("asking for the firewall check: %w", err)
			}
			asked = time.Now()
		}
		got, err := os.ReadFile(ackPath) //nolint:gosec // a fixed name under the root-only jobs directory
		if err == nil && strings.TrimSpace(string(got)) == nonce {
			// Require matching contents as well as a matching filename.
			_ = os.Remove(ackPath)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("kip on your machine did not confirm a new SSH connection within %s", timeout)
		}
		time.Sleep(poll)
	}
}

// AckFirewall records the driver's acknowledgement of nonce for runID.
func AckFirewall(root, runID, nonce string) error {
	if err := CheckRunID(runID); err != nil {
		return err
	}
	if !noncePattern.MatchString(nonce) {
		return fmt.Errorf("%q is not a firewall check nonce", nonce)
	}
	dir := filepath.Join(root, runID)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("no install job %s: %w", runID, err)
	}
	return writeAtomic(filepath.Join(dir, firewallAckPrefix+nonce), []byte(nonce+"\n"))
}
