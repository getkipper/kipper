package handlers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewPostgresServicesServeTLS(t *testing.T) {
	assert.Equal(t, "on", newServiceTLS("postgres"))
	assert.Equal(t, "", newServiceTLS("redis"), "other types do not serve TLS yet")
}
