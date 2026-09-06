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
