package security

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The channels are independent, and one of them being broken is the reason the
// others exist. Sharing one deadline across them serially meant a mail relay
// that accepted the connection and then went quiet could consume the whole
// budget, so the console bell and a perfectly healthy Slack webhook were both
// handed an expired context and delivered nothing.
func TestOneSlowChannelDoesNotStarveTheRest(t *testing.T) {
	reached := make(chan time.Duration, 1)

	n := &Notifier{
		deliveryTimeout: testBudget,
		envSMTP: func(ctx context.Context, _ string, _ Event) {
			// A relay that takes the whole of its own budget.
			<-ctx.Done()
		},
		Console: ConsoleHooks{
			Alert: func(ctx context.Context, _, _ string) {
				deadline, ok := ctx.Deadline()
				require.True(t, ok, "the console delivery needs a bound of its own")
				reached <- time.Until(deadline)
			},
		},
	}

	n.Emit(context.Background(), Event{Kind: "test", Summary: "a security event"})

	select {
	case left := <-reached:
		assert.Greater(t, left, testBudget/2,
			"the console channel was handed a budget already spent by the mail relay")
	case <-time.After(10 * time.Second):
		t.Fatal("the console channel was never reached at all")
	}
}

// The channels inside a group are independent too. The bell writing to the API
// server is not a reason for a healthy Slack webhook or a working mail server
// to hear nothing, and a security event reaching only the process log is the
// state this whole path exists to avoid.
func TestASlowBellDoesNotStarveTheOtherConsoleChannels(t *testing.T) {
	emailed := make(chan time.Duration, 4)

	n := &Notifier{deliveryTimeout: testBudget, Console: ConsoleHooks{
		Alert: func(ctx context.Context, _, _ string) {
			// The API server stalls on the ConfigMap write.
			<-ctx.Done()
		},
		Admins: func() []string { return []string{"ops@example.com"} },
		Email: func(ctx context.Context, _, _, _ string) error {
			deadline, ok := ctx.Deadline()
			require.True(t, ok, "the email channel needs a bound of its own")
			emailed <- time.Until(deadline)
			return nil
		},
	}}

	n.Emit(context.Background(), Event{Kind: "test", Summary: "a security event"})

	select {
	case left := <-emailed:
		assert.Greater(t, left, testBudget/2,
			"the mail server was handed a budget the bell had already spent")
	case <-time.After(10 * time.Second):
		t.Fatal("a working mail server was never reached because the bell was slow")
	}
}

// The env-pinned channels are two, and one being broken is not a reason for the
// other to go unused.
func TestTheEnvPinnedChannelsAreIndependent(t *testing.T) {
	posted := make(chan time.Duration, 2)
	n := &Notifier{
		deliveryTimeout: testBudget,
		envSMTP:         func(ctx context.Context, _ string, _ Event) { <-ctx.Done() },
		envWebhook:      func(ctx context.Context, _ string, _ Event) { posted <- budgetLeft(ctx) },
	}

	n.Emit(context.Background(), Event{Kind: "test", Summary: "a security event"})

	select {
	case left := <-posted:
		assert.Greater(t, left, testBudget/2,
			"the webhook was handed a budget the mail relay had already spent")
	case <-time.After(10 * time.Second):
		t.Fatal("the webhook was never reached")
	}
}

func budgetLeft(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	return time.Until(deadline)
}

// testBudget is short enough that a channel taking all of its own does not slow
// the suite, and long enough that the assertions have room.
const testBudget = 400 * time.Millisecond

// These channels are best-effort background work, and every one of them calls a
// hook wired from another package. A nil map, a nil pointer, anything in one of
// those takes down a goroutine with no caller to recover it, which means the
// whole console-api process. A security notification failing must not be able to
// stop the thing it is notifying about.
func TestAPanickingChannelDoesNotTakeTheProcessWithIt(t *testing.T) {
	delivered := make(chan struct{}, 1)

	n := &Notifier{
		deliveryTimeout: testBudget,
		envSMTP:         func(context.Context, string, Event) { panic("a hook exploded") },
		envWebhook:      func(context.Context, string, Event) {},
		Console: ConsoleHooks{
			Alert: func(context.Context, string, string) { delivered <- struct{}{} },
		},
	}

	assert.NotPanics(t, func() {
		n.Emit(context.Background(), Event{Kind: "test", Summary: "a security event"})
	})

	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("one channel panicking stopped the others")
	}
}

// The webhook is a network call like the others and has to observe the budget
// its channel was given.
func TestTheEnvWebhookObservesItsBudget(t *testing.T) {
	seen := make(chan bool, 1)
	n := &Notifier{
		deliveryTimeout: testBudget,
		envSMTP:         func(context.Context, string, Event) {},
		postWebhook: func(ctx context.Context, _ string, _ Event) error {
			_, ok := ctx.Deadline()
			seen <- ok
			return nil
		},
	}
	t.Setenv("KIPPER_SECURITY_WEBHOOK", "https://hooks.example.com/abc")

	n.Emit(context.Background(), Event{Kind: "test", Summary: "a security event"})

	select {
	case ok := <-seen:
		assert.True(t, ok, "the webhook was handed a context with no deadline on it")
	case <-time.After(5 * time.Second):
		t.Fatal("the webhook was never called")
	}
}

// A channel's budget bounds the channel; it cannot also bound each send inside
// it. Two recipients sharing one deadline means the first relay that stalls
// spends it, and the second address fails its dial instantly — which is the
// starvation the channel split was supposed to remove, one level down, on the
// deliveries that matter most.
func TestEachSecurityRecipientGetsItsOwnBudget(t *testing.T) {
	seen := make(chan time.Duration, 4)

	n := &Notifier{
		deliveryTimeout: testBudget,
		perSendTimeout:  testBudget,
		envSMTP:         func(context.Context, string, Event) {},
		envWebhook:      func(context.Context, string, Event) {},
		Console: ConsoleHooks{
			Admins: func() []string { return []string{"first@example.com", "second@example.com"} },
			Email: func(ctx context.Context, to, _, _ string) error {
				if to == "first@example.com" {
					<-ctx.Done()
					return ctx.Err()
				}
				seen <- budgetLeft(ctx)
				return nil
			},
		},
	}

	n.Emit(context.Background(), Event{Kind: "test", Summary: "a security event"})

	// Both addresses are tried, and whichever is second still has time. The map
	// the admins come from has no stable order, so this waits for the one that
	// was not the stalled address.
	select {
	case left := <-seen:
		assert.Greater(t, left, testBudget/2,
			"the second recipient was handed a budget the first had spent")
	case <-time.After(10 * time.Second):
		t.Fatal("the second recipient was never reached")
	}
}

// Containment is per send now, not per channel. A transport that panics on one
// address used to abort the whole channel and take every later recipient with
// it; the recipients after it are as entitled to hear about the event as the
// one that broke.
func TestAPanickingRecipientDoesNotStopTheNext(t *testing.T) {
	reached := make(chan string, 4)

	n := &Notifier{
		deliveryTimeout: testBudget,
		perSendTimeout:  testBudget,
		envSMTP:         func(context.Context, string, Event) {},
		envWebhook:      func(context.Context, string, Event) {},
		Console: ConsoleHooks{
			Admins: func() []string { return []string{"first@example.com", "second@example.com"} },
			Email: func(_ context.Context, to, _, _ string) error {
				if to == "first@example.com" {
					panic("the transport exploded")
				}
				reached <- to
				return nil
			},
		},
	}

	assert.NotPanics(t, func() {
		n.Emit(context.Background(), Event{Kind: "test", Summary: "a security event"})
	})

	select {
	case to := <-reached:
		assert.Equal(t, "second@example.com", to)
	case <-time.After(5 * time.Second):
		t.Fatal("a panic on the first address stopped the second")
	}
}

// The env-pinned webhook is Slack-compatible and renders <url|label> as a link.
// A security event's summary and fields carry text from elsewhere — a peer's
// self-reported cluster name, a user string — so the ordinary alert path escapes
// them. This path had not, which puts attacker-chosen markup inside a message
// presented as a Kipper security event, in the channel meant to survive a
// compromised admin account.
func TestTheSecurityWebhookEscapesWhatItDidNotWrite(t *testing.T) {
	text := webhookText(Event{
		Summary: `migration from <https://evil.example.com|a trusted cluster> & more`,
		User:    "<!channel>",
		Fields:  []Field{{Key: "peer", Value: "<https://evil.example.com|prod>"}},
	})

	assert.NotContains(t, text, "<https://", "the link markup reached the channel")
	assert.NotContains(t, text, "<!channel>", "so did the mention")
	for _, want := range []string{"&lt;https://", "&amp;", "&lt;!channel&gt;"} {
		assert.Contains(t, text, want, "expected %s in the escaped message", want)
	}
}

// Kipper's own formatting still has to work.
func TestTheSecurityWebhookKeepsItsOwnFormatting(t *testing.T) {
	text := webhookText(Event{Summary: "a git credential was revoked"})
	assert.Contains(t, text, "*Kipper security*")
	assert.Contains(t, text, "a git credential was revoked")
}

// The primitive keeps the caller's values while dropping its cancellation, so
// request-scoped logging and tracing follow a delivery that outlives the
// request. Handing it a bare background context throws that away at the one
// call site that had any values to keep.
func TestASendKeepsTheChannelsValues(t *testing.T) {
	type key struct{}
	seen := make(chan any, 1)

	n := &Notifier{
		deliveryTimeout: testBudget,
		perSendTimeout:  testBudget,
		envSMTP:         func(context.Context, string, Event) {},
		envWebhook:      func(context.Context, string, Event) {},
		Console: ConsoleHooks{
			Admins: func() []string { return []string{"ops@example.com"} },
			Email: func(ctx context.Context, _, _, _ string) error {
				seen <- ctx.Value(key{})
				return nil
			},
		},
	}

	n.Emit(context.WithValue(context.Background(), key{}, "from the request"), Event{Kind: "test", Summary: "x"})

	select {
	case got := <-seen:
		assert.Equal(t, "from the request", got,
			"the send was handed a context with none of the caller's values")
	case <-time.After(5 * time.Second):
		t.Fatal("the send was never reached")
	}
}
