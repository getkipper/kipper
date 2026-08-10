package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestDriverInfoRejectsInjectableDatabaseName(t *testing.T) {
	// The override reaches the DSN as the database name; a space would inject
	// pgx keyword/value connection params. Rejection happens before any
	// cluster lookup, so a bare handler is enough.
	injections := []string{
		"app host=attacker.example.com",
		"app sslmode=disable",
		"app/x?tls=skip-verify",
		"app;DROP",
	}
	d := &Database{}
	for _, name := range injections {
		if _, err := d.driverInfo(context.Background(), "svc", "ns", name); err == nil {
			t.Errorf("driverInfo accepted injectable database name %q", name)
		}
	}
}

func TestValidDBNameAcceptsPlainIdentifiers(t *testing.T) {
	for _, name := range []string{"app", "app_db", "App1", "orders_2026"} {
		if !validDBName.MatchString(name) {
			t.Errorf("validDBName rejected plain identifier %q", name)
		}
	}
}

func TestShouldAutoLimit(t *testing.T) {
	cases := []struct {
		sql  string
		want bool
	}{
		{"SELECT * FROM users", true},
		{"  select id from users", true},
		{"SELECT * FROM users LIMIT 10", false},
		{"INSERT INTO users (id) VALUES (1)", false},
		{"UPDATE users SET name='x'", false},
		{"DELETE FROM users", false},
		{"WITH cte AS (SELECT 1) SELECT * FROM cte", false}, // CTE — out of scope; safer to not auto-limit
		{"explain select * from users", false},
	}
	for _, c := range cases {
		if got := shouldAutoLimit(c.sql); got != c.want {
			t.Errorf("shouldAutoLimit(%q) = %v, want %v", c.sql, got, c.want)
		}
	}
}

func TestApplyLimit(t *testing.T) {
	cases := []struct {
		in     string
		limit  int
		driver string
		want   string
	}{
		{"SELECT * FROM users", 100, "pgx", "SELECT * FROM users LIMIT 100"},
		{"SELECT * FROM users;", 50, "mysql", "SELECT * FROM users LIMIT 50"},
		{"  SELECT id FROM t  ", 10, "pgx", "SELECT id FROM t LIMIT 10"},
	}
	for _, c := range cases {
		if got := applyLimit(c.in, c.limit, c.driver); got != c.want {
			t.Errorf("applyLimit(%q, %d, %q) = %q, want %q", c.in, c.limit, c.driver, got, c.want)
		}
	}
}

func TestNormaliseKind(t *testing.T) {
	pgCases := map[string]string{"BASE TABLE": "table", "VIEW": "view", "FOREIGN": "foreign"}
	for in, want := range pgCases {
		if got := normalisePostgresKind(in); got != want {
			t.Errorf("normalisePostgresKind(%q) = %q, want %q", in, got, want)
		}
	}
	myCases := map[string]string{"BASE TABLE": "table", "VIEW": "view"}
	for in, want := range myCases {
		if got := normaliseMySQLKind(in); got != want {
			t.Errorf("normaliseMySQLKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGroupSchema(t *testing.T) {
	rels := []schemaRow{
		{catalog: "app", schema: "public", name: "users", kind: "BASE TABLE"},
		{catalog: "app", schema: "public", name: "orders", kind: "BASE TABLE"},
		{catalog: "app", schema: "billing", name: "invoices", kind: "BASE TABLE"},
		{catalog: "analytics", schema: "public", name: "events", kind: "BASE TABLE"},
	}
	got := groupSchema(rels, normalisePostgresKind)

	if len(got.Databases) != 2 {
		t.Fatalf("expected 2 databases, got %d", len(got.Databases))
	}
	app := got.Databases[0]
	if app.Name != "app" {
		t.Errorf("expected first database 'app', got %q", app.Name)
	}
	if len(app.Schemas) != 2 {
		t.Errorf("expected 2 schemas in app, got %d", len(app.Schemas))
	}
	publicSchema := app.Schemas[0]
	if publicSchema.Name != "public" || len(publicSchema.Relations) != 2 {
		t.Errorf("unexpected public schema: %+v", publicSchema)
	}
	for _, r := range publicSchema.Relations {
		if r.Kind != "table" {
			t.Errorf("expected kind=table, got %q", r.Kind)
		}
	}
}

func TestNormaliseRow_Bytes(t *testing.T) {
	in := []interface{}{[]byte("hello"), int64(42), nil, "world"}
	out := normaliseRow(in)
	if out[0] != "hello" {
		t.Errorf("bytes should become string, got %T %v", out[0], out[0])
	}
	if out[1] != int64(42) {
		t.Errorf("int64 should pass through, got %v", out[1])
	}
	if out[2] != nil {
		t.Errorf("nil should pass through, got %v", out[2])
	}
	if out[3] != "world" {
		t.Errorf("string should pass through, got %v", out[3])
	}
}

func TestApplyLimit_StripsTrailingSemicolon(t *testing.T) {
	got := applyLimit("SELECT * FROM users;", 5, "pgx")
	if strings.Contains(got, ";") {
		t.Errorf("trailing semicolon should be stripped before LIMIT: %q", got)
	}
}

// Empty results must marshal as `[]`, not `null`, so the browser
// doesn't have to defend against either shape on every render.
func TestGroupSchema_EmptyMarshalsAsArray(t *testing.T) {
	resp := groupSchema(nil, normalisePostgresKind)
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"databases":[]`) {
		t.Errorf("expected databases:[] in JSON, got %s", b)
	}
}

// A schema with no tables must still appear in the sidebar. Without
// this, dropping every table in a schema makes the schema disappear
// from the UI even though Postgres still has it — confusing users into
// thinking their service bindings broke.
func TestGroupSchema_EmptySchemaStillAppears(t *testing.T) {
	rels := []schemaRow{
		// Schema-only marker — empty name and kind.
		{catalog: "app", schema: "public"},
	}
	got := groupSchema(rels, normalisePostgresKind)
	if len(got.Databases) != 1 || got.Databases[0].Name != "app" {
		t.Fatalf("expected one 'app' database, got %+v", got.Databases)
	}
	schemas := got.Databases[0].Schemas
	if len(schemas) != 1 || schemas[0].Name != "public" {
		t.Fatalf("expected one 'public' schema, got %+v", schemas)
	}
	if len(schemas[0].Relations) != 0 {
		t.Errorf("expected zero relations, got %+v", schemas[0].Relations)
	}
}

func TestGroupSchema_SchemaMarkerMergesWithRelations(t *testing.T) {
	// Schema-only marker first, then a real table in the same schema.
	// The relation should land under the schema created by the marker.
	rels := []schemaRow{
		{catalog: "app", schema: "public"},
		{catalog: "app", schema: "public", name: "users", kind: "BASE TABLE"},
	}
	got := groupSchema(rels, normalisePostgresKind)
	publicSchema := got.Databases[0].Schemas[0]
	if len(publicSchema.Relations) != 1 || publicSchema.Relations[0].Name != "users" {
		t.Errorf("expected single 'users' relation, got %+v", publicSchema.Relations)
	}
}

func TestGroupSchema_EmptyDatabaseHasArraySchemas(t *testing.T) {
	// When a database has at least one row, schemas/relations must be
	// non-nil arrays for the same reason.
	rels := []schemaRow{{catalog: "app", schema: "public", name: "users", kind: "BASE TABLE"}}
	resp := groupSchema(rels, normalisePostgresKind)
	b, _ := json.Marshal(resp)
	if !strings.Contains(string(b), `"schemas":`) || strings.Contains(string(b), `"schemas":null`) {
		t.Errorf("schemas should be a non-null array, got %s", b)
	}
	if !strings.Contains(string(b), `"relations":`) || strings.Contains(string(b), `"relations":null`) {
		t.Errorf("relations should be a non-null array, got %s", b)
	}
}
