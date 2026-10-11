package cmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBindSaysWhenTheDatabaseWasNotCreated(t *testing.T) {
	var out bytes.Buffer

	printBindResult(&out, "db", "web", bindResult{
		Type: "postgres", Database: "web_prod",
		Warning: `database "web_prod" was not created in db: pod not running`,
	})

	assert.Contains(t, out.String(), `⚠  The binding was saved, but database "web_prod" was not created`)
	assert.NotContains(t, out.String(), "✔  Database: web_prod", "a database that was not created is not reported as ready")
}

func TestBindReportsTheDatabaseItCreated(t *testing.T) {
	var out bytes.Buffer

	printBindResult(&out, "db", "web", bindResult{Type: "postgres", Database: "web_prod"})

	assert.Contains(t, out.String(), "✔  Database: web_prod")
}
