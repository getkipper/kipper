import client from './client'

// All endpoints below require both `service` (the Service CR name) and
// `namespace` (the project the service lives in). Two services may share
// a name in different projects; the server uses the namespace to pick
// the right one and rejects requests that don't include it.

function withNS(namespace: string, extra?: URLSearchParams): string {
  const p = extra ?? new URLSearchParams()
  p.set('namespace', namespace)
  return `?${p.toString()}`
}

// dbParams folds the database picker's selection into a URLSearchParams
// alongside any caller-provided params. Pass the result to withNS so
// the namespace is appended last and the URL stays consistent.
function dbParams(database?: string, extra?: URLSearchParams): URLSearchParams {
  const p = extra ?? new URLSearchParams()
  if (database) p.set('database', database)
  return p
}

export interface DBDatabaseEntry {
  name: string
  // True for the service's default database (the NAME field on the
  // service credentials Secret). The picker labels this entry so the
  // user knows which database an app gets by default when bound.
  default: boolean
}

export async function fetchDBDatabases(service: string, namespace: string): Promise<DBDatabaseEntry[]> {
  // No database parameter: the server always probes via the service's
  // default credentials so the Default flag is unambiguous.
  const { data } = await client.get<DBDatabaseEntry[]>(`/services/${service}/db/databases${withNS(namespace)}`)
  return data || []
}

export interface DBColumn {
  name: string
  type: string
}

export interface DBQueryResult {
  columns?: DBColumn[]
  rows?: unknown[][]
  rows_affected: number
  duration_ms: number
  truncated: boolean
  sql: string
  error?: string
}

export interface DBQueryRequest {
  sql: string
  database?: string
  limit?: number
  no_limit?: boolean
  transaction?: boolean
}

export interface DBSchemaRelation {
  name: string
  kind: 'table' | 'view' | string
  columns: number
  rows?: number
}

export interface DBSchemaSchema {
  name: string
  relations: DBSchemaRelation[]
}

export interface DBSchemaDatabase {
  name: string
  schemas: DBSchemaSchema[]
}

export interface DBSchema {
  databases: DBSchemaDatabase[]
}

export async function fetchDBSchema(service: string, namespace: string, database?: string): Promise<DBSchema> {
  const { data } = await client.get<DBSchema>(`/services/${service}/db/schema${withNS(namespace, dbParams(database))}`)
  return data || { databases: [] }
}

export interface DBColumnInfo {
  name: string
  type: string
  nullable: boolean
  default?: string
  generated: boolean
  position: number
}

export interface DBForeignKey {
  column: string
  ref_schema: string
  ref_table: string
  ref_column: string
}

export interface DBIndexInfo {
  name: string
  columns: string[]
  unique: boolean
  primary: boolean
  method?: string
}

export interface DBTableStructure {
  schema: string
  name: string
  columns: DBColumnInfo[]
  primary_key: string[]
  foreign_keys: DBForeignKey[]
  indexes: DBIndexInfo[]
}

export interface DBRowsResponse {
  structure: DBTableStructure
  rows: unknown[][]
  total: number
  limit: number
  offset: number
  duration_ms: number
}

export interface DBRowsListOptions {
  limit?: number
  offset?: number
  order_by?: string
  order_dir?: 'ASC' | 'DESC'
  filter_col?: string
  filter_val?: string
}

export async function fetchDBRows(
  service: string,
  namespace: string,
  schema: string,
  table: string,
  opts: DBRowsListOptions = {},
  database?: string,
): Promise<DBRowsResponse> {
  const params = dbParams(database)
  if (opts.limit !== undefined) params.set('limit', String(opts.limit))
  if (opts.offset !== undefined) params.set('offset', String(opts.offset))
  if (opts.order_by) params.set('order_by', opts.order_by)
  if (opts.order_dir) params.set('order_dir', opts.order_dir)
  if (opts.filter_col) params.set('filter_col', opts.filter_col)
  if (opts.filter_val !== undefined) params.set('filter_val', opts.filter_val)
  const { data } = await client.get<DBRowsResponse>(
    `/services/${service}/db/tables/${encodeURIComponent(schema)}/${encodeURIComponent(table)}/rows${withNS(namespace, params)}`,
  )
  return data
}

export async function insertDBRow(
  service: string,
  namespace: string,
  schema: string,
  table: string,
  values: Record<string, unknown>,
  database?: string,
): Promise<{ row?: unknown[] }> {
  const { data } = await client.post<{ row?: unknown[] }>(
    `/services/${service}/db/tables/${encodeURIComponent(schema)}/${encodeURIComponent(table)}/rows${withNS(namespace, dbParams(database))}`,
    values,
  )
  return data
}

export async function updateDBRow(
  service: string,
  namespace: string,
  schema: string,
  table: string,
  pk: Record<string, unknown>,
  changes: Record<string, unknown>,
  database?: string,
): Promise<{ rows_affected: number }> {
  const { data } = await client.patch<{ rows_affected: number }>(
    `/services/${service}/db/tables/${encodeURIComponent(schema)}/${encodeURIComponent(table)}/rows${withNS(namespace, dbParams(database))}`,
    { pk, changes },
  )
  return data
}

// --- Designer (G3) ---

export interface DBColumnSpec {
  name: string
  type: string
  nullable: boolean
  default?: string | null
  generated_expr?: string
  comment?: string
}

export interface DBConstraintSpec {
  name?: string
  type: 'PRIMARY KEY' | 'FOREIGN KEY' | 'UNIQUE' | 'CHECK'
  columns?: string[]
  ref_schema?: string
  ref_table?: string
  ref_columns?: string[]
  on_delete?: string
  on_update?: string
  check_expr?: string
}

export type DBTableOpKind =
  | 'add_column'
  | 'drop_column'
  | 'rename_column'
  | 'alter_column_type'
  | 'set_nullable'
  | 'set_default'
  | 'add_constraint'
  | 'drop_constraint'
  | 'rename_table'

export interface DBTableOp {
  op: DBTableOpKind
  column?: DBColumnSpec
  name?: string
  old_name?: string
  new_name?: string
  constraint?: DBConstraintSpec
  constraint_name?: string
}

export interface DBCreateTablePayload {
  schema: string
  name: string
  columns: DBColumnSpec[]
  constraints?: DBConstraintSpec[]
}

export interface DBIndexPayload {
  schema: string
  table: string
  name: string
  columns: string[]
  unique?: boolean
  method?: string
  where?: string
  concurrent?: boolean
}

export interface DBPreviewRequest {
  create_table?: DBCreateTablePayload
  alter_table?: { schema: string; table: string; ops: DBTableOp[] }
  create_index?: DBIndexPayload
  drop_index?: { schema: string; name: string; concurrent?: boolean }
}

export interface DBPreviewResponse {
  ddl: string[]
}

export async function fetchDBTableStructure(
  service: string,
  namespace: string,
  schema: string,
  table: string,
  database?: string,
): Promise<DBTableStructure> {
  const { data } = await client.get<DBTableStructure>(
    `/services/${service}/db/tables/${encodeURIComponent(schema)}/${encodeURIComponent(table)}/structure${withNS(namespace, dbParams(database))}`,
  )
  return data
}

export async function createDBTable(service: string, namespace: string, payload: DBCreateTablePayload, database?: string): Promise<DBPreviewResponse> {
  const { data } = await client.post<DBPreviewResponse>(`/services/${service}/db/tables${withNS(namespace, dbParams(database))}`, payload)
  return data
}

export async function alterDBTable(
  service: string,
  namespace: string,
  schema: string,
  table: string,
  ops: DBTableOp[],
  database?: string,
): Promise<DBPreviewResponse> {
  const { data } = await client.patch<DBPreviewResponse>(
    `/services/${service}/db/tables/${encodeURIComponent(schema)}/${encodeURIComponent(table)}${withNS(namespace, dbParams(database))}`,
    { ops },
  )
  return data
}

export async function createDBIndex(service: string, namespace: string, payload: DBIndexPayload, database?: string): Promise<DBPreviewResponse> {
  const { data } = await client.post<DBPreviewResponse>(`/services/${service}/db/indexes${withNS(namespace, dbParams(database))}`, payload)
  return data
}

export async function dropDBIndex(
  service: string,
  namespace: string,
  schema: string,
  name: string,
  concurrent = false,
  database?: string,
): Promise<DBPreviewResponse> {
  const params = dbParams(database)
  if (concurrent) params.set('concurrent', 'true')
  const { data } = await client.delete<DBPreviewResponse>(
    `/services/${service}/db/indexes/${encodeURIComponent(schema)}/${encodeURIComponent(name)}${withNS(namespace, params)}`,
  )
  return data
}

export async function previewDBDDL(service: string, namespace: string, req: DBPreviewRequest, database?: string): Promise<DBPreviewResponse> {
  const { data } = await client.post<DBPreviewResponse>(`/services/${service}/db/ddl/preview${withNS(namespace, dbParams(database))}`, req)
  return data
}

// --- Snippets + history (G5) ---

export interface DBSnippet {
  name: string
  sql: string
  pinned?: boolean
  updated_at?: string
  updated_by?: string
}

export interface DBHistoryEntry {
  sql: string
  duration_ms: number
  error?: string
  timestamp: string
  user?: string
}

export async function fetchDBSnippets(service: string, namespace: string): Promise<DBSnippet[]> {
  const { data } = await client.get<DBSnippet[]>(`/services/${service}/db/snippets${withNS(namespace)}`)
  return data || []
}

export async function saveDBSnippet(service: string, namespace: string, snippet: DBSnippet): Promise<DBSnippet> {
  const { data } = await client.post<DBSnippet>(`/services/${service}/db/snippets${withNS(namespace)}`, snippet)
  return data
}

export async function deleteDBSnippet(service: string, namespace: string, name: string): Promise<void> {
  await client.delete(`/services/${service}/db/snippets/${encodeURIComponent(name)}${withNS(namespace)}`)
}

export async function fetchDBHistory(service: string, namespace: string): Promise<DBHistoryEntry[]> {
  const { data } = await client.get<DBHistoryEntry[]>(`/services/${service}/db/history${withNS(namespace)}`)
  return data || []
}

export async function deleteDBRows(
  service: string,
  namespace: string,
  schema: string,
  table: string,
  pks: Array<Record<string, unknown>>,
  database?: string,
): Promise<{ rows_affected: number }> {
  const { data } = await client.delete<{ rows_affected: number }>(
    `/services/${service}/db/tables/${encodeURIComponent(schema)}/${encodeURIComponent(table)}/rows${withNS(namespace, dbParams(database))}`,
    { data: { pks, confirm: true } },
  )
  return data
}

export async function runDBQuery(service: string, namespace: string, req: DBQueryRequest): Promise<DBQueryResult> {
  // The server returns 400 with the same envelope shape on SQL errors so
  // the editor can show duration + executed SQL even on failure. We treat
  // it as a normal response and let the caller branch on `error`.
  try {
    const { data } = await client.post<DBQueryResult>(`/services/${service}/db/query${withNS(namespace)}`, req)
    return data
  } catch (e: unknown) {
    const err = e as { response?: { data?: DBQueryResult & { error?: string } }; message?: string }
    if (err.response?.data) return err.response.data
    return {
      rows_affected: 0,
      duration_ms: 0,
      truncated: false,
      sql: req.sql,
      error: err.message || 'request failed',
    }
  }
}
