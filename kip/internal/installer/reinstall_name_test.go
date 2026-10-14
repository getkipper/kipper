package installer

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/config"
)

func TestReinstallNameHint(t *testing.T) {
	moved := reinstallNameHint(&config.Cluster{Name: "old.example.com", Domain: "old.example.com"}, "new.example.com")
	assert.Contains(t, moved, "old.example.com")
	assert.Contains(t, moved, "new.example.com")
	assert.Contains(t, moved, "kip cluster rename old.example.com new.example.com")

	assert.Empty(t, reinstallNameHint(&config.Cluster{Name: "prod", Domain: "shop.example.com"}, "shop.example.com"), "same domain, nothing to say")
	assert.Empty(t, reinstallNameHint(&config.Cluster{Name: "new.example.com", Domain: "old.example.com"}, "new.example.com"), "the name already matches the new domain")
	assert.Empty(t, reinstallNameHint(nil, "new.example.com"), "a first install has no entry")
}
