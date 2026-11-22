package migration

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Each import replaces data from a failed attempt so the operator can retry.
// Migration preserves namespace names, so an exact namespace avoids adding
// the operator's organization prefix through --project.
//
// A database too large for the automatic transfer is moved with kip's own
// export and import, one database at a time, from a machine that reaches both
// clusters. kip handles the engine credentials, so no step names one.
func TestManualSteps_MoveEachDatabaseWithKip(t *testing.T) {
	steps := strings.Join(buildManualMigrationSteps("mysql", "db", "shop-prod", []string{"orders", "stock"}), "\n")

	for _, db := range []string{"orders", "stock"} {
		assert.Contains(t, steps, "kip service export db --namespace shop-prod --database "+db+" --file shop-prod.db."+db+".sql.gz --cluster <source-cluster> && "+
			"kip service import db --namespace shop-prod --database "+db+" --drop --file shop-prod.db."+db+".sql.gz --cluster <target-cluster>")
	}
	assert.NotContains(t, steps, "scp", "kip reaches both clusters itself")
	assert.NotContains(t, steps, "PASSWORD")
}

func TestManualSteps_NameEachEnginesDumpFormat(t *testing.T) {
	postgres := strings.Join(buildManualMigrationSteps("postgres", "db", "shop", []string{"app"}), "\n")
	assert.Contains(t, postgres, "kip service export db --namespace shop --database app --file shop.db.app.dump --cluster <source-cluster>",
		"the default environment needs no flag")

	mongo := strings.Join(buildManualMigrationSteps("mongodb", "docs", "shop-prod", nil), "\n")
	assert.Contains(t, mongo, "kip service export docs --namespace shop-prod --file shop-prod.docs.archive.gz --cluster <source-cluster> && "+
		"kip service import docs --namespace shop-prod --drop --file shop-prod.docs.archive.gz --cluster <target-cluster>",
		"a mongodb archive carries every database")
}

func TestManualSteps_SayToRepeatWhenTheDatabasesAreUnknown(t *testing.T) {
	steps := buildManualMigrationSteps("mysql", "db", "shop-prod", nil)
	joined := strings.Join(steps, "\n")

	// Omitting --database starts with the database named in the service's credentials.
	assert.Contains(t, joined, "kip service export db --namespace shop-prod --file shop-prod.db.sql.gz --cluster <source-cluster> && "+
		"kip service import db --namespace shop-prod --drop --file shop-prod.db.sql.gz --cluster <target-cluster>")
	for _, step := range steps {
		if strings.HasPrefix(step, "kip ") {
			assert.NotContains(t, step, "--database")
		}
	}
	// Export never overwrites a file, so the next database needs its own.
	assert.Contains(t, joined, "Repeat this line for each additional database in the service. Add --database <name> to both commands and use the same --file path in both, with a different path for each database.")
}

// Steps from several namespaces can run in one directory. Export rejects an
// existing dump file, so requiring a successful export prevents importing data
// from another namespace or an earlier attempt.
func TestManualSteps_KeepEachNamespacesDumpApart(t *testing.T) {
	prod := strings.Join(buildManualMigrationSteps("mysql", "db", "shop-prod", []string{"app"}), "\n")
	staging := strings.Join(buildManualMigrationSteps("mysql", "db", "shop-staging", []string{"app"}), "\n")

	assert.Contains(t, prod, "--file shop-prod.db.app.sql.gz")
	assert.Contains(t, staging, "--file shop-staging.db.app.sql.gz")
	for _, step := range strings.Split(prod, "\n") {
		if strings.Contains(step, "kip service import") {
			assert.True(t, strings.HasPrefix(step, "kip service export "), "an import runs only after its own export: %s", step)
			assert.Contains(t, step, " && kip service import ")
		}
	}
}

func TestManualSteps_NeverPasteADatabaseNameIntoACommand(t *testing.T) {
	steps := buildManualMigrationSteps("mysql", "db", "shop-prod", []string{"orders", "x; rm -rf ~"})

	for _, step := range steps {
		if strings.HasPrefix(step, "kip ") {
			assert.NotContains(t, step, "rm -rf", "a database name reached a command the operator pastes")
		}
	}
	assert.Contains(t, strings.Join(steps, "\n"), "by hand")
}

func TestManualSteps_ForAnEngineKipCannotExport(t *testing.T) {
	steps := buildManualMigrationSteps("redis", "cache", "shop-prod", nil)

	assert.Len(t, steps, 1)
	assert.Contains(t, steps[0], "redis")
}

// A database name is an identifier, so spaces and line breaks in it are part
// of the name. Discovery has to hand the exact name on, or the steps would
// move a different database, or none, without the warning an unsafe name gets.
func TestListDatabases_KeepsEachNameExactly(t *testing.T) {
	names := []string{" orders ", "line\nbreak", "stock"}
	for svcType, encoder := range map[string]string{"postgres": "encode(", "mysql": "HEX("} {
		engine := func(cmd []string) (string, error) {
			script := strings.Join(cmd, " ")
			if !strings.Contains(script, encoder) {
				// An engine asked for plain names prints them one per line.
				return strings.Join(names, "\n") + "\n", nil
			}
			var lines []string
			for _, n := range names {
				lines = append(lines, hex.EncodeToString([]byte(n)))
			}
			return strings.Join(lines, "\n") + "\n", nil
		}

		got, err := listDatabases(svcType, engine)

		assert.NoError(t, err)
		assert.Equal(t, names, got, svcType)
		steps := strings.Join(buildManualMigrationSteps(svcType, "db", "shop", got), "\n")
		assert.Contains(t, steps, `Move the database " orders " by hand`, svcType)
		assert.Contains(t, steps, "--database stock", svcType)
		assert.NotContains(t, steps, "--database orders", svcType)
	}
}
