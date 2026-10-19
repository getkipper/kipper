package handlers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewPostgresServicesGetConsistentBackups(t *testing.T) {
	assert.True(t, newServiceConsistentBackups("postgres"))
	assert.False(t, newServiceConsistentBackups("mysql"), "only Postgres dumps so far")
}
