package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
)

// openMySQLTestDB connects to the server named by KIPPER_TEST_MYSQL_DSN and
// builds a test schema with a unique name there, which it drops when the test
// ends. The DSN needs a user that may create and drop databases, for example
// root:pw@tcp(127.0.0.1:3306)/.
func openMySQLTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("KIPPER_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("KIPPER_TEST_MYSQL_DSN is not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := fmt.Sprintf("kipper_structure_test_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, "CREATE DATABASE "+schema); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+schema)
	})
	stmts := []string{
		`CREATE TABLE ` + schema + `.users (
			id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
			email VARCHAR(255) NOT NULL,
			first_name VARCHAR(100) NULL,
			last_name VARCHAR(100) NULL,
			full_name VARCHAR(201) GENERATED ALWAYS AS (CONCAT(first_name, ' ', last_name)) VIRTUAL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE KEY users_email_uq (email),
			KEY users_lower_email_idx ((lower(email)), first_name),
			KEY users_shout_idx ((concat(last_name, '!'))),
			` + "`a\\'b` INT NULL, `c\\\\d` INT NULL" + `,
			KEY users_odd_names_idx (` + "`a\\'b`, `c\\\\d`" + `),
			KEY users_quote_idx ((concat(last_name, 'it''s \\\\ ok')))
		)`,
		`CREATE TABLE ` + schema + `.order_lines (
			order_id INT NOT NULL,
			line INT NOT NULL,
			user_id BIGINT UNSIGNED NOT NULL,
			PRIMARY KEY (order_id, line),
			CONSTRAINT order_lines_user_fk FOREIGN KEY (user_id) REFERENCES users (id)
		)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	return db, schema
}

func TestReadTableStructureMySQL(t *testing.T) {
	db, schema := openMySQLTestDB(t)
	ctx := context.Background()

	users, err := readTableStructure(ctx, db, "mysql", schema, "users")
	if err != nil {
		t.Fatalf("users structure: %v", err)
	}
	var names []string
	generated := map[string]bool{}
	types := map[string]string{}
	for _, c := range users.Columns {
		names = append(names, c.Name)
		generated[c.Name] = c.Generated
		types[c.Name] = c.Type
	}
	if want := []string{"id", "email", "first_name", "last_name", "full_name", "created_at", `a\'b`, `c\\d`}; !slices.Equal(names, want) {
		t.Errorf("users columns = %v, want %v", names, want)
	}
	if !generated["full_name"] {
		t.Error("full_name is a generated column and should be flagged generated")
	}
	if generated["id"] || generated["created_at"] {
		t.Errorf("AUTO_INCREMENT and DEFAULT CURRENT_TIMESTAMP columns take explicit values, generated = %v", generated)
	}
	if types["email"] != "varchar(255)" || types["id"] != "bigint unsigned" {
		t.Errorf("column types should carry length and sign, got %v", types)
	}
	if !slices.Equal(users.PrimaryKey, []string{"id"}) {
		t.Errorf("users primary key = %v, want [id]", users.PrimaryKey)
	}
	if !hasIndex(users.Indexes, "users_email_uq", true) {
		t.Errorf("users indexes = %+v, want unique users_email_uq", users.Indexes)
	}
	wantCols := map[string][]string{
		"users_lower_email_idx": {"(lower(`email`))", "first_name"},
		"users_shout_idx":       {"(concat(`last_name`,_utf8mb4'!'))"},
		"users_odd_names_idx":   {`a\'b`, `c\\d`},
		"users_quote_idx":       {`(concat(` + "`last_name`" + `,_utf8mb4'it\'s \\\\ ok'))`},
	}
	for _, idx := range users.Indexes {
		if want, ok := wantCols[idx.Name]; ok {
			if !slices.Equal(idx.Columns, want) {
				t.Errorf("%s columns = %q, want %q", idx.Name, idx.Columns, want)
			}
			delete(wantCols, idx.Name)
		}
	}
	if len(wantCols) > 0 {
		t.Errorf("functional indexes missing from %+v: %v", users.Indexes, wantCols)
	}

	lines, err := readTableStructure(ctx, db, "mysql", schema, "order_lines")
	if err != nil {
		t.Fatalf("order_lines structure: %v", err)
	}
	if !slices.Equal(lines.PrimaryKey, []string{"order_id", "line"}) {
		t.Errorf("order_lines primary key = %v, want [order_id line]", lines.PrimaryKey)
	}
	wantFK := fkInfo{Column: "user_id", RefSchema: schema, RefTable: "users", RefColumn: "id"}
	if len(lines.ForeignKeys) != 1 || lines.ForeignKeys[0] != wantFK {
		t.Errorf("order_lines foreign keys = %+v, want [%+v]", lines.ForeignKeys, wantFK)
	}
	if !hasIndex(lines.Indexes, "PRIMARY", true) {
		t.Errorf("order_lines indexes = %+v, want PRIMARY", lines.Indexes)
	}
}

// TestBrowseRowsMySQLIncludesInvisibleColumns reads a table with an INVISIBLE
// column, which SELECT * leaves out, through the browse and insert lookups.
func TestBrowseRowsMySQLIncludesInvisibleColumns(t *testing.T) {
	db, schema := openMySQLTestDB(t)
	ctx := context.Background()
	create := "CREATE TABLE " + quoteIdent("mysql", schema) + ".vis (id INT AUTO_INCREMENT PRIMARY KEY, label VARCHAR(10), hidden INT INVISIBLE)" //nolint:gosec // The schema is the test's own generated name.
	if _, err := db.ExecContext(ctx, create); err != nil {
		t.Fatal(err)
	}
	res, err := db.ExecContext(ctx, buildInsert("mysql", schema, "vis", []string{"label", "hidden"}), "abc", 7)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	structure, err := readTableStructure(ctx, db, "mysql", schema, "vis")
	if err != nil {
		t.Fatal(err)
	}
	if len(structure.Columns) != 3 || structure.Columns[2].Name != "hidden" {
		t.Fatalf("columns = %+v, want id, label, hidden", structure.Columns)
	}

	rows, err := db.QueryContext(ctx, buildSelectRows("mysql", schema, "vis", structure.Columns, "id", "ASC", 10, 0, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatal("no row")
	}
	targets := make([]interface{}, len(structure.Columns))
	ptrs := make([]interface{}, len(targets))
	for i := range targets {
		ptrs[i] = &targets[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatalf("browse scan: %v", err)
	}
	if got := fmt.Sprint(normaliseRow(targets)[2]); got != "7" {
		t.Errorf("browsed hidden = %v, want 7", got)
	}

	row, err := scanSingleRow(db.QueryRowContext(ctx, buildSelectRowByKey("mysql", schema, "vis", structure.Columns, "id"), id), structure)
	if err != nil {
		t.Fatalf("insert lookup: %v", err)
	}
	if got := fmt.Sprint(row[2]); got != "7" {
		t.Errorf("looked-up hidden = %v, want 7", got)
	}
}

func TestDropIndexMySQL(t *testing.T) {
	db, schema := openMySQLTestDB(t)
	ctx := context.Background()

	ddl, err := buildDropIndex("mysql", schema, "users", "users_email_uq", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := execDDL(ctx, db, ddl, false); err != nil {
		t.Fatalf("%s: %v", ddl, err)
	}
	users, err := readTableStructure(ctx, db, "mysql", schema, "users")
	if err != nil {
		t.Fatalf("users structure: %v", err)
	}
	if hasIndex(users.Indexes, "users_email_uq", true) {
		t.Errorf("users_email_uq is still present after %s", ddl)
	}
}

func hasIndex(indexes []indexInfo, name string, unique bool) bool {
	for _, idx := range indexes {
		if idx.Name == name && idx.Unique == unique {
			return true
		}
	}
	return false
}
