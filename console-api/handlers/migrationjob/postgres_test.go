package migrationjob

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildPostgres_ProducesPipedDumpRestore(t *testing.T) {
	spec := BuildPostgres(PostgresSpec{
		TargetNamespace: "demo-prod",
		TargetService:   "backend",
		SourceNamespace: "demo-test",
		Image:           "postgres:16-alpine",
	})

	assert.Equal(t, "demo-prod", spec.Namespace)
	assert.Equal(t, "postgres:16-alpine", spec.Image)
	assert.Contains(t, spec.JobName, "migrate-backend-")

	require := assert.Len
	require(t, spec.Command, 3)
	assert.Equal(t, "sh", spec.Command[0])
	assert.Equal(t, "-c", spec.Command[1])
	body := spec.Command[2]
	assert.Contains(t, body, "pg_dump", "must shell out to pg_dump")
	assert.Contains(t, body, "psql", "must pipe into psql")
	assert.Contains(t, body, "--clean --if-exists", "re-runs need an idempotent restore")
	assert.Contains(t, body, "ON_ERROR_STOP", "any psql error should fail the job")
	assert.Contains(t, body, "${SRC_HOST}", "source host comes from the mirrored secret prefix")
	assert.Contains(t, body, "${DST_HOST}", "target host comes from the local secret prefix")
	assert.True(t, strings.Contains(body, "set -o pipefail"), "pipefail required so pg_dump failure isn't masked")
}

func TestBuildPostgres_MirrorsSourceCredentialsWithUniqueName(t *testing.T) {
	spec := BuildPostgres(PostgresSpec{
		TargetNamespace: "demo-prod",
		TargetService:   "backend",
		SourceNamespace: "demo-test",
		Image:           "postgres:16-alpine",
	})

	assert.Len(t, spec.Mirrors, 1)
	assert.Equal(t, "demo-test", spec.Mirrors[0].SourceNamespace)
	assert.Equal(t, "backend-credentials", spec.Mirrors[0].SourceName)
	assert.Equal(t, "backend-from-demo-test-credentials", spec.Mirrors[0].TargetName,
		"mirror name must encode the source namespace so concurrent migrations from different envs don't collide")

	assert.Len(t, spec.EnvFrom, 2)
	assert.Equal(t, "SRC_", spec.EnvFrom[0].Prefix)
	assert.Equal(t, "backend-from-demo-test-credentials", spec.EnvFrom[0].SecretRef.Name)
	assert.Equal(t, "DST_", spec.EnvFrom[1].Prefix)
	assert.Equal(t, "backend-credentials", spec.EnvFrom[1].SecretRef.Name)
}

func TestBuildPostgres_LabelsForLookup(t *testing.T) {
	spec := BuildPostgres(PostgresSpec{
		TargetNamespace: "demo-prod",
		TargetService:   "backend",
		SourceNamespace: "demo-test",
		Image:           "postgres:16-alpine",
	})
	assert.Equal(t, "backend", spec.Labels["kipper.run/service"])
	assert.Equal(t, "postgres", spec.Labels["kipper.run/service-type"])
}
