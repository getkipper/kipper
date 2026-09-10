package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestHumanAge(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		then time.Time
		want string
	}{
		{"a moment that was never set", time.Time{}, "-"},
		{"the same instant", now, "0s"},
		{"seconds", now.Add(-45 * time.Second), "45s"},
		{"rounds down to the minute", now.Add(-90 * time.Second), "1m"},
		{"minutes", now.Add(-3 * time.Minute), "3m"},
		{"rounds down to the hour", now.Add(-119 * time.Minute), "1h"},
		{"hours", now.Add(-2 * time.Hour), "2h"},
		{"rounds down to the day", now.Add(-25 * time.Hour), "1d"},
		{"days", now.Add(-3 * 24 * time.Hour), "3d"},
		{"a clock running ahead reads zero, never a negative age", now.Add(5 * time.Minute), "0s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, humanAge(now, tt.then))
		})
	}
}
