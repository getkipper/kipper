package cmd

import (
	"fmt"
	"time"
)

// humanAge renders how long before now a moment was, as "2d", "3h", "5m" or
// "12s". A zero time reads "-". A moment in the future reads "0s", because a
// clock running slightly ahead says nothing worth printing as a negative age.
// The result is approximate, sized for a table column.
func humanAge(now, then time.Time) string {
	if then.IsZero() {
		return "-"
	}
	d := now.Sub(then)
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}
