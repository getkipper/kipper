package deliver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every outbound send in this service needs the same three things around it: a
// deadline of its own, detachment from a caller that has already been answered,
// and containment if it panics. They were written out separately in each place
// that sends, which is why a fix applied to one send loop was repeatedly not a
// fix for the equivalent ones.

func TestBoundedGivesEachSendItsOwnDeadline(t *testing.T) {
	var first, second time.Duration

	Bounded(context.Background(), 300*time.Millisecond, "first", func(ctx context.Context) {
		first = budgetOf(t, ctx)
		<-ctx.Done() // spends all of it
	})
	Bounded(context.Background(), 300*time.Millisecond, "second", func(ctx context.Context) {
		second = budgetOf(t, ctx)
	})

	assert.Greater(t, first, 200*time.Millisecond)
	assert.Greater(t, second, 200*time.Millisecond,
		"the second send was handed a budget the first had already spent")
}

// The caller has usually been answered already, so its cancellation must not
// reach the send. Its values must, because that is where request-scoped logging
// and tracing live.
func TestBoundedDetachesCancellationButKeepsValues(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "kept"))
	cancel()

	var ran bool
	Bounded(parent, time.Second, "send", func(ctx context.Context) {
		ran = true
		assert.NoError(t, ctx.Err(), "a cancelled caller must not cancel the send")
		assert.Equal(t, "kept", ctx.Value(key{}), "the caller's values belong to the send")
	})

	assert.True(t, ran, "the send never ran")
}

// These run on background goroutines with no caller to recover them, so a panic
// in one takes the process down. Delivery is best-effort; the thing it is
// reporting on is not.
func TestBoundedContainsAPanic(t *testing.T) {
	assert.NotPanics(t, func() {
		Bounded(context.Background(), time.Second, "exploding send", func(context.Context) {
			panic("the transport exploded")
		})
	})
}

// A panic in one send must not stop the next.
func TestBoundedCarriesOnAfterAPanic(t *testing.T) {
	var reached bool

	Bounded(context.Background(), time.Second, "first", func(context.Context) { panic("boom") })
	Bounded(context.Background(), time.Second, "second", func(context.Context) { reached = true })

	assert.True(t, reached)
}

// A budget nobody set is the default rather than an instantly-expired context.
func TestBoundedFallsBackToADefaultBudget(t *testing.T) {
	for _, budget := range []time.Duration{0, -time.Second} {
		Bounded(context.Background(), budget, "send", func(ctx context.Context) {
			assert.Greater(t, budgetOf(t, ctx), DefaultBudget/2,
				"budget %v should have fallen back to the default", budget)
		})
	}
}

func TestDefaultBudgetIsTheOneTheAlertPathUsed(t *testing.T) {
	assert.Equal(t, 20*time.Second, DefaultBudget)
}

// The send's own error is the caller's business, not this package's.
func TestBoundedDoesNotSwallowTheSendsResult(t *testing.T) {
	want := errors.New("relay refused")
	var got error

	Bounded(context.Background(), time.Second, "send", func(context.Context) { got = want })

	require.ErrorIs(t, got, want)
}

func budgetOf(t *testing.T, ctx context.Context) time.Duration {
	t.Helper()
	deadline, ok := ctx.Deadline()
	require.True(t, ok, "the send was given no deadline")
	return time.Until(deadline)
}
