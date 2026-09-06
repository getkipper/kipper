// Package deliver bounds one outbound delivery.
//
// Alerts leave this cluster by several routes — a Slack webhook, an SMTP relay
// for each admin, an env-pinned channel — and every one of them needs the same
// three things: a deadline of its own, detachment from a caller that has
// already been answered, and containment if it panics.
//
// Those three were written out separately in each place that sends, and the
// consequence was not hypothetical: a per-send deadline added to the alert
// batch was not added to the two equivalent loops beside it, so a relay that
// accepted the connection and then went quiet still spent the whole budget and
// every address after it failed its dial instantly. Applying the rules here
// means a change to them lands everywhere at once.
package deliver

import (
	"context"
	"log"
	"time"
)

// DefaultBudget is what one send gets when the caller names no other.
const DefaultBudget = 20 * time.Second

// Bounded runs one send.
//
// The deadline is the send's own, not a slice of some larger one: a budget
// shared across serial sends belongs to whichever of them stalls first, and the
// rest inherit an expired context. A caller wanting to bound a whole batch has
// to do it by other means, which is what running the routes on separate
// goroutines is for.
//
// The caller's cancellation is dropped and its values are kept. Delivery
// outlives the request that triggered it, so an admin closing a browser tab
// must not stop a security notification, while request-scoped logging and
// tracing should still follow it.
//
// A panic is logged and contained. These run on background goroutines with
// nobody above them to recover, so a nil map in a transport would otherwise
// take the process down — and delivery failing must never be able to stop the
// thing it was reporting on.
func Bounded(parent context.Context, budget time.Duration, label string, send func(context.Context)) {
	if budget <= 0 {
		budget = DefaultBudget
	}

	defer func() {
		if r := recover(); r != nil {
			log.Printf("delivery: %s panicked: %v", label, r)
		}
	}()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), budget)
	defer cancel()
	send(ctx)
}
