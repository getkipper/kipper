package manifest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestServiceTLSReachesTheCR(t *testing.T) {
	r := convertService("db", "shop", SvcSpec{Type: "postgres", TLS: "on"})
	spec := r.Object.Object["spec"].(map[string]interface{})
	assert.Equal(t, "on", spec["tls"])

	r = convertService("cache", "shop", SvcSpec{Type: "redis"})
	assert.NotContains(t, r.Object.Object["spec"], "tls", "an absent field stays absent, which means off")
}

func TestTLSOnAServiceThatCannotServeItIsRefused(t *testing.T) {
	m := &Manifest{Project: "shop", Services: map[string]SvcSpec{"cache": {Type: "redis", TLS: "on"}}}

	assert.Error(t, Validate(m), "the CLI refuses it, so kipper.yaml must too")
}
