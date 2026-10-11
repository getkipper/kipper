package manifest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateRouteHost(t *testing.T) {
	for _, ok := range []string{"shop.example.com", "api--lab.kipper.run", "a.b.c.example.org"} {
		assert.NoError(t, ValidateRouteHost(ok), ok)
	}
	for _, bad := range []string{"", "https://shop.example.com", "Shop.Example.com", "shop", "shop..example.com", "-shop.example.com", "*.example.com", "shop.example.com/api"} {
		assert.Error(t, ValidateRouteHost(bad), bad)
	}
}

func TestValidateRoutePath(t *testing.T) {
	for _, ok := range []string{"/", "/api", "/api/v1", "/webhooks/stripe"} {
		assert.NoError(t, ValidateRoutePath(ok), ok)
	}
	for _, bad := range []string{"api", "/api?x=1", "/a b", "//api", "/../etc"} {
		assert.Error(t, ValidateRoutePath(bad), bad)
	}
}
