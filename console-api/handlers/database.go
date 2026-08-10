// Package handlers — database.go provides the browser-native SQL surface
// for Kipper-managed services. The console-api opens a short-lived
// connection per request using the credentials Secret already on the
// cluster; the browser never sees credentials.
//
// G1 ships read-only schema browsing, ad-hoc query execution, and a
// safety-railed result envelope. Per-driver code is isolated behind the
// dbDriver interface so adding MongoDB / Redis later doesn't ripple
// into the HTTP layer.
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// validDBName bounds a caller-supplied database name to a plain identifier
// so it can't inject DSN connection parameters.
var validDBName = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

const (
	defaultQueryTimeout = 30 * time.Second
	defaultRowLimit     = 1000
	maxRowLimit         = 10000
	poolIdleTTL         = 5 * time.Minute
)

// Database provides the HTTP face of the database console. The struct
// holds an in-memory pool of *sql.DB keyed by service-namespace+name so
// repeated queries don't pay reconnection cost.
type Database struct {
	Client   kubernetes.Interface
	CRClient crclient.Client

	mu    sync.Mutex
	conns map[string]*pooledConn
}

type pooledConn struct {
	db        *sql.DB
	driver    string
	lastUsed  time.Time
	closeOnce sync.Once
}

// dbDriverInfo carries everything needed to talk to a specific service.
type dbDriverInfo struct {
	driver string // "postgres" or "mysql"
	dsn    string
}

// queryRequest is what the browser POSTs to /db/query.
type queryRequest struct {
	SQL         string `json:"sql"`
	Database    string `json:"database,omitempty"` // optional, defaults to the credentials' NAME
	Limit       int    `json:"limit,omitempty"`
	NoLimit     bool   `json:"no_limit,omitempty"`
	Transaction bool   `json:"transaction,omitempty"`
}

// queryResponse is the result envelope. Either Rows is populated (for
// SELECT-shaped queries) or RowsAffected is populated (for DML), with
// DurationMs and the executed SQL echoed for the history pane.
type queryResponse struct {
	Columns      []columnMeta    `json:"columns,omitempty"`
	Rows         [][]interface{} `json:"rows,omitempty"`
	RowsAffected int64           `json:"rows_affected"`
	DurationMs   int64           `json:"duration_ms"`
	Truncated    bool            `json:"truncated"`
	SQL          string          `json:"sql"`
}

type columnMeta struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// schemaResponse mirrors what the schema sidebar renders: a tree of
// databases → schemas → relations. We deliberately do not include
// columns at the top level — those are fetched on table click via a
// separate call so the sidebar load stays fast on big schemas.
type schemaResponse struct {
	Databases []schemaDatabase `json:"databases"`
}

type schemaDatabase struct {
	Name    string         `json:"name"`
	Schemas []schemaSchema `json:"schemas"`
}

type schemaSchema struct {
	Name      string           `json:"name"`
	Relations []schemaRelation `json:"relations"`
}

type schemaRelation struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"` // "table" | "view"
	Columns int    `json:"columns"`
	Rows    *int64 `json:"rows,omitempty"` // approximate; nil when unknown
}

// databaseEntry describes one selectable database for the picker.
// Default is true for the database the service's credentials Secret
// points at (the NAME field). The list endpoint always connects there
// when probing, so "Default" is unambiguous regardless of which
// database the user has currently picked in the UI.
type databaseEntry struct {
	Name    string `json:"name"`
	Default bool   `json:"default"`
}

// Query runs ad-hoc SQL against a Kipper service.
// POST /api/v1/services/{name}/db/query?namespace={ns}
func (d *Database) Query(w http.ResponseWriter, r *http.Request) {
	svcName, namespace, ok := requireService(w, r)
	if !ok {
		return
	}

	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.SQL) == "" {
		respondError(w, http.StatusBadRequest, "sql is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), defaultQueryTimeout+5*time.Second)
	defer cancel()

	db, info, err := d.connect(ctx, svcName, namespace, req.Database)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	limit := req.Limit
	if limit <= 0 {
		limit = defaultRowLimit
	}
	if limit > maxRowLimit {
		limit = maxRowLimit
	}
	effectiveSQL := req.SQL
	if !req.NoLimit && shouldAutoLimit(req.SQL) {
		effectiveSQL = applyLimit(req.SQL, limit, info.driver)
	}

	queryCtx, queryCancel := context.WithTimeout(ctx, defaultQueryTimeout)
	defer queryCancel()

	start := time.Now()
	resp, err := executeQuery(queryCtx, db, effectiveSQL, req.Transaction, limit)
	resp.DurationMs = time.Since(start).Milliseconds()
	resp.SQL = effectiveSQL

	// Record into per-user history + emit audit log line. Capture the
	// user's identity now while the request context is still live;
	// recordHistory then runs against a fresh background context with
	// its own timeout so it survives the response being flushed.
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	user := userIdentifier(r)
	historyCtx, historyCancel := context.WithTimeout(context.Background(), 5*time.Second)
	d.recordHistory(historyCtx, svcName, namespace, user, historyEntry{
		SQL:        effectiveSQL,
		DurationMs: resp.DurationMs,
		Error:      errMsg,
	})
	historyCancel()
	auditLog(r, svcName, effectiveSQL, resp.DurationMs, errMsg)

	if err != nil {
		// Always include the executed SQL + duration so the user can iterate.
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":       err.Error(),
			"sql":         effectiveSQL,
			"duration_ms": resp.DurationMs,
		})
		return
	}

	respondJSON(w, http.StatusOK, resp)
}

// ListDatabases enumerates the databases visible to the service user
// so the UI can offer a picker. The connection is opened against the
// service's default database (NAME on the credentials Secret) — never
// against whatever the caller currently has selected — so the Default
// flag in the response is stable no matter which database the user is
// browsing.
// GET /api/v1/services/{name}/db/databases?namespace={ns}
func (d *Database) ListDatabases(w http.ResponseWriter, r *http.Request) {
	svcName, namespace, ok := requireService(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	db, info, err := d.connect(ctx, svcName, namespace, "")
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	var (
		entries []databaseEntry
		listErr error
	)
	switch info.driver {
	case "pgx":
		entries, listErr = listPostgresDatabases(ctx, db)
	case "mysql":
		entries, listErr = listMySQLDatabases(ctx, db)
	default:
		listErr = fmt.Errorf("unsupported driver %q", info.driver)
	}
	if listErr != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("list databases: %v", listErr))
		return
	}
	respondJSON(w, http.StatusOK, entries)
}

// Schema returns the database/schema/relation tree for the schema sidebar.
// GET /api/v1/services/{name}/db/schema?namespace={ns}
func (d *Database) Schema(w http.ResponseWriter, r *http.Request) {
	svcName, namespace, ok := requireService(w, r)
	if !ok {
		return
	}
	databaseQ := r.URL.Query().Get("database")

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	db, info, err := d.connect(ctx, svcName, namespace, databaseQ)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	var resp schemaResponse
	switch info.driver {
	case "pgx":
		resp, err = readPostgresSchema(ctx, db)
	case "mysql":
		resp, err = readMySQLSchema(ctx, db)
	default:
		err = fmt.Errorf("unsupported driver %q", info.driver)
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("schema read failed: %v", err))
		return
	}

	respondJSON(w, http.StatusOK, resp)
}

// connect resolves the credentials Secret for a service and returns a
// pooled *sql.DB. The pool is keyed by driver+DSN, so two services with
// the same name in different namespaces get separate handles (their
// DSNs differ in host or credentials).
func (d *Database) connect(ctx context.Context, svcName, namespace, database string) (*sql.DB, dbDriverInfo, error) {
	info, err := d.driverInfo(ctx, svcName, namespace, database)
	if err != nil {
		return nil, info, err
	}

	d.mu.Lock()
	if d.conns == nil {
		d.conns = make(map[string]*pooledConn)
	}
	key := info.driver + "|" + info.dsn
	pc := d.conns[key]
	if pc != nil && time.Since(pc.lastUsed) > poolIdleTTL {
		pc.closeOnce.Do(func() { _ = pc.db.Close() })
		delete(d.conns, key)
		pc = nil
	}
	if pc == nil {
		db, openErr := sql.Open(info.driver, info.dsn)
		if openErr != nil {
			d.mu.Unlock()
			return nil, info, fmt.Errorf("open: %w", openErr)
		}
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(2)
		db.SetConnMaxIdleTime(poolIdleTTL)
		pc = &pooledConn{db: db, driver: info.driver, lastUsed: time.Now()}
		d.conns[key] = pc
	}
	pc.lastUsed = time.Now()
	d.mu.Unlock()

	if pingErr := pc.db.PingContext(ctx); pingErr != nil {
		// Drop the dead handle so the next request reopens.
		d.mu.Lock()
		pc.closeOnce.Do(func() { _ = pc.db.Close() })
		delete(d.conns, key)
		d.mu.Unlock()
		return nil, info, fmt.Errorf("ping: %w", pingErr)
	}
	return pc.db, info, nil
}

// driverInfo reads the credentials Secret and builds a DSN. Only
// postgres and mysql are wired in G1; other types return an error so
// the UI can show a "not yet supported" state.
func (d *Database) driverInfo(ctx context.Context, svcName, namespace, overrideDB string) (dbDriverInfo, error) {
	// The override reaches the DSN as the database name. Without this the
	// value flows into a pgx keyword/value DSN, where a space injects
	// connection params (host, sslmode), or into the mysql DSN path. Only
	// plain identifiers select a database.
	if overrideDB != "" && !validDBName.MatchString(overrideDB) {
		return dbDriverInfo{}, fmt.Errorf("invalid database name %q", overrideDB)
	}

	svc, err := d.findServiceCR(ctx, svcName, namespace)
	if err != nil {
		return dbDriverInfo{}, fmt.Errorf("service %q not found", svcName)
	}

	secret, err := d.Client.CoreV1().Secrets(namespace).Get(ctx, secretname.ServiceCredentials(svcName), metav1.GetOptions{})
	if err != nil {
		return dbDriverInfo{}, fmt.Errorf("credentials secret missing: %w", err)
	}
	host := string(secret.Data["HOST"])
	port := string(secret.Data["PORT"])
	user := string(secret.Data["USERNAME"])
	pw := string(secret.Data["PASSWORD"])
	name := string(secret.Data["NAME"])
	if overrideDB != "" {
		name = overrideDB
	}
	if host == "" || port == "" {
		return dbDriverInfo{}, errors.New("credentials secret is missing HOST or PORT")
	}

	switch svc.Spec.Type {
	case "postgres":
		dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
			host, port, user, pw, defaultDB(name, "postgres"))
		return dbDriverInfo{driver: "pgx", dsn: dsn}, nil
	case "mysql":
		dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&multiStatements=false",
			user, pw, host, port, defaultDB(name, ""))
		return dbDriverInfo{driver: "mysql", dsn: dsn}, nil
	default:
		return dbDriverInfo{}, fmt.Errorf("service type %q not yet supported by the database console", svc.Spec.Type)
	}
}

func defaultDB(name, fallback string) string {
	if name != "" {
		return name
	}
	return fallback
}

// findServiceCR looks up a Service CR scoped to a specific namespace.
// Two services can share a name across projects (postgres "db" in one
// project and mysql "db" in another), so the caller must always supply
// the namespace — the database handlers route through Service CR ->
// credentials Secret -> driver, and any cross-namespace fuzziness
// would silently route queries to the wrong database.
func (d *Database) findServiceCR(ctx context.Context, name, namespace string) (*kipperv1.Service, error) {
	var svc kipperv1.Service
	if err := d.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, &svc); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, k8serrors.NewNotFound(corev1.Resource("services"), name)
		}
		return nil, err
	}
	return &svc, nil
}

// requireService reads the {name} URL param and the ?namespace= query
// parameter that database endpoints need to disambiguate services.
// Returns (svcName, namespace, true) on success; emits a 400 and
// returns ok=false when either is missing.
//
// Two services can share a name across projects (e.g. postgres "db"
// in one project and mysql "db" in another). Without namespace the
// handler would silently route the query to the first match — a real
// bug we hit in practice. Namespace is therefore mandatory.
func requireService(w http.ResponseWriter, r *http.Request) (svcName, namespace string, ok bool) {
	svcName = chi.URLParam(r, "name")
	if svcName == "" {
		respondError(w, http.StatusBadRequest, "service name required")
		return "", "", false
	}
	namespace = r.URL.Query().Get("namespace")
	if namespace == "" {
		respondError(w, http.StatusBadRequest, "namespace query parameter is required")
		return "", "", false
	}
	return svcName, namespace, true
}

// executeQuery runs the SQL and produces a row-major envelope. SELECT-
// shaped queries return Columns + Rows; DML returns RowsAffected only.
func executeQuery(ctx context.Context, db *sql.DB, query string, asTransaction bool, limit int) (queryResponse, error) {
	resp := queryResponse{}

	if asTransaction {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return resp, err
		}
		// Best-effort rollback on any error path; commit on the happy path.
		var commitErr error
		defer func() {
			if commitErr != nil {
				_ = tx.Rollback()
			}
		}()
		commitErr = runOnConn(ctx, tx, query, limit, &resp)
		if commitErr != nil {
			return resp, commitErr
		}
		return resp, tx.Commit()
	}
	err := runOnConn(ctx, db, query, limit, &resp)
	return resp, err
}

// txOrDB lets executeQuery use either *sql.DB or *sql.Tx.
type txOrDB interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

func runOnConn(ctx context.Context, conn txOrDB, query string, limit int, resp *queryResponse) error {
	// Try Query first; if the driver tells us this is non-SELECT, fall back to Exec.
	rows, err := conn.QueryContext(ctx, query)
	if err == nil {
		defer func() { _ = rows.Close() }()
		cols, err := rows.ColumnTypes()
		if err != nil {
			return err
		}
		resp.Columns = make([]columnMeta, len(cols))
		for i, c := range cols {
			resp.Columns[i] = columnMeta{Name: c.Name(), Type: c.DatabaseTypeName()}
		}

		count := 0
		for rows.Next() {
			if count >= limit {
				resp.Truncated = true
				break
			}
			scanTargets := make([]interface{}, len(cols))
			scanPtrs := make([]interface{}, len(cols))
			for i := range scanTargets {
				scanPtrs[i] = &scanTargets[i]
			}
			if err := rows.Scan(scanPtrs...); err != nil {
				return err
			}
			resp.Rows = append(resp.Rows, normaliseRow(scanTargets))
			count++
		}
		return rows.Err()
	}
	// Some drivers report DML/DDL as an error here; fall through to Exec.
	res, execErr := conn.ExecContext(ctx, query)
	if execErr != nil {
		return execErr
	}
	if affected, aerr := res.RowsAffected(); aerr == nil {
		resp.RowsAffected = affected
	}
	return nil
}

// normaliseRow turns []byte (the default for many drivers) into strings
// so JSON encoding produces readable output. nil stays nil.
func normaliseRow(row []interface{}) []interface{} {
	out := make([]interface{}, len(row))
	for i, v := range row {
		switch t := v.(type) {
		case []byte:
			out[i] = string(t)
		default:
			out[i] = v
		}
	}
	return out
}

// shouldAutoLimit returns true for queries that are clearly bare
// SELECTs (no LIMIT, no aggregate already). Heuristic — false negatives
// are fine, false positives are not.
func shouldAutoLimit(query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if !strings.HasPrefix(q, "select") {
		return false
	}
	if strings.Contains(q, " limit ") || strings.HasSuffix(q, " limit") {
		return false
	}
	return true
}

// applyLimit appends a LIMIT clause that is dialect-correct.
func applyLimit(query string, limit int, driver string) string {
	q := strings.TrimRight(strings.TrimSpace(query), ";")
	switch driver {
	case "mysql":
		return fmt.Sprintf("%s LIMIT %d", q, limit)
	default:
		return fmt.Sprintf("%s LIMIT %d", q, limit)
	}
}

// schemaRow is the flat row shape we read from information_schema in
// either driver before grouping into the response tree.
type schemaRow struct {
	catalog, schema, name, kind string
}

// readPostgresSchema queries information_schema for the visible
// databases, schemas, and relations. Schemas are read independently
// from tables so an empty schema still appears in the sidebar — the
// user-visible "schema disappeared" symptom after dropping every
// table was just an artifact of joining off information_schema.tables.
func readPostgresSchema(ctx context.Context, db *sql.DB) (schemaResponse, error) {
	const schemasQ = `
SELECT current_database(), schema_name
FROM information_schema.schemata
WHERE schema_name NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
ORDER BY schema_name`

	rels, err := readSchemasOnly(ctx, db, schemasQ)
	if err != nil {
		return schemaResponse{}, err
	}

	const tablesQ = `
SELECT table_catalog, table_schema, table_name, table_type
FROM information_schema.tables
WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
ORDER BY table_catalog, table_schema, table_name`

	rows, err := db.QueryContext(ctx, tablesQ)
	if err != nil {
		return schemaResponse{}, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var r schemaRow
		if err := rows.Scan(&r.catalog, &r.schema, &r.name, &r.kind); err != nil {
			return schemaResponse{}, err
		}
		rels = append(rels, r)
	}
	if err := rows.Err(); err != nil {
		return schemaResponse{}, err
	}
	return groupSchema(rels, normalisePostgresKind), nil
}

// readSchemasOnly returns one schemaRow per visible schema with
// catalog and schema populated and name/kind left empty. groupSchema
// treats these as "schema exists, no relations" markers so empty
// schemas still render in the sidebar.
func readSchemasOnly(ctx context.Context, db *sql.DB, query string) ([]schemaRow, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []schemaRow
	for rows.Next() {
		var r schemaRow
		if err := rows.Scan(&r.catalog, &r.schema); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func normalisePostgresKind(kind string) string {
	switch strings.ToUpper(kind) {
	case "BASE TABLE":
		return "table"
	case "VIEW":
		return "view"
	default:
		return strings.ToLower(kind)
	}
}

// readMySQLSchema mirrors the postgres reader using MySQL's information_schema.
func readMySQLSchema(ctx context.Context, db *sql.DB) (schemaResponse, error) {
	const schemasQ = `
SELECT schema_name, schema_name
FROM information_schema.schemata
WHERE schema_name NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')
ORDER BY schema_name`

	rels, err := readSchemasOnly(ctx, db, schemasQ)
	if err != nil {
		return schemaResponse{}, err
	}

	const tablesQ = `
SELECT table_schema, table_schema, table_name, table_type
FROM information_schema.tables
WHERE table_schema NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')
ORDER BY table_schema, table_name`

	rows, err := db.QueryContext(ctx, tablesQ)
	if err != nil {
		return schemaResponse{}, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var r schemaRow
		if err := rows.Scan(&r.catalog, &r.schema, &r.name, &r.kind); err != nil {
			return schemaResponse{}, err
		}
		rels = append(rels, r)
	}
	if err := rows.Err(); err != nil {
		return schemaResponse{}, err
	}
	return groupSchema(rels, normaliseMySQLKind), nil
}

func normaliseMySQLKind(kind string) string {
	switch strings.ToUpper(kind) {
	case "BASE TABLE":
		return "table"
	case "VIEW":
		return "view"
	default:
		return strings.ToLower(kind)
	}
}

// listPostgresDatabases reads every non-template, connectable database
// from pg_database. has_database_privilege keeps databases the role
// cannot actually log into out of the picker. current_database()
// resolves to the service default because the caller of this helper
// always connects without an overrideDB.
func listPostgresDatabases(ctx context.Context, db *sql.DB) ([]databaseEntry, error) {
	const q = `
SELECT d.datname, d.datname = current_database()
FROM pg_database d
WHERE d.datistemplate = false
  AND d.datallowconn = true
  AND has_database_privilege(current_user, d.datname, 'CONNECT')
ORDER BY d.datname`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []databaseEntry{}
	for rows.Next() {
		var e databaseEntry
		if err := rows.Scan(&e.Name, &e.Default); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// listMySQLDatabases reads information_schema.schemata, which in MySQL
// is the cluster-wide list of databases. We filter the four canonical
// system schemas; anything the user created shows up.
func listMySQLDatabases(ctx context.Context, db *sql.DB) ([]databaseEntry, error) {
	const q = `
SELECT schema_name, schema_name = DATABASE()
FROM information_schema.schemata
WHERE schema_name NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')
ORDER BY schema_name`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []databaseEntry{}
	for rows.Next() {
		var e databaseEntry
		if err := rows.Scan(&e.Name, &e.Default); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// groupSchema converts a flat list of (catalog, schema, name, kind)
// rows into the nested tree the UI expects. Always returns non-nil
// slices so the JSON response uses `[]` rather than `null` and the
// browser doesn't have to defend against either.
func groupSchema(rels []schemaRow, kindFn func(string) string) schemaResponse {
	resp := schemaResponse{Databases: []schemaDatabase{}}
	dbIdx := map[string]int{}
	schemaIdx := map[string]int{}
	for _, r := range rels {
		dbKey := r.catalog
		di, ok := dbIdx[dbKey]
		if !ok {
			resp.Databases = append(resp.Databases, schemaDatabase{Name: r.catalog, Schemas: []schemaSchema{}})
			di = len(resp.Databases) - 1
			dbIdx[dbKey] = di
		}
		schemaKey := dbKey + "/" + r.schema
		si, ok := schemaIdx[schemaKey]
		if !ok {
			resp.Databases[di].Schemas = append(resp.Databases[di].Schemas, schemaSchema{Name: r.schema, Relations: []schemaRelation{}})
			si = len(resp.Databases[di].Schemas) - 1
			schemaIdx[schemaKey] = si
		}
		// Schema-only marker — schema exists but the row carries no
		// relation. Keeps empty schemas visible in the sidebar.
		if r.name == "" {
			continue
		}
		resp.Databases[di].Schemas[si].Relations = append(resp.Databases[di].Schemas[si].Relations, schemaRelation{
			Name: r.name,
			Kind: kindFn(r.kind),
		})
	}
	return resp
}
