package deliver_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Four places used to implement "deliver a thing with a deadline", and the
// consequence was that a fix applied to one of them was not a fix: a per-send
// budget added to the alert batch was absent from the two equivalent loops
// beside it, and an end-of-day review found a fourth send that had been left
// out of the primitive entirely.
//
// So this walks the source. Every call that puts a message on the network has
// to sit inside deliver.Bounded, and a new one that does not will fail here
// rather than in production a year from now.
func TestEveryOutboundSendGoesThroughThePrimitive(t *testing.T) {
	// The transports themselves, which Bounded wraps rather than replaces:
	// each one defines a send rather than performing one on its own account.
	transports := map[string]bool{
		filepath.Join("mail", "mail.go"):      true, // defines mail.Send
		filepath.Join("handlers", "slack.go"): true, // defines SendSlackAlert
		filepath.Join("handlers", "email.go"): true, // the thin wrapper over mail.Send
	}

	sends := regexp.MustCompile(`\b(mail\.Send|SendSlackAlert)\(`)

	// The module root, two levels up from internal/deliver.
	root := filepath.Join("..", "..")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}

		rel, relErr := filepath.Rel(root, path)
		require.NoError(t, relErr)
		if transports[rel] {
			return nil
		}

		body, readErr := os.ReadFile(path) //nolint:gosec // walking this module's own source
		require.NoError(t, readErr)
		if !sends.Match(body) {
			return nil
		}
		if !strings.Contains(string(body), "deliver.Bounded(") {
			t.Errorf("%s puts a message on the network without going through deliver.Bounded; "+
				"it will not get the deadline, the detachment or the panic containment every other send has", rel)
		}
		return nil
	})
	require.NoError(t, err)
}
