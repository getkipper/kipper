package handlers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewPostgresServicesRequireTLS(t *testing.T) {
	assert.Equal(t, "required", newServiceTLS("postgres"))
	assert.Equal(t, "", newServiceTLS("redis"), "other types do not serve TLS yet")
}
