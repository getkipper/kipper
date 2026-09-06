package deliver_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
	// Where a send is defined or handed to something else to perform, rather
	// than performed here. Keyed by function as well as file, so a different
	// function in the same file is not excused along with it.
	definitions := map[string]map[string]bool{
		filepath.Join("mail", "mail.go"):               {"Send": true},
		filepath.Join("handlers", "slack.go"):          {"SendSlackAlert": true},
		filepath.Join("handlers", "email.go"):          {"Send": true},
		filepath.Join("handlers", "alert_delivery.go"): {"deliveryFor": true},
	}

	sends := regexp.MustCompile(`\b(mail\.Send|SendSlackAlert)\(`)

	// The primitive, or one of the two thin helpers whose only job is to call
	// it. Those two are checked below, so recognising them here is not a hole.
	wrappers := regexp.MustCompile(`(deliver\.Bounded|\.bounded|\.perSend)\(`)

	root := filepath.Join("..", "..")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}

		rel, relErr := filepath.Rel(root, path)
		require.NoError(t, relErr)

		body, readErr := os.ReadFile(path) //nolint:gosec // walking this module's own source
		require.NoError(t, readErr)

		// Per call site, not per file. Checking only that the file mentions
		// Bounded somewhere lets a second, unwrapped send hide beside a wrapped
		// one — and excluding a whole file because it also defines a transport
		// hid exactly that.
		for _, line := range enclosingLines(string(body), sends) {
			if wrappers.MatchString(line.enclosing) {
				continue
			}
			if definitions[rel][line.fn] {
				// The definition, or the closure a bounded caller invokes.
				continue
			}
			t.Errorf("%s:%d puts a message on the network outside deliver.Bounded, so it gets none of "+
				"the deadline, the detachment or the panic containment every other send has:\n    %s",
				rel, line.number, strings.TrimSpace(line.text))
		}
		return nil
	})
	require.NoError(t, err)
}

// sendSite is one call to a transport, with the function it sits in and the
// block around it.
type sendSite struct {
	number    int
	text      string
	fn        string
	enclosing string
}

// enclosingLines finds each send and returns it with the function it is in and
// the twelve lines above it, which is where a wrapping deliver.Bounded would be.
func enclosingLines(body string, sends *regexp.Regexp) []sendSite {
	lines := strings.Split(body, "\n")
	fnAt := regexp.MustCompile(`^func (?:\([^)]*\) )?(\w+)`)

	var sites []sendSite
	fn := ""
	for i, line := range lines {
		if m := fnAt.FindStringSubmatch(line); m != nil {
			fn = m[1]
		}
		if !sends.MatchString(line) {
			continue
		}
		from := i - 12
		if from < 0 {
			from = 0
		}
		sites = append(sites, sendSite{
			number:    i + 1,
			text:      line,
			fn:        fn,
			enclosing: strings.Join(lines[from:i+1], "\n"),
		})
	}
	return sites
}

// The two helpers the check above accepts in place of the primitive exist only
// to call it. If one ever stops, the check would be accepting a send that gets
// none of what it promises.
func TestTheAcceptedWrappersCallThePrimitive(t *testing.T) {
	for _, w := range []struct{ file, fn string }{
		{filepath.Join("..", "..", "handlers", "alert_delivery.go"), "bounded"},
		{filepath.Join("..", "..", "security", "security.go"), "perSend"},
	} {
		body, err := os.ReadFile(w.file) //nolint:gosec // this module's own source
		require.NoError(t, err)

		fn := regexp.MustCompile(`(?s)func \([^)]*\) ` + w.fn + `\(.*?\n\}`).FindString(string(body))
		require.NotEmpty(t, fn, "%s no longer defines %s; the send check accepts a wrapper that is gone", w.file, w.fn)
		assert.Contains(t, fn, "deliver.Bounded(",
			"%s stopped calling the primitive, so every send it wraps quietly lost its deadline", w.fn)
	}
}
