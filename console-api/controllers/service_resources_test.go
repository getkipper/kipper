package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/controller/pkg/servicecatalog"
)

// The CLI capacity check relies on these defaults matching the reconciler.
func TestServiceCatalogRequestsMatchTheSharedDefaults(t *testing.T) {
	for _, svcType := range []string{"postgres", "mysql", "redis", "mongodb", "rabbitmq", "opensearch", "minio", "mailhog"} {
		cpuReq, _, memReq, _ := profileResources(serviceCatalog(svcType).resourceProfile)
		cpu, memory := servicecatalog.DefaultResources(svcType)
		assert.Equal(t, cpuReq, cpu, "%s cpu request", svcType)
		assert.Equal(t, memReq, memory, "%s memory request", svcType)
	}
}
