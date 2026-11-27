package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"
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
		`CREATE TABLE ` + schema + `.settings (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			label VARCHAR(50) CHARACTER SET latin1 COLLATE latin1_bin NOT NULL DEFAULT 'none' COMMENT 'NOT NULL in a comment',
			note VARCHAR(20) NULL COMMENT 'it''s optional',
			code VARCHAR(5) CHARACTER SET latin1 NOT NULL DEFAULT '1',
			updated_at DATETIME NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			body TEXT,
			doubled INT GENERATED ALWAYS AS (CHAR_LENGTH(label) * 2) VIRTUAL,
			ts TIMESTAMP NULL,
			loc POINT SRID 4326,
			hidden INT NULL INVISIBLE COMMENT 'x',
			` + "`we``ird`" + ` INT NULL DEFAULT 5
		)`,
		`CREATE TABLE ` + schema + `.nokey (id INT NULL, v INT)`,
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

func TestExecInTxMySQLCountsCommittedStatements(t *testing.T) {
	db, schema := openMySQLTestDB(t)
	ctx := context.Background()
	add := fmt.Sprintf("ALTER TABLE %s ADD COLUMN extra int", qualified("mysql", schema, "users"))
	bad := fmt.Sprintf("ALTER TABLE %s DROP COLUMN no_such_column", qualified("mysql", schema, "users"))
	for name, tc := range map[string]struct {
		stmts []string
		ran   int
	}{
		"first statement fails":  {[]string{bad, add}, 0},
		"second statement fails": {[]string{add, bad}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			err := execInTx(ctx, db, tc.stmts)
			if n, ok := committedStatements(err); !ok || n != tc.ran {
				t.Errorf("committed = %d, %v, want %d, true (error %v)", n, ok, tc.ran, err)
			}
		})
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

// buildMySQLAlter builds the DDL for ops the way AlterTable does, from the
// table's current column definitions.
func buildMySQLAlter(t *testing.T, db *sql.DB, schema, table string, ops []tableOp) ([]string, error) {
	t.Helper()
	ddl, _, err := planMySQLAlter(t, db, schema, table, ops)
	return ddl, err
}

// planMySQLAlter builds DDL for ops as AlterTable does and returns the prepared
// connection on which it must run. Test cleanup restores the session settings
// and closes the connection.
func planMySQLAlter(t *testing.T, db *sql.DB, schema, table string, ops []tableOp) ([]string, *sql.Conn, error) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cols, restore, err := currentMySQLColumns(ctx, conn, "mysql", schema, table)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("read column definitions: %v", err)
	}
	t.Cleanup(func() {
		restore()
		_ = conn.Close()
	})
	ddl, err := buildAlterTable("mysql", schema, table, ops, cols)
	return ddl, conn, err
}

// applyMySQLAlter builds and runs ops the way AlterTable does, then restores
// the session.
func applyMySQLAlter(t *testing.T, db *sql.DB, schema, table string, ops []tableOp) ([]string, error) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	cols, restore, err := currentMySQLColumns(ctx, conn, "mysql", schema, table)
	if err != nil {
		t.Fatalf("read column definitions: %v", err)
	}
	defer restore()
	ddl, err := buildAlterTable("mysql", schema, table, ops, cols)
	if err != nil {
		return nil, err
	}
	return ddl, execInTx(ctx, conn, ddl)
}

type mysqlColumnState struct {
	Type, Nullable, Default, Extra, Comment, Collation, SRID string
}

func mysqlColumnStates(t *testing.T, db *sql.DB, schema, table string) map[string]mysqlColumnState {
	t.Helper()
	rows, err := db.Query(`SELECT column_name, column_type, is_nullable, COALESCE(column_default, '<null>'),
		extra, column_comment, COALESCE(collation_name, ''), COALESCE(CAST(srs_id AS CHAR), '')
		FROM information_schema.columns WHERE table_schema = ? AND table_name = ?`, schema, table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]mysqlColumnState{}
	for rows.Next() {
		var name string
		var c mysqlColumnState
		if err := rows.Scan(&name, &c.Type, &c.Nullable, &c.Default, &c.Extra, &c.Comment, &c.Collation, &c.SRID); err != nil {
			t.Fatal(err)
		}
		out[name] = c
	}
	return out
}

// TestAlterColumnMySQLKeepsDefinition uses the designer's empty type for
// set_nullable and checks that type and nullability changes preserve other
// column attributes, except incompatible character sets, collations, and
// NULL defaults.
func TestAlterColumnMySQLKeepsDefinition(t *testing.T) {
	db, schema := openMySQLTestDB(t)
	ops := []tableOp{
		{Op: "alter_column_type", Column: &columnSpec{Name: "id", Type: "bigint"}},
		{Op: "alter_column_type", Column: &columnSpec{Name: "label", Type: "varchar(80)"}},
		{Op: "set_nullable", Column: &columnSpec{Name: "label", Nullable: true}},
		{Op: "set_nullable", Column: &columnSpec{Name: "note", Nullable: false}},
		{Op: "rename_column", OldName: "note", NewName: "remark"},
		{Op: "alter_column_type", Column: &columnSpec{Name: "remark", Type: "varchar(40)"}},
		{Op: "rename_column", OldName: "code", NewName: "code1"},
		{Op: "rename_column", OldName: "code1", NewName: "code2"},
		{Op: "alter_column_type", Column: &columnSpec{Name: "code2", Type: "int"}},
		{Op: "set_nullable", Column: &columnSpec{Name: "updated_at", Nullable: false}},
		{Op: "set_nullable", Column: &columnSpec{Name: "body", Nullable: false}},
		{Op: "set_nullable", Column: &columnSpec{Name: "ts", Nullable: false}},
		{Op: "set_nullable", Column: &columnSpec{Name: "loc", Nullable: false}},
		{Op: "alter_column_type", Column: &columnSpec{Name: "hidden", Type: "bigint"}},
		{Op: "set_nullable", Column: &columnSpec{Name: "we`ird", Nullable: false}},
	}
	ddl, err := applyMySQLAlter(t, db, schema, "settings", ops)
	if err != nil {
		t.Fatalf("alter: %v\n%s", err, strings.Join(ddl, "\n"))
	}
	assertMySQLSettingsAfterAlter(t, db, schema)
}

func assertMySQLSettingsAfterAlter(t *testing.T, db *sql.DB, schema string) {
	t.Helper()
	got := mysqlColumnStates(t, db, schema, "settings")
	want := map[string]mysqlColumnState{
		"id":         {Type: "bigint", Nullable: "NO", Default: "<null>", Extra: "auto_increment"},
		"label":      {Type: "varchar(80)", Nullable: "YES", Default: "none", Comment: "NOT NULL in a comment", Collation: "latin1_bin"},
		"remark":     {Type: "varchar(40)", Nullable: "NO", Default: "<null>", Comment: "it's optional", Collation: "utf8mb4_0900_ai_ci"},
		"code2":      {Type: "int", Nullable: "NO", Default: "1"},
		"updated_at": {Type: "datetime", Nullable: "NO", Default: "CURRENT_TIMESTAMP", Extra: "DEFAULT_GENERATED on update CURRENT_TIMESTAMP"},
		"body":       {Type: "text", Nullable: "NO", Default: "<null>", Collation: "utf8mb4_0900_ai_ci"},
		"ts":         {Type: "timestamp", Nullable: "NO", Default: "<null>"},
		"loc":        {Type: "point", Nullable: "NO", Default: "<null>", SRID: "4326"},
		"hidden":     {Type: "bigint", Nullable: "YES", Default: "<null>", Extra: "INVISIBLE", Comment: "x"},
		"we`ird":     {Type: "int", Nullable: "NO", Default: "5"},
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %+v\nwant %+v", name, got[name], w)
		}
	}
	for _, gone := range []string{"note", "code", "code1"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%s should have been renamed", gone)
		}
	}
}

// TestAlterColumnMySQLTypeSynonyms changes a latin1 column to type synonyms.
// CHARACTER keeps the column's character set, and the national types, which
// fix their own character set, drop it.
func TestAlterColumnMySQLTypeSynonyms(t *testing.T) {
	for typ, want := range map[string]mysqlColumnState{
		"character varying(90)": {Type: "varchar(90)", Nullable: "NO", Default: "none", Comment: "NOT NULL in a comment", Collation: "latin1_bin"},
		"character(20)":         {Type: "char(20)", Nullable: "NO", Default: "none", Comment: "NOT NULL in a comment", Collation: "latin1_bin"},
		"nvarchar(30)":          {Type: "varchar(30)", Nullable: "NO", Default: "none", Comment: "NOT NULL in a comment", Collation: "utf8mb3_general_ci"},
		"national char(10)":     {Type: "char(10)", Nullable: "NO", Default: "none", Comment: "NOT NULL in a comment", Collation: "utf8mb3_general_ci"},
	} {
		t.Run(typ, func(t *testing.T) {
			db, schema := openMySQLTestDB(t)
			ops := []tableOp{{Op: "alter_column_type", Column: &columnSpec{Name: "label", Type: typ}}}
			if ddl, err := applyMySQLAlter(t, db, schema, "settings", ops); err != nil {
				t.Fatalf("alter: %v\n%s", err, strings.Join(ddl, "\n"))
			}
			if got := mysqlColumnStates(t, db, schema, "settings")["label"]; got != want {
				t.Errorf("label = %+v\nwant %+v", got, want)
			}
		})
	}
}

// TestAlterColumnMySQLQuotingModes runs the designer on connections whose
// SHOW CREATE TABLE output uses double quotes or leaves identifiers bare.
func TestAlterColumnMySQLQuotingModes(t *testing.T) {
	for name, params := range map[string]string{
		"ANSI_QUOTES":             "sql_mode=%27ANSI_QUOTES%27",
		"ANSI":                    "sql_mode=%27ANSI%27",
		"sql_quote_show_create=0": "sql_quote_show_create=0",
	} {
		t.Run(name, func(t *testing.T) {
			_, schema := openMySQLTestDB(t)
			dsn := os.Getenv("KIPPER_TEST_MYSQL_DSN")
			sep := "?"
			if strings.Contains(dsn, "?") {
				sep = "&"
			}
			db, err := sql.Open("mysql", dsn+sep+params)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			ops := []tableOp{
				{Op: "set_nullable", Column: &columnSpec{Name: "we`ird", Nullable: false}},
				{Op: "alter_column_type", Column: &columnSpec{Name: "label", Type: "varchar(60)"}},
			}
			if ddl, err := applyMySQLAlter(t, db, schema, "settings", ops); err != nil {
				t.Fatalf("alter: %v\n%s", err, strings.Join(ddl, "\n"))
			}
			got := mysqlColumnStates(t, db, schema, "settings")
			if got["we`ird"].Nullable != "NO" || got["label"].Type != "varchar(60)" || got["label"].Default != "none" {
				t.Errorf("we`ird = %+v, label = %+v", got["we`ird"], got["label"])
			}
			var showCreate int
			if err := db.QueryRow("SELECT @@SESSION.sql_quote_show_create").Scan(&showCreate); err != nil {
				t.Fatal(err)
			}
			if want := map[bool]int{true: 0, false: 1}[name == "sql_quote_show_create=0"]; showCreate != want {
				t.Errorf("sql_quote_show_create = %d after the read, want the session's own %d", showCreate, want)
			}
		})
	}
}

func TestStandardStringLiterals(t *testing.T) {
	cases := []struct {
		ansiQuotes bool
		in, want   string
	}{
		{false, ` DEFAULT 'a\\b\'c' COMMENT 'x\\y'`, ` DEFAULT 'a\b''c' COMMENT 'x\y'`},
		{false, ` DEFAULT 'l1\nl2\r\Z\0'`, " DEFAULT 'l1\nl2\r\x1a\x00'"},
		{false, ` DEFAULT (concat(` + "`a\\'b``c`" + `, 'd\\'))`, ` DEFAULT (concat(` + "`a\\'b``c`" + `, 'd\'))`},
		{true, ` DEFAULT (concat("it's\\", 'e\\'))`, ` DEFAULT (concat("it's\\", 'e\'))`},
		// Expressions are printed with backticks even under ANSI_QUOTES.
		{true, ` DEFAULT ((` + "`a'\\nb`" + ` + 1))`, ` DEFAULT ((` + "`a'\\nb`" + ` + 1))`},
		{false, `enum('a\\b','it\'s')`, `enum('a\b','it''s')`},
		{false, ` NOT NULL DEFAULT 'plain'`, ` NOT NULL DEFAULT 'plain'`},
	}
	for _, c := range cases {
		if got := standardStringLiterals(c.in, c.ansiQuotes); got != c.want {
			t.Errorf("standardStringLiterals(%q, %v)\n got %q\nwant %q", c.in, c.ansiQuotes, got, c.want)
		}
	}
}

// TestAlterColumnMySQLNoBackslashEscapes checks that changing a column preserves
// backslashes in its default and comment under NO_BACKSLASH_ESCAPES. Escapes in
// the copied SHOW CREATE TABLE definition must be converted for this mode.
func TestAlterColumnMySQLNoBackslashEscapes(t *testing.T) {
	_, schema := openMySQLTestDB(t)
	dsn := os.Getenv("KIPPER_TEST_MYSQL_DSN")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("mysql", dsn+sep+"sql_mode=%27NO_BACKSLASH_ESCAPES%27")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	create := "CREATE TABLE " + quoteIdent("mysql", schema) + `.bs (v VARCHAR(20) DEFAULT 'a\b''c' COMMENT 'x\y')` //nolint:gosec // The schema is the test's own generated name.
	if _, err := db.Exec(create); err != nil {
		t.Fatal(err)
	}
	ops := []tableOp{{Op: "alter_column_type", Column: &columnSpec{Name: "v", Type: "varchar(30)"}}}
	if ddl, err := applyMySQLAlter(t, db, schema, "bs", ops); err != nil {
		t.Fatalf("alter: %v\n%s", err, strings.Join(ddl, "\n"))
	}
	var def, comment string
	if err := db.QueryRow("SELECT HEX(column_default), column_comment FROM information_schema.columns WHERE table_schema = ? AND table_name = 'bs' AND column_name = 'v'", schema).Scan(&def, &comment); err != nil {
		t.Fatal(err)
	}
	if def != "615C622763" || comment != `x\y` {
		t.Errorf("default hex = %s, comment = %q, want 615C622763 (a\\b'c) and x\\y", def, comment)
	}
	var mode string
	if err := db.QueryRow("SELECT @@SESSION.sql_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "NO_BACKSLASH_ESCAPES" {
		t.Errorf("sql_mode = %q after the alter, want the session's own NO_BACKSLASH_ESCAPES", mode)
	}
}

// TestAlterColumnMySQLNoBackslashEscapesKeepsNewLiterals checks that a single
// batch preserves backslashes in both copied definitions and new string literals
// under NO_BACKSLASH_ESCAPES, matching the SQL editor's behavior.
func TestAlterColumnMySQLNoBackslashEscapesKeepsNewLiterals(t *testing.T) {
	_, schema := openMySQLTestDB(t)
	dsn := os.Getenv("KIPPER_TEST_MYSQL_DSN")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("mysql", dsn+sep+"sql_mode=%27NO_BACKSLASH_ESCAPES%27")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	create := "CREATE TABLE " + quoteIdent("mysql", schema) + `.bs (v VARCHAR(20) DEFAULT 'a\b''c' COMMENT 'x\y', u VARCHAR(20) DEFAULT 'old', e ENUM('a\b','it''s') NULL)` //nolint:gosec // The schema is the test's own generated name.
	if _, err := db.Exec(create); err != nil {
		t.Fatal(err)
	}
	enumType := func() string {
		var typ string
		if err := db.QueryRow("SELECT HEX(column_type) FROM information_schema.columns WHERE table_schema = ? AND table_name = 'bs' AND column_name = 'e'", schema).Scan(&typ); err != nil {
			t.Fatal(err)
		}
		return typ
	}
	enumBefore := enumType()
	ops := []tableOp{
		{Op: "alter_column_type", Column: &columnSpec{Name: "v", Type: "varchar(30)"}},
		{Op: "set_nullable", Column: &columnSpec{Name: "e", Nullable: false}},
		{Op: "set_default", Column: &columnSpec{Name: "u", Default: strPtr(`'a\nb'`)}},
		{Op: "add_column", Column: &columnSpec{Name: "w", Type: "varchar(10)", Nullable: true, Default: strPtr(`'p\q'`)}},
	}
	if ddl, err := applyMySQLAlter(t, db, schema, "bs", ops); err != nil {
		t.Fatalf("alter: %v\n%s", err, strings.Join(ddl, "\n"))
	}
	if got := enumType(); got != enumBefore {
		t.Errorf("enum members changed: column_type hex %s, want %s", got, enumBefore)
	}
	want := map[string]string{"v": "615C622763", "u": "615C6E62", "w": "705C71"}
	for col, hex := range want {
		var got string
		if err := db.QueryRow("SELECT HEX(column_default) FROM information_schema.columns WHERE table_schema = ? AND table_name = 'bs' AND column_name = ?", schema, col).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != hex {
			t.Errorf("%s default hex = %s, want %s", col, got, hex)
		}
	}
}

// TestAlterColumnMySQLRefusesUnknownDefinitions checks that each batch is
// refused while the statements are built, before anything runs.
func TestAlterColumnMySQLRefusesUnknownDefinitions(t *testing.T) {
	db, schema := openMySQLTestDB(t)
	cases := map[string]struct {
		table string
		ops   []tableOp
	}{
		"generated column": {"settings", []tableOp{{Op: "set_nullable", Column: &columnSpec{Name: "doubled", Nullable: false}}}},
		"column added in the same batch": {"settings", []tableOp{
			{Op: "add_column", Column: &columnSpec{Name: "extra", Type: "int", Nullable: true}},
			{Op: "set_nullable", Column: &columnSpec{Name: "extra", Nullable: false}},
		}},
		"dropped column": {"settings", []tableOp{
			{Op: "drop_column", Name: "body"},
			{Op: "alter_column_type", Column: &columnSpec{Name: "body", Type: "mediumtext"}},
		}},
		"unknown column": {"settings", []tableOp{{Op: "alter_column_type", Column: &columnSpec{Name: "missing", Type: "int"}}}},
		"default changed in the same batch": {"settings", []tableOp{
			{Op: "set_default", Column: &columnSpec{Name: "label", Default: strPtr("'other'")}},
			{Op: "set_nullable", Column: &columnSpec{Name: "label", Nullable: true}},
		}},
		"default changed under another spelling": {"settings", []tableOp{
			{Op: "set_default", Column: &columnSpec{Name: "LABEL", Default: strPtr("'other'")}},
			{Op: "alter_column_type", Column: &columnSpec{Name: "label", Type: "varchar(80)"}},
		}},
		"column dropped under another spelling": {"settings", []tableOp{
			{Op: "drop_column", Name: "Body"},
			{Op: "alter_column_type", Column: &columnSpec{Name: "body", Type: "mediumtext"}},
		}},
		"primary key added in the same batch": {"nokey", []tableOp{
			{Op: "add_constraint", Constraint: &constraintSpec{Type: "PRIMARY KEY", Columns: []string{"id"}}},
			{Op: "alter_column_type", Column: &columnSpec{Name: "id", Type: "bigint"}},
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if ddl, err := buildMySQLAlter(t, db, schema, c.table, c.ops); err == nil {
				t.Errorf("expected the batch to be refused, got DDL %q", ddl)
			}
		})
	}
}

func TestReadSchemaMySQL(t *testing.T) {
	db, schema := openMySQLTestDB(t)
	resp, err := readSchema(context.Background(), db, "mysql")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Dialect != "mysql" {
		t.Errorf("dialect = %q, want mysql", resp.Dialect)
	}
	found := false
	for _, d := range resp.Databases {
		for _, s := range d.Schemas {
			if s.Name == schema {
				found = len(s.Relations) > 0
			}
		}
	}
	if !found {
		t.Errorf("schema %s with its tables is missing from %+v", schema, resp.Databases)
	}
}

// TestCreateTableMySQLWithConsoleDefaults checks the designer's MySQL defaults
// and preset types from console/src/utils/columnTypes.ts. The added integer
// columns omit AUTO_INCREMENT because the table already has an auto-increment id.
func TestCreateTableMySQLWithConsoleDefaults(t *testing.T) {
	db, schema := openMySQLTestDB(t)
	ctx := context.Background()
	now := "CURRENT_TIMESTAMP"
	ddl, err := buildCreateTable("mysql", createTableRequest{
		Schema: schema,
		Name:   "made",
		Columns: []columnSpec{
			{Name: "id", Type: "bigint unsigned auto_increment"},
			{Name: "created_at", Type: "datetime", Default: &now},
		},
		Constraints: []constraintSpec{{Type: "PRIMARY KEY", Columns: []string{"id"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := execInTx(ctx, db, []string{ddl}); err != nil {
		t.Fatalf("%s: %v", ddl, err)
	}
	presets := []string{
		"varchar(255)", "text", "char(36)", "mediumtext", "longtext",
		"int", "bigint", "smallint", "tinyint", "decimal(10,2)", "double", "float",
		"int unsigned",
		"boolean",
		"datetime", "timestamp", "date", "time", "year",
		"json",
		"blob", "varbinary(255)", "binary(16)",
	}
	var ops []tableOp
	for i, typ := range presets {
		ops = append(ops, tableOp{Op: "add_column", Column: &columnSpec{Name: fmt.Sprintf("c%d", i), Type: typ, Nullable: true}})
	}
	if _, err := applyMySQLAlter(t, db, schema, "made", ops); err != nil {
		t.Fatalf("add preset columns: %v", err)
	}
	got := mysqlColumnStates(t, db, schema, "made")
	if c := got["id"]; c.Extra != "auto_increment" || c.Nullable != "NO" {
		t.Errorf("id = %+v, want an auto-increment NOT NULL column", c)
	}
	if c := got["created_at"]; c.Default != "CURRENT_TIMESTAMP" {
		t.Errorf("created_at = %+v, want DEFAULT CURRENT_TIMESTAMP", c)
	}

	ddl, err = buildCreateTable("mysql", createTableRequest{
		Schema:      schema,
		Name:        "made_small",
		Columns:     []columnSpec{{Name: "id", Type: "int unsigned auto_increment"}},
		Constraints: []constraintSpec{{Type: "PRIMARY KEY", Columns: []string{"id"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := execInTx(ctx, db, []string{ddl}); err != nil {
		t.Fatalf("%s: %v", ddl, err)
	}
	if c := mysqlColumnStates(t, db, schema, "made_small")["id"]; c.Extra != "auto_increment" {
		t.Errorf("made_small.id = %+v, want an auto-increment column", c)
	}
}
