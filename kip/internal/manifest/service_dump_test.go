package manifest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConsistentBackupsReachTheCRAndBack(t *testing.T) {
	r := convertService("db", "shop", SvcSpec{Type: "postgres", ConsistentBackups: true})
	spec := r.Object.Object["spec"].(map[string]interface{})
	assert.Equal(t, true, spec["consistentBackups"])

	r = convertService("db", "shop", SvcSpec{Type: "postgres"})
	assert.NotContains(t, r.Object.Object["spec"], "consistentBackups")
}

func TestConsistentBackupsOnAnotherTypeAreRefused(t *testing.T) {
	m := &Manifest{Project: "shop", Services: map[string]SvcSpec{"cache": {Type: "redis", ConsistentBackups: true}}}
	assert.Error(t, Validate(m))
}
