package handlers

import (
	"strings"
	"testing"
)

func strPtr(s string) *string { return &s }

func TestBuildColumnDef(t *testing.T) {
	cases := []struct {
		driver string
		col    columnSpec
		want   string
	}{
		{"pgx", columnSpec{Name: "id", Type: "bigserial", Nullable: false}, `"id" bigserial NOT NULL`},
		{"pgx", columnSpec{Name: "email", Type: "text", Nullable: true}, `"email" text`},
		{"pgx", columnSpec{Name: "name", Type: "text", Nullable: false, Default: strPtr("''")}, `"name" text NOT NULL DEFAULT ''`},
		{"mysql", columnSpec{Name: "id", Type: "BIGINT", Nullable: false}, "`id` BIGINT NOT NULL"},
		{"pgx", columnSpec{Name: "fullname", Type: "text", Nullable: true, GeneratedExpr: "first || ' ' || last"}, `"fullname" text GENERATED ALWAYS AS (first || ' ' || last) STORED`},
	}
	for _, c := range cases {
		got, err := buildColumnDef(c.driver, c.col)
		if err != nil {
			t.Errorf("buildColumnDef(%v): unexpected err %v", c.col, err)
			continue
		}
		if got != c.want {
			t.Errorf("buildColumnDef(%v):\n got %q\nwant %q", c.col, got, c.want)
		}
	}
}

func TestBuildCreateTable_Postgres(t *testing.T) {
	req := createTableRequest{
		Schema: "public",
		Name:   "domains",
		Columns: []columnSpec{
			{Name: "id", Type: "bigserial", Nullable: false},
			{Name: "name", Type: "text", Nullable: false},
			{Name: "last_synced_at", Type: "timestamptz", Nullable: true, Default: strPtr("now()")},
		},
		Constraints: []constraintSpec{
			{Type: "PRIMARY KEY", Columns: []string{"id"}},
			{Name: "domains_name_unique", Type: "UNIQUE", Columns: []string{"name"}},
		},
	}
	got, err := buildCreateTable("pgx", req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	mustContain := []string{
		`CREATE TABLE "public"."domains"`,
		`"id" bigserial NOT NULL`,
		`"name" text NOT NULL`,
		`"last_synced_at" timestamptz DEFAULT now()`,
		`PRIMARY KEY ("id")`,
		`CONSTRAINT "domains_name_unique" UNIQUE ("name")`,
	}
	for _, want := range mustContain {
		if !strings.Contains(got, want) {
			t.Errorf("DDL missing %q in:\n%s", want, got)
		}
	}
}

func TestBuildAlterTable_Postgres(t *testing.T) {
	ops := []tableOp{
		{Op: "add_column", Column: &columnSpec{Name: "last_synced_at", Type: "timestamptz", Nullable: true}},
		{Op: "drop_column", Name: "legacy_field"},
		{Op: "rename_column", OldName: "name", NewName: "domain_name"},
		{Op: "alter_column_type", Column: &columnSpec{Name: "owner_id", Type: "bigint"}},
		{Op: "set_nullable", Column: &columnSpec{Name: "owner_id", Nullable: false}},
		{Op: "set_default", Column: &columnSpec{Name: "status", Default: strPtr("'pending'")}},
		{Op: "drop_constraint", ConstraintName: "old_check"},
	}
	got, err := buildAlterTable("pgx", "public", "domains", ops)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != len(ops) {
		t.Fatalf("expected %d statements, got %d: %v", len(ops), len(got), got)
	}
	checks := []struct {
		idx  int
		want string
	}{
		{0, `ADD COLUMN "last_synced_at" timestamptz`},
		{1, `DROP COLUMN "legacy_field"`},
		{2, `RENAME COLUMN "name" TO "domain_name"`},
		{3, `ALTER COLUMN "owner_id" TYPE bigint`},
		{4, `ALTER COLUMN "owner_id" SET NOT NULL`},
		{5, `ALTER COLUMN "status" SET DEFAULT 'pending'`},
		{6, `DROP CONSTRAINT "old_check"`},
	}
	for _, c := range checks {
		if !strings.Contains(got[c.idx], c.want) {
			t.Errorf("stmt[%d] missing %q in %q", c.idx, c.want, got[c.idx])
		}
	}
}

func TestBuildAlterTable_RejectsUnknownOp(t *testing.T) {
	_, err := buildAlterTable("pgx", "public", "t", []tableOp{{Op: "explode"}})
	if err == nil || !strings.Contains(err.Error(), "unknown op") {
		t.Errorf("expected unknown op error, got %v", err)
	}
}

func TestBuildCreateIndex(t *testing.T) {
	cases := []struct {
		driver string
		req    indexRequest
		want   string
	}{
		{
			"pgx",
			indexRequest{Schema: "public", Table: "domains", Name: "domains_owner_idx", Columns: []string{"owner_id"}},
			`CREATE INDEX "domains_owner_idx" ON "public"."domains" ("owner_id")`,
		},
		{
			"pgx",
			indexRequest{Schema: "public", Table: "domains", Name: "domains_name_uniq", Columns: []string{"name"}, Unique: true},
			`CREATE UNIQUE INDEX "domains_name_uniq" ON "public"."domains" ("name")`,
		},
		{
			"pgx",
			indexRequest{Schema: "public", Table: "events", Name: "events_payload_gin", Columns: []string{"payload"}, Method: "gin"},
			`CREATE INDEX "events_payload_gin" ON "public"."events" USING gin ("payload")`,
		},
		{
			"pgx",
			indexRequest{Schema: "public", Table: "events", Name: "events_active_partial", Columns: []string{"created_at"}, Where: "status = 'active'"},
			`CREATE INDEX "events_active_partial" ON "public"."events" ("created_at") WHERE status = 'active'`,
		},
		{
			"pgx",
			indexRequest{Schema: "public", Table: "events", Name: "ev_concurrent", Columns: []string{"id"}, Concurrent: true},
			`CREATE INDEX CONCURRENTLY "ev_concurrent" ON "public"."events" ("id")`,
		},
		{
			"mysql",
			indexRequest{Schema: "appdb", Table: "users", Name: "users_email_idx", Columns: []string{"email"}, Unique: true},
			"CREATE UNIQUE INDEX `users_email_idx` ON `appdb`.`users` (`email`)",
		},
	}
	for _, c := range cases {
		got, err := buildCreateIndex(c.driver, c.req)
		if err != nil {
			t.Errorf("err: %v", err)
			continue
		}
		if got != c.want {
			t.Errorf("got %q\nwant %q", got, c.want)
		}
	}
}

func TestBuildDropIndex(t *testing.T) {
	got := buildDropIndex("pgx", "public", "users_email_idx", true)
	want := `DROP INDEX CONCURRENTLY "public"."users_email_idx"`
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	got = buildDropIndex("pgx", "public", "users_email_idx", false)
	want = `DROP INDEX "public"."users_email_idx"`
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestBuildConstraint_FK(t *testing.T) {
	c := constraintSpec{
		Name: "domains_owner_fk", Type: "FOREIGN KEY",
		Columns:   []string{"owner_id"},
		RefSchema: "public", RefTable: "users", RefColumns: []string{"id"},
		OnDelete: "cascade", OnUpdate: "no action",
	}
	got, err := buildConstraintClause("pgx", c)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := `CONSTRAINT "domains_owner_fk" FOREIGN KEY ("owner_id") REFERENCES "public"."users" ("id") ON DELETE CASCADE ON UPDATE NO ACTION`
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestBuildConstraint_Check(t *testing.T) {
	c := constraintSpec{Name: "positive_price", Type: "CHECK", CheckExpr: "price > 0"}
	got, err := buildConstraintClause("pgx", c)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != `CONSTRAINT "positive_price" CHECK (price > 0)` {
		t.Errorf("got %q", got)
	}
}

func TestParsePgArray(t *testing.T) {
	cases := map[string][]string{
		"{a,b,c}":       {"a", "b", "c"},
		`{"a b","c,d"}`: {"a b", "c,d"},
		"{}":            {},
		"":              nil,
		"not-an-array":  nil,
	}
	for in, want := range cases {
		got := parsePgArray(in)
		if len(got) != len(want) {
			t.Errorf("parsePgArray(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("parsePgArray(%q)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}
