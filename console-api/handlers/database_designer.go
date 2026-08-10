// Package handlers — database_designer.go provides the visual table
// and index designer surface. The browser sends a typed list of
// operations; the server generates dialect-correct DDL via the
// builders here and either previews it or executes it in a single
// transaction. CONCURRENT index ops on Postgres run outside the
// transaction because Postgres forbids them inside one.
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// columnSpec is the input shape for column-level operations. Default
// is a pointer so the caller can distinguish "no default" from
// "default is the empty string".
type columnSpec struct {
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	Nullable      bool    `json:"nullable"`
	Default       *string `json:"default,omitempty"`
	GeneratedExpr string  `json:"generated_expr,omitempty"`
	Comment       string  `json:"comment,omitempty"`
}

type constraintSpec struct {
	Name       string   `json:"name,omitempty"`
	Type       string   `json:"type"` // "PRIMARY KEY" | "FOREIGN KEY" | "UNIQUE" | "CHECK"
	Columns    []string `json:"columns,omitempty"`
	RefSchema  string   `json:"ref_schema,omitempty"`
	RefTable   string   `json:"ref_table,omitempty"`
	RefColumns []string `json:"ref_columns,omitempty"`
	OnDelete   string   `json:"on_delete,omitempty"`
	OnUpdate   string   `json:"on_update,omitempty"`
	CheckExpr  string   `json:"check_expr,omitempty"`
}

// tableOp is one entry in a PATCH /db/tables/{schema}/{table} request.
// Each op maps to one DDL statement; the server runs them in order
// inside a transaction.
type tableOp struct {
	Op             string          `json:"op"`
	Column         *columnSpec     `json:"column,omitempty"`
	Name           string          `json:"name,omitempty"`
	OldName        string          `json:"old_name,omitempty"`
	NewName        string          `json:"new_name,omitempty"`
	Constraint     *constraintSpec `json:"constraint,omitempty"`
	ConstraintName string          `json:"constraint_name,omitempty"`
}

// createTableRequest is the body for POST /db/tables.
type createTableRequest struct {
	Schema      string           `json:"schema"`
	Name        string           `json:"name"`
	Columns     []columnSpec     `json:"columns"`
	Constraints []constraintSpec `json:"constraints,omitempty"`
}

// alterTableRequest is the body for PATCH /db/tables/{schema}/{table}.
type alterTableRequest struct {
	Ops []tableOp `json:"ops"`
}

// indexRequest is the body for POST /db/indexes and is also used for
// the preview endpoint when generating a single CREATE INDEX statement.
type indexRequest struct {
	Schema     string   `json:"schema"`
	Table      string   `json:"table"`
	Name       string   `json:"name"`
	Columns    []string `json:"columns"`
	Unique     bool     `json:"unique"`
	Method     string   `json:"method,omitempty"` // btree, gin, gist, hash
	Where      string   `json:"where,omitempty"`
	Concurrent bool     `json:"concurrent,omitempty"`
}

// previewRequest is the body for POST /db/ddl/preview. Exactly one of
// the action fields should be populated; the server returns the SQL it
// would execute without running anything.
type previewRequest struct {
	CreateTable *createTableRequest `json:"create_table,omitempty"`
	AlterTable  *struct {
		Schema string    `json:"schema"`
		Table  string    `json:"table"`
		Ops    []tableOp `json:"ops"`
	} `json:"alter_table,omitempty"`
	CreateIndex *indexRequest `json:"create_index,omitempty"`
	DropIndex   *struct {
		Schema     string `json:"schema"`
		Name       string `json:"name"`
		Concurrent bool   `json:"concurrent,omitempty"`
	} `json:"drop_index,omitempty"`
}

// CreateTable builds and executes a CREATE TABLE statement in a
// transaction. Returns the executed DDL.
// POST /api/v1/services/{name}/db/tables
func (d *Database) CreateTable(w http.ResponseWriter, r *http.Request) {
	svcName, namespace, ok := requireService(w, r)
	if !ok {
		return
	}

	var req createTableRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Schema == "" || req.Name == "" || len(req.Columns) == 0 {
		respondError(w, http.StatusBadRequest, "schema, name, and at least one column are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), defaultQueryTimeout+5*time.Second)
	defer cancel()

	db, info, err := d.connect(ctx, svcName, namespace, r.URL.Query().Get("database"))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	ddl, err := buildCreateTable(info.driver, req)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := execInTx(ctx, db, []string{ddl}); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"ddl": []string{ddl}})
}

// AlterTable applies an ordered list of operations to a table inside
// a single transaction. Returns the executed DDL list.
// PATCH /api/v1/services/{name}/db/tables/{schema}/{table}
func (d *Database) AlterTable(w http.ResponseWriter, r *http.Request) {
	svcName, namespace, ok := requireService(w, r)
	if !ok {
		return
	}
	schemaName := chi.URLParam(r, "schema")
	table := chi.URLParam(r, "table")

	var req alterTableRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Ops) == 0 {
		respondError(w, http.StatusBadRequest, "at least one op is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), defaultQueryTimeout+5*time.Second)
	defer cancel()

	db, info, err := d.connect(ctx, svcName, namespace, r.URL.Query().Get("database"))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	ddl, err := buildAlterTable(info.driver, schemaName, table, req.Ops)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := execInTx(ctx, db, ddl); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"ddl": ddl})
}

// CreateIndex builds and runs a CREATE INDEX statement. CONCURRENTLY
// (Postgres only) runs outside any transaction because Postgres
// forbids concurrent index creation inside one.
// POST /api/v1/services/{name}/db/indexes
func (d *Database) CreateIndex(w http.ResponseWriter, r *http.Request) {
	svcName, namespace, ok := requireService(w, r)
	if !ok {
		return
	}

	var req indexRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Schema == "" || req.Table == "" || req.Name == "" || len(req.Columns) == 0 {
		respondError(w, http.StatusBadRequest, "schema, table, name, and columns are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), defaultQueryTimeout*2)
	defer cancel()

	db, info, err := d.connect(ctx, svcName, namespace, r.URL.Query().Get("database"))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	ddl, err := buildCreateIndex(info.driver, req)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Concurrent && info.driver == "pgx" {
		// Concurrent ops cannot run in a transaction.
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		if err := execInTx(ctx, db, []string{ddl}); err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"ddl": []string{ddl}})
}

// DropIndex removes an index. CONCURRENTLY for Postgres opt-in.
// DELETE /api/v1/services/{name}/db/indexes/{schema}/{indexName}
func (d *Database) DropIndex(w http.ResponseWriter, r *http.Request) {
	svcName, namespace, ok := requireService(w, r)
	if !ok {
		return
	}
	schemaName := chi.URLParam(r, "schema")
	indexName := chi.URLParam(r, "indexName")
	concurrent := r.URL.Query().Get("concurrent") == "true"

	ctx, cancel := context.WithTimeout(r.Context(), defaultQueryTimeout*2)
	defer cancel()

	db, info, err := d.connect(ctx, svcName, namespace, r.URL.Query().Get("database"))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	ddl := buildDropIndex(info.driver, schemaName, indexName, concurrent)

	if concurrent && info.driver == "pgx" {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		if err := execInTx(ctx, db, []string{ddl}); err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"ddl": []string{ddl}})
}

// PreviewDDL takes the same shape as the executing endpoints but never
// touches the database. Used by the live DDL preview pane on the
// designer so the user can read what's about to run before clicking
// Apply.
// POST /api/v1/services/{name}/db/ddl/preview
func (d *Database) PreviewDDL(w http.ResponseWriter, r *http.Request) {
	svcName, namespace, ok := requireService(w, r)
	if !ok {
		return
	}

	var req previewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	_, info, err := d.connect(ctx, svcName, namespace, r.URL.Query().Get("database"))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	switch {
	case req.CreateTable != nil:
		ddl, err := buildCreateTable(info.driver, *req.CreateTable)
		if err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"ddl": []string{ddl}})
	case req.AlterTable != nil:
		ddl, err := buildAlterTable(info.driver, req.AlterTable.Schema, req.AlterTable.Table, req.AlterTable.Ops)
		if err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"ddl": ddl})
	case req.CreateIndex != nil:
		ddl, err := buildCreateIndex(info.driver, *req.CreateIndex)
		if err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{"ddl": []string{ddl}})
	case req.DropIndex != nil:
		ddl := buildDropIndex(info.driver, req.DropIndex.Schema, req.DropIndex.Name, req.DropIndex.Concurrent)
		respondJSON(w, http.StatusOK, map[string]interface{}{"ddl": []string{ddl}})
	default:
		respondError(w, http.StatusBadRequest, "exactly one of create_table, alter_table, create_index, drop_index is required")
	}
}

// execInTx runs a list of statements inside one transaction. Any
// failure rolls everything back. We prefer atomicity over partial
// success so a failed alter never leaves the schema in a half state.
func execInTx(ctx context.Context, db *sql.DB, stmts []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	for _, s := range stmts {
		if strings.TrimSpace(s) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, s); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("exec %q: %w", truncate(s, 80), err)
		}
	}
	return tx.Commit()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// --- DDL builders ---

func buildCreateTable(driver string, req createTableRequest) (string, error) {
	if len(req.Columns) == 0 {
		return "", fmt.Errorf("at least one column is required")
	}
	parts := make([]string, 0, len(req.Columns)+len(req.Constraints))
	for _, c := range req.Columns {
		def, err := buildColumnDef(driver, c)
		if err != nil {
			return "", err
		}
		parts = append(parts, "  "+def)
	}
	for _, c := range req.Constraints {
		clause, err := buildConstraintClause(driver, c)
		if err != nil {
			return "", err
		}
		parts = append(parts, "  "+clause)
	}
	return fmt.Sprintf("CREATE TABLE %s (\n%s\n)",
		qualified(driver, req.Schema, req.Name),
		strings.Join(parts, ",\n"),
	), nil
}

func buildAlterTable(driver, schema, table string, ops []tableOp) ([]string, error) {
	out := make([]string, 0, len(ops))
	qual := qualified(driver, schema, table)
	for _, op := range ops {
		switch op.Op {
		case "add_column":
			if op.Column == nil {
				return nil, fmt.Errorf("add_column requires a column spec")
			}
			def, err := buildColumnDef(driver, *op.Column)
			if err != nil {
				return nil, err
			}
			out = append(out, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", qual, def))
		case "drop_column":
			name := op.Name
			if name == "" && op.Column != nil {
				name = op.Column.Name
			}
			if name == "" {
				return nil, fmt.Errorf("drop_column requires a column name")
			}
			out = append(out, fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", qual, quoteIdent(driver, name)))
		case "rename_column":
			if op.OldName == "" || op.NewName == "" {
				return nil, fmt.Errorf("rename_column requires old_name and new_name")
			}
			// MySQL 8+ supports RENAME COLUMN, matching PG's syntax.
			out = append(out, fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s",
				qual, quoteIdent(driver, op.OldName), quoteIdent(driver, op.NewName)))
		case "alter_column_type":
			if op.Column == nil {
				return nil, fmt.Errorf("alter_column_type requires a column spec")
			}
			if driver == "pgx" {
				out = append(out, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s TYPE %s",
					qual, quoteIdent(driver, op.Column.Name), op.Column.Type))
			} else {
				// MySQL collapses several alter-column ops into MODIFY COLUMN.
				def, err := buildColumnDef(driver, *op.Column)
				if err != nil {
					return nil, err
				}
				out = append(out, fmt.Sprintf("ALTER TABLE %s MODIFY COLUMN %s", qual, def))
			}
		case "set_nullable":
			if op.Column == nil {
				return nil, fmt.Errorf("set_nullable requires a column spec")
			}
			if driver == "pgx" {
				if op.Column.Nullable {
					out = append(out, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL",
						qual, quoteIdent(driver, op.Column.Name)))
				} else {
					out = append(out, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL",
						qual, quoteIdent(driver, op.Column.Name)))
				}
			} else {
				def, err := buildColumnDef(driver, *op.Column)
				if err != nil {
					return nil, err
				}
				out = append(out, fmt.Sprintf("ALTER TABLE %s MODIFY COLUMN %s", qual, def))
			}
		case "set_default":
			if op.Column == nil {
				return nil, fmt.Errorf("set_default requires a column spec")
			}
			if op.Column.Default == nil {
				out = append(out, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT",
					qual, quoteIdent(driver, op.Column.Name)))
			} else {
				out = append(out, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s",
					qual, quoteIdent(driver, op.Column.Name), *op.Column.Default))
			}
		case "add_constraint":
			if op.Constraint == nil {
				return nil, fmt.Errorf("add_constraint requires a constraint spec")
			}
			clause, err := buildConstraintClause(driver, *op.Constraint)
			if err != nil {
				return nil, err
			}
			out = append(out, fmt.Sprintf("ALTER TABLE %s ADD %s", qual, clause))
		case "drop_constraint":
			if op.ConstraintName == "" {
				return nil, fmt.Errorf("drop_constraint requires constraint_name")
			}
			out = append(out, fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s",
				qual, quoteIdent(driver, op.ConstraintName)))
		case "rename_table":
			if op.NewName == "" {
				return nil, fmt.Errorf("rename_table requires new_name")
			}
			if driver == "mysql" {
				out = append(out, fmt.Sprintf("RENAME TABLE %s TO %s",
					qual, qualified(driver, schema, op.NewName)))
			} else {
				out = append(out, fmt.Sprintf("ALTER TABLE %s RENAME TO %s",
					qual, quoteIdent(driver, op.NewName)))
			}
		default:
			return nil, fmt.Errorf("unknown op %q", op.Op)
		}
	}
	return out, nil
}

func buildColumnDef(driver string, c columnSpec) (string, error) {
	if c.Name == "" || c.Type == "" {
		return "", fmt.Errorf("column %q is missing name or type", c.Name)
	}
	parts := []string{quoteIdent(driver, c.Name), c.Type}
	if c.GeneratedExpr != "" {
		// GENERATED ALWAYS AS (expr) STORED is the portable shape.
		parts = append(parts, fmt.Sprintf("GENERATED ALWAYS AS (%s) STORED", c.GeneratedExpr))
	}
	if !c.Nullable {
		parts = append(parts, "NOT NULL")
	}
	if c.Default != nil {
		parts = append(parts, "DEFAULT "+*c.Default)
	}
	return strings.Join(parts, " "), nil
}

func buildConstraintClause(driver string, c constraintSpec) (string, error) {
	if c.Type == "" {
		return "", fmt.Errorf("constraint type is required")
	}
	prefix := ""
	if c.Name != "" {
		prefix = "CONSTRAINT " + quoteIdent(driver, c.Name) + " "
	}
	switch strings.ToUpper(c.Type) {
	case "PRIMARY KEY":
		if len(c.Columns) == 0 {
			return "", fmt.Errorf("primary key requires columns")
		}
		return prefix + "PRIMARY KEY (" + joinIdents(driver, c.Columns) + ")", nil
	case "UNIQUE":
		if len(c.Columns) == 0 {
			return "", fmt.Errorf("unique constraint requires columns")
		}
		return prefix + "UNIQUE (" + joinIdents(driver, c.Columns) + ")", nil
	case "FOREIGN KEY":
		if len(c.Columns) == 0 || c.RefTable == "" || len(c.RefColumns) == 0 {
			return "", fmt.Errorf("foreign key requires columns, ref_table, and ref_columns")
		}
		var ref string
		if c.RefSchema != "" {
			ref = qualified(driver, c.RefSchema, c.RefTable)
		} else {
			ref = quoteIdent(driver, c.RefTable)
		}
		clause := fmt.Sprintf("%sFOREIGN KEY (%s) REFERENCES %s (%s)",
			prefix, joinIdents(driver, c.Columns), ref, joinIdents(driver, c.RefColumns))
		if c.OnDelete != "" {
			clause += " ON DELETE " + strings.ToUpper(c.OnDelete)
		}
		if c.OnUpdate != "" {
			clause += " ON UPDATE " + strings.ToUpper(c.OnUpdate)
		}
		return clause, nil
	case "CHECK":
		if c.CheckExpr == "" {
			return "", fmt.Errorf("check constraint requires check_expr")
		}
		return prefix + "CHECK (" + c.CheckExpr + ")", nil
	default:
		return "", fmt.Errorf("unknown constraint type %q", c.Type)
	}
}

func buildCreateIndex(driver string, req indexRequest) (string, error) {
	if len(req.Columns) == 0 {
		return "", fmt.Errorf("columns are required")
	}
	uniq := ""
	if req.Unique {
		uniq = "UNIQUE "
	}
	concurrent := ""
	if req.Concurrent && driver == "pgx" {
		concurrent = "CONCURRENTLY "
	}
	method := ""
	if req.Method != "" && driver == "pgx" {
		method = " USING " + req.Method
	} else if req.Method != "" && driver == "mysql" {
		method = " USING " + req.Method
	}
	where := ""
	if req.Where != "" && driver == "pgx" {
		where = " WHERE " + req.Where
	}
	return fmt.Sprintf("CREATE %sINDEX %s%s ON %s%s (%s)%s",
		uniq,
		concurrent,
		quoteIdent(driver, req.Name),
		qualified(driver, req.Schema, req.Table),
		method,
		joinIdents(driver, req.Columns),
		where,
	), nil
}

func buildDropIndex(driver, schema, name string, concurrent bool) string {
	if driver == "mysql" {
		// MySQL needs the table name; users will hit DROP INDEX via ALTER TABLE
		// in practice. Emit the simpler form and let the server return an
		// actionable error if the dialect rejects it.
		return fmt.Sprintf("DROP INDEX %s ON %s", quoteIdent(driver, name), quoteIdent(driver, schema))
	}
	c := ""
	if concurrent {
		c = "CONCURRENTLY "
	}
	return fmt.Sprintf("DROP INDEX %s%s", c, qualified(driver, schema, name))
}

func joinIdents(driver string, names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = quoteIdent(driver, n)
	}
	return strings.Join(q, ", ")
}
