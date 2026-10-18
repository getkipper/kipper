package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStatusPrintsNoUsageOnARuntimeError(t *testing.T) {
	assert.True(t, statusCmd.SilenceUsage, "an unreachable API is not a usage mistake")
}
