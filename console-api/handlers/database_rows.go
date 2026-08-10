// Package handlers — database_rows.go provides the row-level CRUD
// surface used by the TableBrowser. All identifier quoting flows
// through quoteIdent so user input never reaches the SQL string;
// values are always parameterised.
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const defaultRowPageSize = 50

// columnInfo carries enough column metadata for the row editor to
// render an input, validate edits, and recognise server-generated
// columns (sequences / GENERATED ALWAYS) which shouldn't show edit
// affordances by default.
type columnInfo struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Nullable  bool   `json:"nullable"`
	Default   string `json:"default,omitempty"`
	Generated bool   `json:"generated"`
	Position  int    `json:"position"`
}

// fkInfo describes a single foreign-key column → references-relation
// edge so the UI can render an FK chip.
type fkInfo struct {
	Column    string `json:"column"`
	RefSchema string `json:"ref_schema"`
	RefTable  string `json:"ref_table"`
	RefColumn string `json:"ref_column"`
}

// indexInfo describes a single index on a table.
type indexInfo struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique"`
	Primary bool     `json:"primary"`
	Method  string   `json:"method,omitempty"`
}

// tableStructure is the response shape for GET /db/tables/{schema}/{table}/structure
// and is also embedded into the rowsResponse so the browser doesn't
// need a second round trip on first page load.
type tableStructure struct {
	Schema      string       `json:"schema"`
	Name        string       `json:"name"`
	Columns     []columnInfo `json:"columns"`
	PrimaryKey  []string     `json:"primary_key"`
	ForeignKeys []fkInfo     `json:"foreign_keys"`
	Indexes     []indexInfo  `json:"indexes"`
}

type rowsResponse struct {
	Structure  tableStructure  `json:"structure"`
	Rows       [][]interface{} `json:"rows"`
	Total      int64           `json:"total"`
	Limit      int             `json:"limit"`
	Offset     int             `json:"offset"`
	DurationMs int64           `json:"duration_ms"`
}

// ListRows returns paginated rows from a single table along with its
// column metadata, primary key, and foreign keys.
// GET /api/v1/services/{name}/db/tables/{schema}/{table}/rows
func (d *Database) ListRows(w http.ResponseWriter, r *http.Request) {
	svcName, namespace, ok := requireService(w, r)
	if !ok {
		return
	}
	schema := chi.URLParam(r, "schema")
	table := chi.URLParam(r, "table")

	limit := parseIntDefault(r.URL.Query().Get("limit"), defaultRowPageSize)
	if limit <= 0 || limit > maxRowLimit {
		limit = defaultRowPageSize
	}
	offset := parseIntDefault(r.URL.Query().Get("offset"), 0)
	if offset < 0 {
		offset = 0
	}
	orderBy := r.URL.Query().Get("order_by")
	orderDir := r.URL.Query().Get("order_dir")
	filterCol := r.URL.Query().Get("filter_col")
	filterVal := r.URL.Query().Get("filter_val")
	// An empty value means "no filter applied yet" — don't build a
	// WHERE clause that would compare against '' and fail for typed
	// columns like bigint or uuid.
	if filterVal == "" {
		filterCol = ""
	}

	ctx, cancel := context.WithTimeout(r.Context(), defaultQueryTimeout+5*time.Second)
	defer cancel()

	db, info, err := d.connect(ctx, svcName, namespace, r.URL.Query().Get("database"))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	structure, err := readTableStructure(ctx, db, info.driver, schema, table)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("read structure: %v", err))
		return
	}
	if len(structure.Columns) == 0 {
		respondError(w, http.StatusNotFound, fmt.Sprintf("table %s.%s not found", schema, table))
		return
	}

	// Validate orderBy/filterCol against the real column list — never
	// concatenate user input into the SQL string.
	colNames := map[string]bool{}
	for _, c := range structure.Columns {
		colNames[c.Name] = true
	}
	if orderBy != "" && !colNames[orderBy] {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("unknown order_by column %q", orderBy))
		return
	}
	if filterCol != "" && !colNames[filterCol] {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("unknown filter column %q", filterCol))
		return
	}

	// Default ordering: by primary key when present so pagination is stable.
	if orderBy == "" && len(structure.PrimaryKey) > 0 {
		orderBy = structure.PrimaryKey[0]
	}
	dir := strings.ToUpper(strings.TrimSpace(orderDir))
	if dir != "ASC" && dir != "DESC" {
		dir = "ASC"
	}

	q := buildSelectRows(info.driver, schema, table, orderBy, dir, limit, offset, filterCol)
	cq := buildCountRows(info.driver, schema, table, filterCol)

	queryCtx, queryCancel := context.WithTimeout(ctx, defaultQueryTimeout)
	defer queryCancel()
	start := time.Now()

	args := []interface{}{}
	if filterCol != "" {
		args = append(args, filterVal)
	}

	rows, err := db.QueryContext(queryCtx, q, args...)
	if err != nil {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("query: %v", err))
		return
	}
	defer func() { _ = rows.Close() }()

	var result rowsResponse
	result.Structure = structure
	result.Limit = limit
	result.Offset = offset

	for rows.Next() {
		scanTargets := make([]interface{}, len(structure.Columns))
		scanPtrs := make([]interface{}, len(structure.Columns))
		for i := range scanTargets {
			scanPtrs[i] = &scanTargets[i]
		}
		if err := rows.Scan(scanPtrs...); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("scan: %v", err))
			return
		}
		result.Rows = append(result.Rows, normaliseRow(scanTargets))
	}
	if err := rows.Err(); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("rows: %v", err))
		return
	}

	// Best-effort total count for pagination.
	var total int64
	if err := db.QueryRowContext(queryCtx, cq, args...).Scan(&total); err == nil { //nolint:gosec // identifiers validated against introspected schema; values pass as placeholders
		result.Total = total
	}

	result.DurationMs = time.Since(start).Milliseconds()

	if result.Rows == nil {
		result.Rows = [][]interface{}{}
	}
	respondJSON(w, http.StatusOK, result)
}

// InsertRow inserts a row from a flat key→value JSON body. It uses
// RETURNING * on Postgres / LAST_INSERT_ID() on MySQL to surface
// server-generated columns back to the UI.
// POST /api/v1/services/{name}/db/tables/{schema}/{table}/rows
func (d *Database) InsertRow(w http.ResponseWriter, r *http.Request) {
	svcName, namespace, ok := requireService(w, r)
	if !ok {
		return
	}
	schema := chi.URLParam(r, "schema")
	table := chi.URLParam(r, "table")

	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body) == 0 {
		respondError(w, http.StatusBadRequest, "at least one column value is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), defaultQueryTimeout+5*time.Second)
	defer cancel()

	db, info, err := d.connect(ctx, svcName, namespace, r.URL.Query().Get("database"))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	structure, err := readTableStructure(ctx, db, info.driver, schema, table)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("read structure: %v", err))
		return
	}
	knownCols := map[string]bool{}
	for _, c := range structure.Columns {
		knownCols[c.Name] = true
	}
	cols := make([]string, 0, len(body))
	for k := range body {
		if !knownCols[k] {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("unknown column %q", k))
			return
		}
		cols = append(cols, k)
	}
	sort.Strings(cols)
	sortedVals := make([]interface{}, len(cols))
	for i, c := range cols {
		sortedVals[i] = body[c]
	}

	queryCtx, queryCancel := context.WithTimeout(ctx, defaultQueryTimeout)
	defer queryCancel()

	switch info.driver {
	case "pgx":
		q := buildInsertReturning(info.driver, schema, table, cols, len(structure.Columns))
		row := db.QueryRowContext(queryCtx, q, sortedVals...) //nolint:gosec // identifiers validated against introspected schema; values pass as placeholders
		inserted, err := scanSingleRow(row, structure)
		if err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("insert: %v", err))
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"row": inserted,
		})
	case "mysql":
		q := buildInsert(info.driver, schema, table, cols)
		res, err := db.ExecContext(queryCtx, q, sortedVals...)
		if err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("insert: %v", err))
			return
		}
		// Try to fetch the inserted row by last insert id when there is a
		// single-column auto-increment PK.
		var inserted []interface{}
		if id, err := res.LastInsertId(); err == nil && id > 0 && len(structure.PrimaryKey) == 1 {
			selQ := fmt.Sprintf("SELECT * FROM %s WHERE %s = ?", //nolint:gosec // identifiers validated against introspected schema; id passes as placeholder
				qualified(info.driver, schema, table),
				quoteIdent(info.driver, structure.PrimaryKey[0]))
			row := db.QueryRowContext(queryCtx, selQ, id) //nolint:gosec // identifiers validated against introspected schema; id passes as placeholder
			inserted, _ = scanSingleRow(row, structure)
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"row":           inserted,
			"rows_affected": affectedOr(res, 1),
		})
	default:
		respondError(w, http.StatusBadRequest, "unsupported driver")
	}
}

// UpdateRow patches a row. Body: { pk: {col:val,...}, changes: {col:val,...} }.
// Rejected if the table has no primary key.
// PATCH /api/v1/services/{name}/db/tables/{schema}/{table}/rows
func (d *Database) UpdateRow(w http.ResponseWriter, r *http.Request) {
	svcName, namespace, ok := requireService(w, r)
	if !ok {
		return
	}
	schema := chi.URLParam(r, "schema")
	table := chi.URLParam(r, "table")

	var body struct {
		PK      map[string]interface{} `json:"pk"`
		Changes map[string]interface{} `json:"changes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body.PK) == 0 || len(body.Changes) == 0 {
		respondError(w, http.StatusBadRequest, "pk and changes are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), defaultQueryTimeout+5*time.Second)
	defer cancel()

	db, info, err := d.connect(ctx, svcName, namespace, r.URL.Query().Get("database"))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	structure, err := readTableStructure(ctx, db, info.driver, schema, table)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("read structure: %v", err))
		return
	}
	if len(structure.PrimaryKey) == 0 {
		respondError(w, http.StatusBadRequest, "table has no primary key: cannot update by PK")
		return
	}
	pkSet := map[string]bool{}
	for _, p := range structure.PrimaryKey {
		pkSet[p] = true
	}
	if len(body.PK) != len(structure.PrimaryKey) {
		respondError(w, http.StatusBadRequest, "pk must include every primary key column")
		return
	}
	for k := range body.PK {
		if !pkSet[k] {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a primary key column", k))
			return
		}
	}

	knownCols := map[string]bool{}
	for _, c := range structure.Columns {
		knownCols[c.Name] = true
	}
	for k := range body.Changes {
		if !knownCols[k] {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("unknown column %q", k))
			return
		}
		if pkSet[k] {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("primary key column %q cannot be changed via this endpoint", k))
			return
		}
	}

	changeCols := sortedKeys(body.Changes)
	pkCols := sortedKeys(body.PK)

	q, args := buildUpdate(info.driver, schema, table, changeCols, pkCols, body.Changes, body.PK)

	queryCtx, queryCancel := context.WithTimeout(ctx, defaultQueryTimeout)
	defer queryCancel()
	res, err := db.ExecContext(queryCtx, q, args...)
	if err != nil {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("update: %v", err))
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"rows_affected": affectedOr(res, 0),
	})
}

// DeleteRows removes rows matched by a list of PK values. Requires
// confirm:true to avoid accidental wipes.
// DELETE /api/v1/services/{name}/db/tables/{schema}/{table}/rows
func (d *Database) DeleteRows(w http.ResponseWriter, r *http.Request) {
	svcName, namespace, ok := requireService(w, r)
	if !ok {
		return
	}
	schema := chi.URLParam(r, "schema")
	table := chi.URLParam(r, "table")

	var body struct {
		PKs     []map[string]interface{} `json:"pks"`
		Confirm bool                     `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !body.Confirm {
		respondError(w, http.StatusBadRequest, "confirm:true is required")
		return
	}
	if len(body.PKs) == 0 {
		respondError(w, http.StatusBadRequest, "at least one pk is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), defaultQueryTimeout+5*time.Second)
	defer cancel()

	db, info, err := d.connect(ctx, svcName, namespace, r.URL.Query().Get("database"))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	structure, err := readTableStructure(ctx, db, info.driver, schema, table)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("read structure: %v", err))
		return
	}
	if len(structure.PrimaryKey) == 0 {
		respondError(w, http.StatusBadRequest, "table has no primary key: cannot delete by PK")
		return
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("begin: %v", err))
		return
	}
	var totalAffected int64
	for _, pk := range body.PKs {
		if len(pk) != len(structure.PrimaryKey) {
			_ = tx.Rollback()
			respondError(w, http.StatusBadRequest, "every pk must include every primary key column")
			return
		}
		pkCols := sortedKeys(pk)
		q, args := buildDelete(info.driver, schema, table, pkCols, pk)
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			_ = tx.Rollback()
			respondError(w, http.StatusBadRequest, fmt.Sprintf("delete: %v", err))
			return
		}
		totalAffected += affectedOr(res, 0)
	}
	if err := tx.Commit(); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("commit: %v", err))
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"rows_affected": totalAffected,
	})
}

// readTableStructure pulls columns + PK + FKs in three queries and
// returns a normalised structure regardless of driver.
func readTableStructure(ctx context.Context, db *sql.DB, driver, schema, table string) (tableStructure, error) {
	out := tableStructure{Schema: schema, Name: table, Columns: []columnInfo{}, PrimaryKey: []string{}, ForeignKeys: []fkInfo{}, Indexes: []indexInfo{}}

	const colQ = `
SELECT column_name, data_type, is_nullable, COALESCE(column_default,''),
       COALESCE(is_generated, ''), COALESCE(is_identity, '')
FROM information_schema.columns
WHERE table_schema = $1 AND table_name = $2
ORDER BY ordinal_position`

	const pkQ = `
SELECT kcu.column_name
FROM information_schema.table_constraints tc
JOIN information_schema.key_column_usage kcu USING (constraint_schema, constraint_name)
WHERE tc.constraint_type = 'PRIMARY KEY' AND tc.table_schema = $1 AND tc.table_name = $2
ORDER BY kcu.ordinal_position`

	const fkQ = `
SELECT kcu.column_name, ccu.table_schema, ccu.table_name, ccu.column_name
FROM information_schema.table_constraints tc
JOIN information_schema.key_column_usage kcu USING (constraint_schema, constraint_name)
JOIN information_schema.constraint_column_usage ccu USING (constraint_schema, constraint_name)
WHERE tc.constraint_type = 'FOREIGN KEY' AND tc.table_schema = $1 AND tc.table_name = $2`

	cQ, pQ, fQ := colQ, pkQ, fkQ
	if driver == "mysql" {
		cQ = strings.ReplaceAll(strings.ReplaceAll(colQ, "$1", "?"), "$2", "?")
		pQ = strings.ReplaceAll(strings.ReplaceAll(pkQ, "$1", "?"), "$2", "?")
		fQ = strings.ReplaceAll(strings.ReplaceAll(fkQ, "$1", "?"), "$2", "?")
	}

	rows, err := db.QueryContext(ctx, cQ, schema, table)
	if err != nil {
		return out, err
	}
	pos := 0
	for rows.Next() {
		var c columnInfo
		var nullable, gen, identity string
		if err := rows.Scan(&c.Name, &c.Type, &nullable, &c.Default, &gen, &identity); err != nil {
			_ = rows.Close()
			return out, err
		}
		c.Nullable = strings.EqualFold(nullable, "YES")
		c.Generated = strings.EqualFold(gen, "ALWAYS") || strings.EqualFold(identity, "ALWAYS")
		c.Position = pos
		pos++
		out.Columns = append(out.Columns, c)
	}
	_ = rows.Close()

	pkRows, err := db.QueryContext(ctx, pQ, schema, table)
	if err != nil {
		return out, err
	}
	for pkRows.Next() {
		var col string
		if err := pkRows.Scan(&col); err != nil {
			_ = pkRows.Close()
			return out, err
		}
		out.PrimaryKey = append(out.PrimaryKey, col)
	}
	_ = pkRows.Close()

	fkRows, err := db.QueryContext(ctx, fQ, schema, table)
	if err != nil {
		// FK info is best-effort — return what we have.
		return out, nil //nolint:nilerr // intentional: FK introspection failure should not fail the whole structure read
	}
	for fkRows.Next() {
		var fk fkInfo
		if err := fkRows.Scan(&fk.Column, &fk.RefSchema, &fk.RefTable, &fk.RefColumn); err != nil {
			_ = fkRows.Close()
			return out, err
		}
		out.ForeignKeys = append(out.ForeignKeys, fk)
	}
	_ = fkRows.Close()

	out.Indexes = readIndexes(ctx, db, driver, schema, table)
	return out, nil
}

// readIndexes pulls index metadata via dialect-specific catalogs. It's
// best-effort — failure returns an empty slice rather than failing the
// whole structure call, because the table view should still render even
// if index introspection breaks.
func readIndexes(ctx context.Context, db *sql.DB, driver, schema, table string) []indexInfo {
	switch driver {
	case "pgx":
		return readPostgresIndexes(ctx, db, schema, table)
	case "mysql":
		return readMySQLIndexes(ctx, db, schema, table)
	}
	return []indexInfo{}
}

func readPostgresIndexes(ctx context.Context, db *sql.DB, schema, table string) []indexInfo {
	const q = `
SELECT i.relname,
       array_agg(a.attname ORDER BY array_position(ix.indkey, a.attnum)) AS cols,
       ix.indisunique,
       ix.indisprimary,
       am.amname
FROM pg_index ix
JOIN pg_class i ON i.oid = ix.indexrelid
JOIN pg_class t ON t.oid = ix.indrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
JOIN pg_am am ON am.oid = i.relam
JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = ANY(ix.indkey)
WHERE n.nspname = $1 AND t.relname = $2
GROUP BY i.relname, ix.indisunique, ix.indisprimary, am.amname
ORDER BY i.relname`

	rows, err := db.QueryContext(ctx, q, schema, table)
	if err != nil {
		return []indexInfo{}
	}
	defer func() { _ = rows.Close() }()
	out := []indexInfo{}
	for rows.Next() {
		var idx indexInfo
		var cols []byte
		if err := rows.Scan(&idx.Name, &cols, &idx.Unique, &idx.Primary, &idx.Method); err != nil {
			continue
		}
		idx.Columns = parsePgArray(string(cols))
		out = append(out, idx)
	}
	return out
}

// parsePgArray decodes the {a,b,c} text representation pq returns for
// array_agg. Handles double-quoted segments so identifiers containing
// commas or whitespace round-trip cleanly.
func parsePgArray(s string) []string {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		return []string{}
	}
	out := []string{}
	var cur strings.Builder
	inQuotes := false
	for i := 0; i < len(inner); i++ {
		ch := inner[i]
		switch {
		case ch == '"' && !inQuotes:
			inQuotes = true
		case ch == '"' && inQuotes:
			// Handle "" → literal "
			if i+1 < len(inner) && inner[i+1] == '"' {
				cur.WriteByte('"')
				i++
				continue
			}
			inQuotes = false
		case ch == ',' && !inQuotes:
			out = append(out, cur.String())
			cur.Reset()
		case ch == '\\' && inQuotes && i+1 < len(inner):
			i++
			cur.WriteByte(inner[i])
		default:
			cur.WriteByte(ch)
		}
	}
	out = append(out, cur.String())
	return out
}

func readMySQLIndexes(ctx context.Context, db *sql.DB, schema, table string) []indexInfo {
	const q = `
SELECT INDEX_NAME, COLUMN_NAME, NON_UNIQUE, INDEX_TYPE, SEQ_IN_INDEX
FROM information_schema.statistics
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
ORDER BY INDEX_NAME, SEQ_IN_INDEX`

	rows, err := db.QueryContext(ctx, q, schema, table)
	if err != nil {
		return []indexInfo{}
	}
	defer func() { _ = rows.Close() }()
	indexMap := map[string]*indexInfo{}
	order := []string{}
	for rows.Next() {
		var name, col, method string
		var nonUnique int
		var seq int
		if err := rows.Scan(&name, &col, &nonUnique, &method, &seq); err != nil {
			continue
		}
		idx, ok := indexMap[name]
		if !ok {
			idx = &indexInfo{
				Name:    name,
				Unique:  nonUnique == 0,
				Primary: name == "PRIMARY",
				Method:  method,
			}
			indexMap[name] = idx
			order = append(order, name)
		}
		idx.Columns = append(idx.Columns, col)
	}
	out := make([]indexInfo, 0, len(order))
	for _, name := range order {
		out = append(out, *indexMap[name])
	}
	return out
}

// GetTableStructure exposes the structure read by readTableStructure as
// a dedicated endpoint for the visual designer.
// GET /api/v1/services/{name}/db/tables/{schema}/{table}/structure
func (d *Database) GetTableStructure(w http.ResponseWriter, r *http.Request) {
	svcName, namespace, ok := requireService(w, r)
	if !ok {
		return
	}
	schemaName := chi.URLParam(r, "schema")
	table := chi.URLParam(r, "table")

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	db, info, err := d.connect(ctx, svcName, namespace, r.URL.Query().Get("database"))
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	structure, err := readTableStructure(ctx, db, info.driver, schemaName, table)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("read structure: %v", err))
		return
	}
	if len(structure.Columns) == 0 {
		respondError(w, http.StatusNotFound, fmt.Sprintf("table %s.%s not found", schemaName, table))
		return
	}
	respondJSON(w, http.StatusOK, structure)
}

// scanSingleRow pulls a single returned row into the same []interface{}
// shape we use for list responses.
func scanSingleRow(row *sql.Row, structure tableStructure) ([]interface{}, error) {
	scanTargets := make([]interface{}, len(structure.Columns))
	scanPtrs := make([]interface{}, len(structure.Columns))
	for i := range scanTargets {
		scanPtrs[i] = &scanTargets[i]
	}
	if err := row.Scan(scanPtrs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("inserted row not visible in result set")
		}
		return nil, err
	}
	return normaliseRow(scanTargets), nil
}

func qualified(driver, schema, table string) string {
	return quoteIdent(driver, schema) + "." + quoteIdent(driver, table)
}

func quoteIdent(driver, ident string) string {
	switch driver {
	case "mysql":
		return "`" + strings.ReplaceAll(ident, "`", "``") + "`"
	default:
		return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
	}
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func sortedKeys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func placeholders(driver string, n, startAt int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		if driver == "mysql" {
			out[i] = "?"
		} else {
			out[i] = fmt.Sprintf("$%d", startAt+i)
		}
	}
	return out
}

func buildSelectRows(driver, schema, table, orderBy, dir string, limit, offset int, filterCol string) string {
	var b strings.Builder
	b.WriteString("SELECT * FROM ")
	b.WriteString(qualified(driver, schema, table))
	if filterCol != "" {
		ph := "?"
		if driver != "mysql" {
			ph = "$1"
		}
		fmt.Fprintf(&b, " WHERE %s = %s", quoteIdent(driver, filterCol), ph)
	}
	if orderBy != "" {
		fmt.Fprintf(&b, " ORDER BY %s %s", quoteIdent(driver, orderBy), dir)
	}
	fmt.Fprintf(&b, " LIMIT %d OFFSET %d", limit, offset)
	return b.String()
}

func buildCountRows(driver, schema, table, filterCol string) string {
	q := "SELECT COUNT(*) FROM " + qualified(driver, schema, table)
	if filterCol != "" {
		ph := "?"
		if driver != "mysql" {
			ph = "$1"
		}
		q += fmt.Sprintf(" WHERE %s = %s", quoteIdent(driver, filterCol), ph)
	}
	return q
}

func buildInsert(driver, schema, table string, cols []string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = quoteIdent(driver, c)
	}
	phs := placeholders(driver, len(cols), 1)
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		qualified(driver, schema, table),
		strings.Join(quoted, ", "),
		strings.Join(phs, ", "),
	)
}

// buildInsertReturning returns the postgres path that surfaces server-
// generated columns. The columnCount arg is unused but kept for parity
// in case a future driver wants to truncate the * to specific columns.
func buildInsertReturning(driver, schema, table string, cols []string, columnCount int) string {
	_ = columnCount
	return buildInsert(driver, schema, table, cols) + " RETURNING *"
}

func buildUpdate(driver, schema, table string, changeCols, pkCols []string, changes, pk map[string]interface{}) (string, []interface{}) {
	args := make([]interface{}, 0, len(changeCols)+len(pkCols))
	setParts := make([]string, len(changeCols))
	for i, c := range changeCols {
		ph := "?"
		if driver != "mysql" {
			ph = fmt.Sprintf("$%d", i+1)
		}
		setParts[i] = quoteIdent(driver, c) + " = " + ph
		args = append(args, changes[c])
	}
	whereParts := make([]string, len(pkCols))
	for i, c := range pkCols {
		ph := "?"
		if driver != "mysql" {
			ph = fmt.Sprintf("$%d", len(changeCols)+i+1)
		}
		whereParts[i] = quoteIdent(driver, c) + " = " + ph
		args = append(args, pk[c])
	}
	q := fmt.Sprintf("UPDATE %s SET %s WHERE %s",
		qualified(driver, schema, table),
		strings.Join(setParts, ", "),
		strings.Join(whereParts, " AND "),
	)
	return q, args
}

func buildDelete(driver, schema, table string, pkCols []string, pk map[string]interface{}) (string, []interface{}) {
	args := make([]interface{}, 0, len(pkCols))
	whereParts := make([]string, len(pkCols))
	for i, c := range pkCols {
		ph := "?"
		if driver != "mysql" {
			ph = fmt.Sprintf("$%d", i+1)
		}
		whereParts[i] = quoteIdent(driver, c) + " = " + ph
		args = append(args, pk[c])
	}
	return fmt.Sprintf("DELETE FROM %s WHERE %s",
		qualified(driver, schema, table),
		strings.Join(whereParts, " AND "),
	), args
}

func affectedOr(res sql.Result, fallback int64) int64 {
	if n, err := res.RowsAffected(); err == nil {
		return n
	}
	return fallback
}
