// Package deliver gives outbound sends a detached deadline and panic recovery.
// Send functions must honor the context to stay within the budget.
package deliver

import (
	"context"
	"log"
	"time"
)

// DefaultBudget is what one send gets when the caller names no other.
const DefaultBudget = 20 * time.Second

// Bounded calls send with an independent timeout, retaining parent values
// while detaching cancellation so delivery can outlive the triggering request.
// It logs and recovers panics. send must honor context cancellation for the
// time budget to bound its execution.
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
