package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Keep cluster role help distinct from project permissions.
func TestUserRolesSayWhatEachRoleDecides(t *testing.T) {
	assert.NotContains(t, userAddCmd.Long, "deploy, scale, manage apps and services")
	assert.Contains(t, userAddCmd.Long, "project membership")
}
