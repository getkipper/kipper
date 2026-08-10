<script setup lang="ts">
import { ref, computed, onMounted, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  ArrowLeft, RefreshCw, Database, Table as TableIcon, Eye, Play, Loader2,
  ChevronRight, ChevronDown, Download, AlertTriangle, Wand2, Plus, Trash2,
  ChevronsLeft, ChevronsRight, ArrowUpDown, ArrowUp, ArrowDown, X, Check,
  Sparkles, Star, History as HistoryIcon, BookmarkPlus, Pin,
} from 'lucide-vue-next'
import AIChat from '@/components/AIChat.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import { useModal } from '@/composables/useModal'
import type { ChatDBSchema, ChatDBTable } from '@/api/ai-chat'
import { Codemirror } from 'vue-codemirror'
import { sql, PostgreSQL, MySQL } from '@codemirror/lang-sql'
import { oneDark } from '@codemirror/theme-one-dark'
import { keymap, EditorView } from '@codemirror/view'
import { Prec } from '@codemirror/state'
import { useToast } from '@/composables/useToast'
import {
  fetchDBDatabases,
  fetchDBSchema,
  runDBQuery,
  fetchDBRows,
  insertDBRow,
  updateDBRow,
  deleteDBRows,
  alterDBTable,
  createDBTable,
  createDBIndex,
  dropDBIndex,
  previewDBDDL,
  fetchDBTableStructure,
  fetchDBSnippets,
  saveDBSnippet,
  deleteDBSnippet,
  fetchDBHistory,
  type DBDatabaseEntry,
  type DBSchema,
  type DBQueryResult,
  type DBRowsResponse,
  type DBTableOp,
  type DBColumnSpec,
  type DBIndexPayload,
  type DBSnippet,
  type DBHistoryEntry,
} from '@/api/database'

const route = useRoute()
const router = useRouter()
const toast = useToast()
const modal = useModal()

const serviceName = computed(() => route.params.name as string)
// Namespace travels via the query string (?namespace=…). Without it the
// server can't disambiguate two services that share a name across
// projects (e.g. postgres "db" and mysql "db"). The Services list and
// the side panel both populate it on the link.
const namespace = computed(() => (route.query.namespace as string) || '')

// --- Database picker ---
// A postgres/mysql service can host many databases (the service's
// default plus per-binding ones created on `kip service bind`). The
// picker lets the user switch between them; the choice persists in
// the URL so reloads and shared links land on the same database.
const databases = ref<DBDatabaseEntry[]>([])
const databasesLoading = ref(false)
const activeDatabase = computed<string>(() => (route.query.database as string) || '')

function setActiveDatabase(name: string) {
  // Empty string means "use the service's default". We avoid putting an
  // empty database= in the URL.
  const next = { ...route.query, database: name || undefined }
  router.replace({ query: next })
}

async function loadDatabases() {
  databasesLoading.value = true
  try {
    databases.value = await fetchDBDatabases(serviceName.value, namespace.value)
    if (!activeDatabase.value) {
      const def = databases.value.find((d) => d.default)
      if (def) setActiveDatabase(def.name)
    }
  } catch {
    // Surface as a tooltip on the picker rather than a top-level
    // banner — the schema panel will already show the underlying
    // connection error if the service is unreachable.
    databases.value = []
  } finally {
    databasesLoading.value = false
  }
}

function onDatabaseChange(name: string) {
  // Updating the URL fires the activeDatabase watcher which handles
  // the schema reload, autocomplete cache flush, and selection reset
  // in one place. Avoid an immediate loadSchema() here — route.query
  // has not been re-read by the computed yet, so we'd fetch with the
  // old database value and race the watcher's correct fetch.
  setActiveDatabase(name)
}

// Tabs in the main pane.
const activeTab = ref<'browse' | 'sql' | 'designer' | 'indexes'>('sql')

// Selected relation for the Browse tab.
const selectedRelation = ref<{ schema: string; name: string } | null>(null)

// --- Schema sidebar ---
const schema = ref<DBSchema>({ databases: [] })
const schemaLoading = ref(false)
const schemaError = ref<string | null>(null)
const expandedDbs = ref<Record<string, boolean>>({})
const expandedSchemas = ref<Record<string, boolean>>({})
const schemaCollapsed = ref(false)

// Per-table column cache for SQL autocomplete. The schema endpoint
// returns table names only (cheap); this cache stores the column list
// for each table so cross-table joins benefit from column suggestions
// without forcing the user to open every table first.
//
// Keyed by `<schema>.<table>` to avoid collisions between schemas.
const tableColumnsCache = ref<Record<string, string[]>>({})

async function ensureTableColumns(schemaName: string, tableName: string) {
  const key = `${schemaName}.${tableName}`
  if (tableColumnsCache.value[key]) return
  try {
    const structure = await fetchDBTableStructure(serviceName.value, namespace.value, schemaName, tableName, activeDatabase.value)
    tableColumnsCache.value = {
      ...tableColumnsCache.value,
      [key]: structure.columns.map((c) => c.name),
    }
  } catch {
    // Best-effort — leave the table in autocomplete by name only.
  }
}

// loadAllTableColumns walks the loaded schema and lazy-loads structure
// for every table whose columns we don't yet have cached. Runs in
// parallel with no explicit concurrency cap because the typical
// project's schema has dozens of tables, not thousands, and the
// browser plus console-api connection pool already throttle in
// practice.
async function loadAllTableColumns() {
  const tasks: Promise<void>[] = []
  for (const db of schema.value.databases) {
    for (const sch of db.schemas) {
      for (const rel of sch.relations) {
        if (rel.kind !== 'table') continue
        tasks.push(ensureTableColumns(sch.name, rel.name))
      }
    }
  }
  await Promise.all(tasks)
}

async function loadSchema() {
  schemaLoading.value = true
  schemaError.value = null
  try {
    const raw = await fetchDBSchema(serviceName.value, namespace.value, activeDatabase.value)
    const dbs = (raw?.databases ?? []).map((db) => ({
      ...db,
      schemas: (db.schemas ?? []).map((sch) => ({
        ...sch,
        relations: sch.relations ?? [],
      })),
    }))
    schema.value = { databases: dbs }
    if (dbs.length === 1) {
      expandedDbs.value[dbs[0].name] = true
      if (dbs[0].schemas.length === 1) {
        const key = dbs[0].name + '/' + dbs[0].schemas[0].name
        expandedSchemas.value[key] = true
      }
    }
    // Lazy-load column metadata for autocomplete in the background.
    // Failures are swallowed in ensureTableColumns so the SQL editor
    // gets the table-name autocomplete even if some structure calls
    // fall over.
    loadAllTableColumns()
  } catch (e: unknown) {
    const err = e as { response?: { data?: { error?: string } }; message?: string }
    schemaError.value = err.response?.data?.error || err.message || 'Failed to load schema'
  } finally {
    schemaLoading.value = false
  }
}

function toggleDb(name: string) {
  expandedDbs.value[name] = !expandedDbs.value[name]
}
function toggleSchema(db: string, sch: string) {
  expandedSchemas.value[db + '/' + sch] = !expandedSchemas.value[db + '/' + sch]
}

// Single click on a table = open it in the Browse tab.
function openInBrowse(schemaName: string, relName: string) {
  selectedRelation.value = { schema: schemaName, name: relName }
  activeTab.value = 'browse'
}

// Double click = drop the qualified name into the SQL editor.
function insertRelation(schemaName: string, relName: string) {
  const ref = schemaName === 'public' ? relName : `${schemaName}.${relName}`
  if (sqlText.value.trim() === '') {
    sqlText.value = `SELECT * FROM ${ref}`
  } else {
    sqlText.value += (sqlText.value.endsWith(' ') ? '' : ' ') + ref
  }
  activeTab.value = 'sql'
}

// --- SQL Editor (G1) ---
const sqlText = ref('')
const runAsTransaction = ref(false)
const noLimit = ref(false)
const running = ref(false)
const result = ref<DBQueryResult | null>(null)

// Schema-aware autocomplete dictionary for @codemirror/lang-sql.
// Map of table name → column names. The open table contributes its
// real columns; other tables show up by name only (so "FROM <Tab>"
// suggests them) until the user opens them. Schema-qualified names are
// included as keys so "FROM public.users" autocompletes too.
const sqlAutocompleteSchema = computed<Record<string, string[]>>(() => {
  const out: Record<string, string[]> = {}
  for (const db of schema.value.databases) {
    for (const sch of db.schemas) {
      for (const rel of sch.relations) {
        if (rel.kind !== 'table' && rel.kind !== 'view') continue
        const cached = tableColumnsCache.value[`${sch.name}.${rel.name}`] || []
        out[rel.name] = cached
        if (sch.name && sch.name !== 'public') {
          out[`${sch.name}.${rel.name}`] = cached
        }
      }
    }
  }
  // Currently-open table overrides with the freshest column data from
  // the rows endpoint, in case the cached structure is stale.
  if (browseData.value && selectedRelation.value) {
    const cols = browseData.value.structure.columns.map((c) => c.name)
    out[selectedRelation.value.name] = cols
    if (selectedRelation.value.schema && selectedRelation.value.schema !== 'public') {
      out[`${selectedRelation.value.schema}.${selectedRelation.value.name}`] = cols
    }
  }
  return out
})

// Run-shortcut keymap. Wrapped in Prec.highest so it always wins over
// any default keymap that might match Mod-Enter, and paired with a
// direct DOM keydown handler so the event is intercepted before
// CodeMirror's contentEditable synthesises a newline.
const sqlKeymap = Prec.highest(
  keymap.of([
    {
      key: 'Mod-Enter',
      preventDefault: true,
      run: () => {
        runQuery()
        return true
      },
    },
    {
      key: 'Mod-Shift-Enter',
      preventDefault: true,
      run: () => {
        runQueryAll()
        return true
      },
    },
  ]),
)

// Belt-and-braces: catch Mod-Enter at the DOM level, before CodeMirror's
// own keydown processing. Both layers calling preventDefault is harmless;
// what matters is that one of them fires before the contentEditable
// inserts a newline.
const sqlDomKeyHandler = EditorView.domEventHandlers({
  keydown(event) {
    if (!(event.ctrlKey || event.metaKey)) return false
    if (event.key !== 'Enter') return false
    event.preventDefault()
    event.stopPropagation()
    if (event.shiftKey) runQueryAll()
    else runQuery()
    return true
  },
})

const editorExtensions = computed(() => [
  sql({
    dialect: inferDialect() === 'mysql' ? MySQL : PostgreSQL,
    schema: sqlAutocompleteSchema.value,
    // When the user types unqualified column names, suggest from the
    // currently-open table by default. Falls back to no default if no
    // table is selected.
    defaultTable: selectedRelation.value?.name,
    upperCaseKeywords: true,
  }),
  oneDark,
  sqlDomKeyHandler,
  sqlKeymap,
])

// CodeMirror view handle. Captured via @ready so we can read the
// cursor and selection state for "run statement at cursor".
const editorView = ref<EditorView | null>(null)

function onEditorReady(payload: { view: EditorView }) {
  editorView.value = payload.view
}

// statementToRun returns the SQL the user wants to execute on the
// next Run / Cmd+Enter. Order of precedence:
//   1. If there's a non-empty selection in the editor, that's it.
//   2. Otherwise, the statement spanning the cursor (text between the
//      previous and next semicolons).
//   3. If the editor isn't ready or the document has no semicolons,
//      the whole editor contents.
//
// Naive boundary detection — semicolons inside string literals are
// not honoured. The user can work around that by selecting the
// statement explicitly, which path 1 catches.
function statementToRun(): string {
  const view = editorView.value
  if (!view) return sqlText.value.trim()

  const sel = view.state.selection.main
  if (sel.from !== sel.to) {
    return view.state.sliceDoc(sel.from, sel.to).trim()
  }
  const doc = view.state.doc.toString()
  if (!doc.includes(';')) return doc.trim()

  const cursor = sel.head
  let start = 0
  for (let i = cursor - 1; i >= 0; i--) {
    if (doc[i] === ';') {
      start = i + 1
      break
    }
  }
  let end = doc.length
  for (let i = cursor; i < doc.length; i++) {
    if (doc[i] === ';') {
      end = i
      break
    }
  }
  return doc.substring(start, end).trim()
}

async function runStatement(text: string) {
  if (!text) return
  if (needsConfirmation(text)) {
    modal.open(ConfirmDialog, {
      title: 'Run this query?',
      message: 'It contains DELETE, DROP, or TRUNCATE without a WHERE clause.',
      confirmLabel: 'Run query',
      onConfirm: () => {
        modal.close()
        void executeStatement(text)
      },
    })
    return
  }
  await executeStatement(text)
}

async function executeStatement(text: string) {
  running.value = true
  try {
    result.value = await runDBQuery(serviceName.value, namespace.value, {
      sql: text,
      database: activeDatabase.value || undefined,
      transaction: runAsTransaction.value,
      no_limit: noLimit.value,
    })
    if (result.value.error) {
      toast.error(result.value.error)
    } else if (looksLikeDDL(text)) {
      // The user just ran DDL via the SQL tab — schema or structure
      // probably changed. Refresh both so the Designer / Indexes /
      // schema sidebar reflect reality.
      await Promise.all([
        loadSchema(),
        selectedRelation.value ? loadRows() : Promise.resolve(),
      ])
      // Drop the cached column list for the open table so autocomplete
      // picks up any new / dropped columns.
      if (selectedRelation.value) {
        const key = `${selectedRelation.value.schema}.${selectedRelation.value.name}`
        const next = { ...tableColumnsCache.value }
        delete next[key]
        tableColumnsCache.value = next
      }
    }
    // History is recorded synchronously on the server before the response
    // is returned, so we can re-fetch immediately.
    loadHistory()
  } finally {
    running.value = false
  }
}

// looksLikeDDL is a coarse heuristic — matches statements that start
// with a keyword likely to change schema. Fine for "should I refresh
// the UI after this?" but not used for any safety-critical logic.
function looksLikeDDL(sql: string): boolean {
  return /^\s*(create|alter|drop|truncate|comment\s+on|grant|revoke)\b/i.test(sql)
}

async function runQuery() {
  await runStatement(statementToRun())
}

async function runQueryAll() {
  await runStatement(sqlText.value.trim())
}

function needsConfirmation(s: string): boolean {
  const lower = s.toLowerCase()
  if (lower.includes('drop ') || lower.includes('truncate ')) return true
  if (lower.startsWith('delete') && !lower.includes(' where ')) return true
  return false
}

function formatSQL() {
  const keywords = [
    'select', 'from', 'where', 'and', 'or', 'order by', 'group by', 'having',
    'limit', 'offset', 'insert into', 'update', 'set', 'delete from',
    'inner join', 'left join', 'right join', 'on', 'as', 'values',
    'create table', 'create index', 'alter table', 'drop table', 'drop index',
  ]
  let out = sqlText.value
  for (const kw of keywords) {
    const re = new RegExp('\\b' + kw.replace(/ /g, '\\s+') + '\\b', 'gi')
    out = out.replace(re, kw.toUpperCase())
  }
  sqlText.value = out
}

function exportCSV() {
  if (!result.value?.columns || !result.value.rows) return
  const header = result.value.columns.map((c) => csvEscape(c.name)).join(',')
  const body = result.value.rows.map((r) => r.map(csvEscape).join(',')).join('\n')
  const csv = header + '\n' + body
  const blob = new Blob([csv], { type: 'text/csv' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `${serviceName.value}-query.csv`
  a.click()
  URL.revokeObjectURL(url)
}

function csvEscape(v: unknown): string {
  if (v === null || v === undefined) return ''
  const s = typeof v === 'string' ? v : JSON.stringify(v)
  if (/[",\n]/.test(s)) return '"' + s.replace(/"/g, '""') + '"'
  return s
}

// Cmd/Ctrl+Enter is now wired through CodeMirror's keymap (sqlKeymap)
// rather than a document-level listener — that lets CodeMirror consume
// the event before its contentEditable inserts a newline.

// --- Row browser (G2) ---
const browseData = ref<DBRowsResponse | null>(null)
const browseLoading = ref(false)
const browseError = ref<string | null>(null)
const pageSize = ref(50)
const offset = ref(0)
const sortCol = ref<string>('')
const sortDir = ref<'ASC' | 'DESC'>('ASC')
const filterCol = ref<string>('')
const filterVal = ref<string>('')

// Pending edits keyed by `<rowIndex>:<colName>`. The UI shows a dirty
// indicator and offers Save / Discard actions.
const pendingEdits = ref<Record<string, unknown>>({})
const editingCell = ref<{ row: number; col: string } | null>(null)
const editBuffer = ref<string>('')

// Bulk-selected row indexes for Delete.
const selectedRows = ref<Set<number>>(new Set())

// Insert form state.
const showInsertForm = ref(false)
const insertValues = ref<Record<string, unknown>>({})

// Column header menu state.
const openHeaderMenu = ref<string | null>(null)

const dirtyCount = computed(() => Object.keys(pendingEdits.value).length)

async function loadRows() {
  if (!selectedRelation.value) return
  browseLoading.value = true
  browseError.value = null
  pendingEdits.value = {}
  selectedRows.value = new Set()
  editingCell.value = null
  try {
    browseData.value = await fetchDBRows(
      serviceName.value,
      namespace.value,
      selectedRelation.value.schema,
      selectedRelation.value.name,
      {
        limit: pageSize.value,
        offset: offset.value,
        order_by: sortCol.value || undefined,
        order_dir: sortCol.value ? sortDir.value : undefined,
        filter_col: filterCol.value || undefined,
        filter_val: filterCol.value ? filterVal.value : undefined,
      },
      activeDatabase.value,
    )
  } catch (e: unknown) {
    const err = e as { response?: { data?: { error?: string } }; message?: string }
    browseError.value = err.response?.data?.error || err.message || 'Failed to load rows'
  } finally {
    browseLoading.value = false
  }
}

watch(selectedRelation, () => {
  offset.value = 0
  sortCol.value = ''
  filterCol.value = ''
  filterVal.value = ''
  if (selectedRelation.value) loadRows()
})

watch(offset, loadRows)

function startEdit(rowIdx: number, col: string, currentVal: unknown) {
  editingCell.value = { row: rowIdx, col }
  editBuffer.value = currentVal === null || currentVal === undefined ? '' : String(currentVal)
}

function commitEdit() {
  if (!editingCell.value || !browseData.value) return
  const { row, col } = editingCell.value
  const original = (browseData.value.rows[row] as unknown[])[colIndex(col)]
  // Only mark dirty if it actually changed.
  const newVal = parseEditBuffer(editBuffer.value, col)
  if (String(newVal) !== String(original)) {
    pendingEdits.value[`${row}:${col}`] = newVal
  } else {
    delete pendingEdits.value[`${row}:${col}`]
  }
  editingCell.value = null
}

function cancelEdit() {
  editingCell.value = null
}

function parseEditBuffer(buf: string, col: string): unknown {
  if (buf === '__null__' || buf === '') {
    // Empty → if column is NOT NULL we leave as ""; user can opt into NULL via __null__ sentinel.
    if (buf === '__null__') return null
  }
  const c = browseData.value?.structure.columns.find((x) => x.name === col)
  if (!c) return buf
  // Numeric coercion for obvious cases — types like "integer", "bigint", "smallint", "numeric".
  const lower = c.type.toLowerCase()
  if (/(int|numeric|decimal|real|double|float)/.test(lower)) {
    const n = Number(buf)
    if (!Number.isNaN(n)) return n
  }
  if (/bool/.test(lower)) {
    if (buf === 'true') return true
    if (buf === 'false') return false
  }
  return buf
}

function colIndex(name: string): number {
  return browseData.value?.structure.columns.findIndex((c) => c.name === name) ?? -1
}

function pkOf(rowIdx: number): Record<string, unknown> | null {
  if (!browseData.value) return null
  const pkCols = browseData.value.structure.primary_key
  if (!pkCols.length) return null
  const out: Record<string, unknown> = {}
  for (const c of pkCols) {
    const i = colIndex(c)
    if (i < 0) return null
    out[c] = (browseData.value.rows[rowIdx] as unknown[])[i]
  }
  return out
}

async function saveEdits() {
  if (!browseData.value || !selectedRelation.value) return
  if (browseData.value.structure.primary_key.length === 0) {
    toast.error('Table has no primary key: cannot save edits via PK. Use the SQL tab.')
    return
  }
  // Group edits by row.
  const byRow: Record<number, Record<string, unknown>> = {}
  for (const key of Object.keys(pendingEdits.value)) {
    const [r, c] = key.split(':')
    const ri = Number(r)
    byRow[ri] = byRow[ri] || {}
    byRow[ri][c] = pendingEdits.value[key]
  }
  let ok = 0
  for (const r of Object.keys(byRow)) {
    const rowIdx = Number(r)
    const pk = pkOf(rowIdx)
    if (!pk) {
      toast.error('Row has no primary key and cannot be saved')
      continue
    }
    try {
      await updateDBRow(
        serviceName.value,
        namespace.value,
        selectedRelation.value.schema,
        selectedRelation.value.name,
        pk,
        byRow[rowIdx],
        activeDatabase.value,
      )
      ok++
    } catch (e: unknown) {
      const err = e as { response?: { data?: { error?: string } } }
      toast.error(err.response?.data?.error || 'Update failed')
    }
  }
  if (ok > 0) toast.success(`Saved ${ok} row${ok === 1 ? '' : 's'}`)
  pendingEdits.value = {}
  await loadRows()
}

function discardEdits() {
  pendingEdits.value = {}
  editingCell.value = null
}

function deleteSelected() {
  if (!browseData.value || !selectedRelation.value || selectedRows.value.size === 0) return
  const count = selectedRows.value.size
  modal.open(ConfirmDialog, {
    title: `Delete ${count} row${count === 1 ? '' : 's'}?`,
    message: 'This cannot be undone.',
    confirmLabel: 'Delete',
    onConfirm: async () => {
      modal.close()
      const pks: Array<Record<string, unknown>> = []
      for (const idx of selectedRows.value) {
        const pk = pkOf(idx)
        if (pk) pks.push(pk)
      }
      if (pks.length === 0) {
        toast.error('Selected rows have no primary key')
        return
      }
      try {
        const res = await deleteDBRows(
          serviceName.value,
          namespace.value,
          selectedRelation.value!.schema,
          selectedRelation.value!.name,
          pks,
          activeDatabase.value,
        )
        toast.success(`Deleted ${res.rows_affected} row${res.rows_affected === 1 ? '' : 's'}`)
      } catch (e: unknown) {
        const err = e as { response?: { data?: { error?: string } } }
        toast.error(err.response?.data?.error || 'Delete failed')
      }
      selectedRows.value = new Set()
      await loadRows()
    },
  })
}

function toggleRowSelected(rowIdx: number) {
  const next = new Set(selectedRows.value)
  if (next.has(rowIdx)) next.delete(rowIdx)
  else next.add(rowIdx)
  selectedRows.value = next
}

function toggleAllSelected() {
  if (!browseData.value) return
  if (selectedRows.value.size === browseData.value.rows.length) {
    selectedRows.value = new Set()
  } else {
    selectedRows.value = new Set(browseData.value.rows.map((_, i) => i))
  }
}

function startInsert() {
  if (!browseData.value) return
  insertValues.value = {}
  for (const c of browseData.value.structure.columns) {
    if (!c.generated && !c.default) insertValues.value[c.name] = ''
  }
  showInsertForm.value = true
}

async function submitInsert() {
  if (!browseData.value || !selectedRelation.value) return
  // Drop empty strings so DB defaults apply, except for explicit nulls.
  const payload: Record<string, unknown> = {}
  for (const k of Object.keys(insertValues.value)) {
    const v = insertValues.value[k]
    if (v === '' || v === undefined) continue
    payload[k] = parseEditBuffer(String(v), k)
  }
  try {
    await insertDBRow(serviceName.value, namespace.value, selectedRelation.value.schema, selectedRelation.value.name, payload, activeDatabase.value)
    toast.success('Row inserted')
    showInsertForm.value = false
    await loadRows()
  } catch (e: unknown) {
    const err = e as { response?: { data?: { error?: string } } }
    toast.error(err.response?.data?.error || 'Insert failed')
  }
}

function copyToClipboard(text: string) {
  navigator.clipboard.writeText(text)
  openHeaderMenu.value = null
}

function setSort(col: string) {
  if (sortCol.value === col) {
    sortDir.value = sortDir.value === 'ASC' ? 'DESC' : 'ASC'
  } else {
    sortCol.value = col
    sortDir.value = 'ASC'
  }
  openHeaderMenu.value = null
  offset.value = 0
  loadRows()
}

function applyFilter(col: string) {
  // Only opens the filter input — does NOT run the query yet, because
  // we don't have a filter value. The previous version kicked off
  // loadRows immediately, which sent filter_val="" and exploded for
  // any non-text column (Postgres rejects "" for bigint, uuid, etc.).
  filterCol.value = col
  filterVal.value = ''
  openHeaderMenu.value = null
}

function clearFilter() {
  filterCol.value = ''
  filterVal.value = ''
  offset.value = 0
  loadRows()
}

function fkFor(col: string) {
  return browseData.value?.structure.foreign_keys.find((fk) => fk.column === col)
}

function navigateToFK(fk: { ref_schema: string; ref_table: string }) {
  selectedRelation.value = { schema: fk.ref_schema, name: fk.ref_table }
}

function nextPage() { if (browseData.value && offset.value + pageSize.value < browseData.value.total) offset.value += pageSize.value }
function prevPage() { offset.value = Math.max(0, offset.value - pageSize.value) }
function firstPage() { offset.value = 0 }
function lastPage() {
  if (!browseData.value) return
  offset.value = Math.max(0, Math.floor((browseData.value.total - 1) / pageSize.value) * pageSize.value)
}

const totalPages = computed(() => browseData.value ? Math.ceil(browseData.value.total / pageSize.value) : 0)
const currentPage = computed(() => Math.floor(offset.value / pageSize.value) + 1)

onMounted(() => {
  loadDatabases()
  loadSchema()
  loadSnippets()
  loadHistory()
})

watch(serviceName, () => {
  loadDatabases()
  loadSchema()
})

// React to URL-driven database switches (back/forward buttons, the
// picker, or programmatic router pushes from elsewhere) so the schema
// sidebar stays in sync with the active database. We also flush the
// per-table column cache — its keys are `<schema>.<table>` and would
// otherwise serve DB A's columns to DB B if both have, say,
// public.users with different shapes.
watch(activeDatabase, async (next, prev) => {
  if (next === prev) return
  selectedRelation.value = null
  browseData.value = null
  tableColumnsCache.value = {}
  await loadSchema()
})

function back() {
  router.push({ name: 'services' })
}

function cellValue(rowIdx: number, col: string): unknown {
  if (!browseData.value) return null
  const key = `${rowIdx}:${col}`
  if (key in pendingEdits.value) return pendingEdits.value[key]
  const i = colIndex(col)
  return i < 0 ? null : (browseData.value.rows[rowIdx] as unknown[])[i]
}

function isCellDirty(rowIdx: number, col: string): boolean {
  return `${rowIdx}:${col}` in pendingEdits.value
}

function displayCell(v: unknown): string {
  if (v === null) return 'NULL'
  if (v === undefined) return ''
  if (typeof v === 'object') return JSON.stringify(v)
  return String(v)
}

// --- Snippets + history (G5) ---
const snippets = ref<DBSnippet[]>([])
const history = ref<DBHistoryEntry[]>([])
const sidebarMode = ref<'snippets' | 'history'>('snippets')
const showSaveSnippet = ref(false)
const newSnippetName = ref('')
const newSnippetPinned = ref(false)

async function loadSnippets() {
  try {
    snippets.value = await fetchDBSnippets(serviceName.value, namespace.value)
  } catch {
    snippets.value = []
  }
}

async function loadHistory() {
  try {
    history.value = await fetchDBHistory(serviceName.value, namespace.value)
  } catch {
    history.value = []
  }
}

function loadSnippet(s: DBSnippet) {
  sqlText.value = s.sql
  activeTab.value = 'sql'
}

async function pinSnippet(s: DBSnippet) {
  try {
    const updated = await saveDBSnippet(serviceName.value, namespace.value, { ...s, pinned: !s.pinned })
    const idx = snippets.value.findIndex((x) => x.name === s.name)
    if (idx >= 0) snippets.value[idx] = updated
    snippets.value = [...snippets.value].sort((a, b) => {
      if (!!a.pinned !== !!b.pinned) return a.pinned ? -1 : 1
      return a.name.toLowerCase().localeCompare(b.name.toLowerCase())
    })
  } catch {
    toast.error('Failed to update snippet')
  }
}

function removeSnippet(s: DBSnippet) {
  modal.open(ConfirmDialog, {
    title: `Delete snippet ${s.name}?`,
    confirmLabel: 'Delete',
    onConfirm: async () => {
      modal.close()
      try {
        await deleteDBSnippet(serviceName.value, namespace.value, s.name)
        snippets.value = snippets.value.filter((x) => x.name !== s.name)
        toast.success('Snippet deleted')
      } catch {
        toast.error('Delete failed')
      }
    },
  })
}

async function saveCurrentAsSnippet() {
  if (!sqlText.value.trim()) {
    toast.error('Editor is empty. Write a query first')
    return
  }
  if (!newSnippetName.value.trim()) {
    toast.error('Name is required')
    return
  }
  try {
    const saved = await saveDBSnippet(serviceName.value, namespace.value, {
      name: newSnippetName.value.trim(),
      sql: sqlText.value,
      pinned: newSnippetPinned.value,
    })
    const idx = snippets.value.findIndex((x) => x.name === saved.name)
    if (idx >= 0) snippets.value[idx] = saved
    else snippets.value = [saved, ...snippets.value]
    snippets.value = [...snippets.value].sort((a, b) => {
      if (!!a.pinned !== !!b.pinned) return a.pinned ? -1 : 1
      return a.name.toLowerCase().localeCompare(b.name.toLowerCase())
    })
    toast.success(`Saved snippet "${saved.name}"`)
    showSaveSnippet.value = false
    newSnippetName.value = ''
    newSnippetPinned.value = false
  } catch (e: unknown) {
    const err = e as { response?: { data?: { error?: string } } }
    toast.error(err.response?.data?.error || 'Save failed')
  }
}

function loadHistoryEntry(h: DBHistoryEntry) {
  sqlText.value = h.sql
  activeTab.value = 'sql'
}

function relativeTime(iso: string): string {
  const t = new Date(iso).getTime()
  if (!t) return ''
  const diff = (Date.now() - t) / 1000
  if (diff < 60) return `${Math.floor(diff)}s ago`
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
  return `${Math.floor(diff / 86400)}d ago`
}

// --- AI panel (G4) ---
const showAI = ref(false)
const aiChatRef = ref<InstanceType<typeof AIChat> | null>(null)

// Best-effort dialect inference. The schema endpoint doesn't tell us
// the driver directly; for the common case we're on Postgres so
// default to that. MySQL service rows on the list page route here too,
// so we trust the URL-side type when it's available.
function inferDialect(): 'postgres' | 'mysql' {
  // The Services list passes service type via route state in some flows;
  // for now we infer from the schema shape: Postgres exposes "public"
  // by default, MySQL uses information_schema with no concept of
  // schemas. If we have any database with a schema named "public" it's
  // postgres; otherwise mysql.
  for (const db of schema.value.databases) {
    for (const sch of db.schemas) {
      if (sch.name === 'public') return 'postgres'
    }
  }
  return schema.value.databases.length > 0 ? 'mysql' : 'postgres'
}

// Build the chat context from whatever schema we know about. If the
// user has a table open in the Browse / Designer / Indexes tab, we
// prioritise its full structure (columns + indexes + FKs); other
// tables are listed shape-only so the model sees the whole database.
const aiSchemaContext = computed<ChatDBSchema>(() => {
  const tables: ChatDBTable[] = []
  // Always include the currently-selected table with full detail when
  // we have it loaded.
  if (browseData.value && selectedRelation.value) {
    const s = browseData.value.structure
    tables.push({
      schema: s.schema,
      name: s.name,
      columns: s.columns.map((c) => ({
        name: c.name,
        type: c.type,
        nullable: c.nullable,
        pk: s.primary_key.includes(c.name),
      })),
      indexes: s.indexes.map((idx) => ({
        name: idx.name,
        columns: idx.columns,
        unique: idx.unique,
      })),
      foreign_keys: s.foreign_keys.map((fk) => ({
        column: fk.column,
        ref_schema: fk.ref_schema,
        ref_table: fk.ref_table,
        ref_column: fk.ref_column,
      })),
    })
  }
  // Other tables: shape only. Helps the model write joins without
  // forcing us to fetch every structure up front.
  for (const db of schema.value.databases) {
    for (const sch of db.schemas) {
      for (const rel of sch.relations) {
        if (rel.kind !== 'table') continue
        if (selectedRelation.value && sch.name === selectedRelation.value.schema && rel.name === selectedRelation.value.name) continue
        tables.push({ schema: sch.name, name: rel.name, columns: [] })
      }
    }
  }
  return { dialect: inferDialect(), tables }
})

function onAIApply(text: string) {
  // The AI returns a single SQL block; drop it into the editor and
  // switch the user to the SQL tab so they can read + run.
  sqlText.value = text
  activeTab.value = 'sql'
  showAI.value = false
}

function explainQuery() {
  if (!sqlText.value.trim()) {
    toast.info('Write a query first, then I can explain it')
    return
  }
  showAI.value = true
  // Hand the prompt to the panel; AIChat will append the SQL via the
  // sql prop the model already sees in its system prompt.
  aiChatRef.value?.submitPrompt('Explain this query in plain English. Walk me through what it does, list the indexes it touches, and call out anything obviously slow.')
}

// --- Create new table (G3.5b) ---
//
// Sits next to the Alter Designer; both share the same column-spec
// row UI. Clicking "+ New table" in the schema sidebar drops into
// this mode and the Designer tab renders the create form instead of
// the alter form.
const creatingTable = ref(false)
const newTableSchema = ref('public')
const newTableName = ref('')
const newTableCols = ref<DBColumnSpec[]>([])
const newTablePK = ref<string[]>([])
const newTablePreview = ref<string[]>([])

function defaultNewTableColumns(): DBColumnSpec[] {
  return [
    { name: 'id', type: 'bigserial', nullable: false, default: null },
    { name: 'created_at', type: 'timestamptz', nullable: false, default: 'now()' },
  ]
}

function startCreateTable() {
  creatingTable.value = true
  selectedRelation.value = null
  activeTab.value = 'designer'
  newTableSchema.value = (schema.value.databases[0]?.schemas[0]?.name) || 'public'
  newTableName.value = ''
  newTableCols.value = defaultNewTableColumns()
  newTablePK.value = ['id']
}

function addNewTableCol() {
  newTableCols.value.push({ name: '', type: 'text', nullable: true, default: null })
}

function removeNewTableCol(idx: number) {
  const removed = newTableCols.value[idx]
  newTableCols.value.splice(idx, 1)
  newTablePK.value = newTablePK.value.filter((c) => c !== removed.name)
}

function toggleNewTablePK(name: string) {
  if (!name) return
  if (newTablePK.value.includes(name)) {
    newTablePK.value = newTablePK.value.filter((c) => c !== name)
  } else {
    newTablePK.value = [...newTablePK.value, name]
  }
}

function cancelCreateTable() {
  creatingTable.value = false
  newTableCols.value = []
  newTablePK.value = []
  newTablePreview.value = []
}

// Debounced preview generation. Without this, every keystroke fires a
// separate /db/ddl/preview request and out-of-order responses can leave
// the pane showing a stale partial-name version of the DDL.
let newTablePreviewTimer: ReturnType<typeof setTimeout> | null = null
let newTablePreviewToken = 0

function scheduleNewTablePreview() {
  if (newTablePreviewTimer) clearTimeout(newTablePreviewTimer)
  newTablePreviewTimer = setTimeout(refreshNewTablePreview, 200)
}

async function refreshNewTablePreview() {
  if (!creatingTable.value) {
    newTablePreview.value = []
    return
  }
  if (!newTableName.value || newTableCols.value.length === 0) {
    newTablePreview.value = []
    return
  }
  const cols = newTableCols.value.filter((c) => c.name && c.type)
  if (cols.length === 0) {
    newTablePreview.value = []
    return
  }
  const constraints = newTablePK.value.length > 0
    ? [{ type: 'PRIMARY KEY' as const, columns: newTablePK.value.filter((c) => cols.some((x) => x.name === c)) }]
    : []
  const myToken = ++newTablePreviewToken
  try {
    const res = await previewDBDDL(serviceName.value, namespace.value, {
      create_table: {
        schema: newTableSchema.value,
        name: newTableName.value,
        columns: cols,
        constraints,
      },
    }, activeDatabase.value)
    if (myToken !== newTablePreviewToken) return
    newTablePreview.value = res.ddl || []
  } catch (e: unknown) {
    if (myToken !== newTablePreviewToken) return
    const err = e as { response?: { data?: { error?: string } } }
    newTablePreview.value = ['-- preview error: ' + (err.response?.data?.error || 'unknown')]
  }
}

watch([newTableSchema, newTableName, newTableCols, newTablePK], scheduleNewTablePreview, { deep: true })

async function applyCreateTable() {
  // Flush any pending debounced preview first so the active inputs are
  // committed before we read them. Important when the user types
  // quickly and clicks Create before the 200ms debounce window expires.
  if (newTablePreviewTimer) {
    clearTimeout(newTablePreviewTimer)
    newTablePreviewTimer = null
  }
  // Read freshly-trimmed values to avoid creating a table named " " or
  // a duplicated name with trailing whitespace.
  const schemaName = newTableSchema.value.trim() || 'public'
  const tableName = newTableName.value.trim()

  if (!tableName) {
    toast.error('Table name is required')
    return
  }
  if (tableName.length < 2) {
    toast.error('Table name looks too short: finish typing then try again')
    return
  }
  const cols = newTableCols.value
    .map((c) => ({ ...c, name: (c.name || '').trim(), type: (c.type || '').trim() }))
    .filter((c) => c.name && c.type)
  if (cols.length === 0) {
    toast.error('At least one column is required')
    return
  }
  const constraints = newTablePK.value.length > 0
    ? [{ type: 'PRIMARY KEY' as const, columns: newTablePK.value.filter((c) => cols.some((x) => x.name === c)) }]
    : []
  try {
    await createDBTable(serviceName.value, namespace.value, {
      schema: schemaName,
      name: tableName,
      columns: cols,
      constraints,
    }, activeDatabase.value)
    toast.success(`Created ${schemaName}.${tableName}`)
    cancelCreateTable()
    // Drop any stale cache entry so loadSchema → loadAllTableColumns
    // re-fetches the columns we just created.
    const key = `${schemaName}.${tableName}`
    const next = { ...tableColumnsCache.value }
    delete next[key]
    tableColumnsCache.value = next
    await loadSchema()
    selectedRelation.value = { schema: schemaName, name: tableName }
  } catch (e: unknown) {
    const err = e as { response?: { data?: { error?: string } } }
    toast.error(err.response?.data?.error || 'Create table failed')
  }
}

// --- Designer (G3) ---
//
// The designer holds an ordered list of "ops" that the user has built
// up via add-column / drop-column / etc. The DDL preview pane is
// driven by a debounced server-side preview call; the user can read
// what's about to run before clicking Apply.
const designerOps = ref<DBTableOp[]>([])
const designerPreview = ref<string[]>([])
const designerApplying = ref(false)
const showAddColumn = ref(false)
const newCol = ref<DBColumnSpec>({ name: '', type: 'text', nullable: true })
const runWithBackup = ref(false)

// Grouped column-type options for the type picker. Postgres-flavoured;
// MySQL accepts most of the same names. The "Custom..." sentinel
// switches the row to a free-form text input so power users can pick
// parameterised types like varchar(50), numeric(10,2), or extension
// types like vector(384).
interface TypeGroup {
  label: string
  types: string[]
}

const TYPE_GROUPS: TypeGroup[] = [
  { label: 'Text', types: ['text', 'varchar', 'char(36)', 'citext'] },
  { label: 'Numbers', types: ['integer', 'bigint', 'smallint', 'numeric', 'real', 'double precision'] },
  { label: 'Auto-increment', types: ['bigserial', 'serial', 'smallserial'] },
  { label: 'Boolean', types: ['boolean'] },
  { label: 'Date/Time', types: ['timestamptz', 'timestamp', 'date', 'time', 'interval'] },
  { label: 'JSON', types: ['jsonb', 'json'] },
  { label: 'UUID', types: ['uuid'] },
  { label: 'Binary', types: ['bytea'] },
  { label: 'Network', types: ['inet', 'cidr', 'macaddr'] },
  { label: 'Search', types: ['tsvector', 'tsquery'] },
]

const CUSTOM_SENTINEL = '__custom__'

const ALL_PRESET_TYPES = new Set<string>(TYPE_GROUPS.flatMap((g) => g.types))

function isCustomType(t: string | undefined): boolean {
  if (!t) return false
  return !ALL_PRESET_TYPES.has(t)
}

function dialectTypeHints(): string[] {
  // Flat list of every preset type, for datalists that take a flat
  // array rather than the grouped TYPE_GROUPS.
  return Array.from(ALL_PRESET_TYPES)
}

function hasOp(op: DBTableOp): boolean {
  return designerOps.value.some((x) => JSON.stringify(x) === JSON.stringify(op))
}

function addOp(op: DBTableOp) {
  if (!hasOp(op)) designerOps.value.push(op)
}

function removeOp(idx: number) {
  designerOps.value.splice(idx, 1)
}

function queueAddColumn() {
  if (!newCol.value.name || !newCol.value.type) {
    toast.error('Column name and type are required')
    return
  }
  addOp({ op: 'add_column', column: { ...newCol.value } })
  newCol.value = { name: '', type: 'text', nullable: true }
  showAddColumn.value = false
}

function queueDropColumn(name: string) {
  modal.open(ConfirmDialog, {
    title: `Drop column ${name}?`,
    message: 'Data in this column will be lost when you apply the change.',
    confirmLabel: 'Drop column',
    onConfirm: () => {
      modal.close()
      addOp({ op: 'drop_column', name })
    },
  })
}

// In-place rename editor — mirrors the inline type picker so both
// destructive-shaped operations have a consistent UX. The Name cell
// becomes an editable input until the user hits Apply or Cancel.
const editingRenameFor = ref<string | null>(null)
const editingRenameValue = ref('')

function startRenameEdit(col: { name: string }) {
  editingRenameFor.value = col.name
  editingRenameValue.value = col.name
}

function commitRenameEdit() {
  if (!editingRenameFor.value) return
  const oldName = editingRenameFor.value
  const newName = editingRenameValue.value.trim()
  if (newName && newName !== oldName) {
    addOp({ op: 'rename_column', old_name: oldName, new_name: newName })
  }
  editingRenameFor.value = null
  editingRenameValue.value = ''
}

function cancelRenameEdit() {
  editingRenameFor.value = null
  editingRenameValue.value = ''
}

// In-place type editor state. The Designer table swaps the Type cell
// into a select+custom picker when the user clicks "Type…" — same
// pattern as the Add Column form so users don't need to remember
// dialect-specific spelling.
const editingTypeFor = ref<string | null>(null)
const editingTypeValue = ref('')

function startTypeEdit(col: { name: string; type: string }) {
  editingTypeFor.value = col.name
  editingTypeValue.value = col.type
}

function commitTypeEdit() {
  if (!editingTypeFor.value) return
  const colName = editingTypeFor.value
  const newType = editingTypeValue.value.trim()
  const orig = browseData.value?.structure.columns.find((c) => c.name === colName)
  if (newType && orig && newType !== orig.type) {
    addOp({ op: 'alter_column_type', column: { name: colName, type: newType, nullable: orig.nullable } })
  }
  editingTypeFor.value = null
  editingTypeValue.value = ''
}

function cancelTypeEdit() {
  editingTypeFor.value = null
  editingTypeValue.value = ''
}

function queueToggleNullable(col: { name: string; nullable: boolean }) {
  addOp({
    op: 'set_nullable',
    column: { name: col.name, type: '', nullable: !col.nullable },
  })
}

let designerPreviewTimer: ReturnType<typeof setTimeout> | null = null
let designerPreviewToken = 0

watch(designerOps, () => {
  if (designerPreviewTimer) clearTimeout(designerPreviewTimer)
  designerPreviewTimer = setTimeout(async () => {
    if (!selectedRelation.value || designerOps.value.length === 0) {
      designerPreview.value = []
      return
    }
    const myToken = ++designerPreviewToken
    try {
      const res = await previewDBDDL(serviceName.value, namespace.value, {
        alter_table: {
          schema: selectedRelation.value.schema,
          table: selectedRelation.value.name,
          ops: designerOps.value.slice(),
        },
      }, activeDatabase.value)
      if (myToken !== designerPreviewToken) return
      designerPreview.value = res.ddl || []
    } catch (e: unknown) {
      if (myToken !== designerPreviewToken) return
      const err = e as { response?: { data?: { error?: string } } }
      designerPreview.value = ['-- preview error: ' + (err.response?.data?.error || 'unknown')]
    }
  }, 200)
}, { deep: true })

function applyDesigner() {
  if (!selectedRelation.value || designerOps.value.length === 0) return
  const count = designerOps.value.length
  modal.open(ConfirmDialog, {
    title: `Apply ${count} change${count === 1 ? '' : 's'}?`,
    message: 'The changes run as one transaction against the live table.',
    confirmLabel: 'Apply changes',
    onConfirm: async () => {
      modal.close()
      designerApplying.value = true
      try {
        const res = await alterDBTable(
          serviceName.value,
          namespace.value,
          selectedRelation.value!.schema,
          selectedRelation.value!.name,
          designerOps.value.slice(),
          activeDatabase.value,
        )
        toast.success(`Applied ${res.ddl.length} statement${res.ddl.length === 1 ? '' : 's'}`)
        designerOps.value = []
        designerPreview.value = []
        // Invalidate the cached column list for this table so the next
        // autocomplete pass picks up the new shape.
        if (selectedRelation.value) {
          const key = `${selectedRelation.value.schema}.${selectedRelation.value.name}`
          const next = { ...tableColumnsCache.value }
          delete next[key]
          tableColumnsCache.value = next
        }
        // Reload structure + rows so the user sees the new shape immediately.
        await loadRows()
        await loadSchema()
      } catch (e: unknown) {
        const err = e as { response?: { data?: { error?: string } } }
        toast.error(err.response?.data?.error || 'Apply failed')
      } finally {
        designerApplying.value = false
      }
    },
  })
}

// --- Indexes (G3) ---
const showAddIndex = ref(false)
const newIndex = ref<DBIndexPayload>({
  schema: '', table: '', name: '', columns: [], unique: false, method: '', where: '', concurrent: false,
})
const indexPreview = ref<string[]>([])

// Columns from the active table that haven't already been added to the
// new index. Drives the "+ Add column" picker so the user can only
// select real columns and never duplicates.
const availableIndexColumns = computed(() => {
  if (!browseData.value) return []
  const taken = new Set(newIndex.value.columns)
  return browseData.value.structure.columns.filter((c) => !taken.has(c.name))
})

// The custom-type sentinel maps back to an empty string so the free-text
// type input takes over.
function typeSelectValue(e: Event): string {
  const value = (e.target as HTMLSelectElement).value
  return value === CUSTOM_SENTINEL ? '' : value
}

function addIndexColumnFromSelect(e: Event) {
  const select = e.target as HTMLSelectElement
  addIndexColumn(select.value)
  select.value = ''
}

function addIndexColumn(name: string) {
  if (!name) return
  if (newIndex.value.columns.includes(name)) return
  newIndex.value.columns = [...newIndex.value.columns, name]
}

function removeIndexColumn(name: string) {
  newIndex.value.columns = newIndex.value.columns.filter((c) => c !== name)
}

function moveIndexColumn(idx: number, dir: -1 | 1) {
  const next = [...newIndex.value.columns]
  const target = idx + dir
  if (target < 0 || target >= next.length) return
  ;[next[idx], next[target]] = [next[target], next[idx]]
  newIndex.value.columns = next
}

function suggestedIndexName(): string {
  if (!newIndex.value.table || newIndex.value.columns.length === 0) return ''
  const cols = newIndex.value.columns.join('_')
  return `${newIndex.value.table}_${cols}_idx`
}

watch(selectedRelation, () => {
  if (selectedRelation.value) {
    newIndex.value.schema = selectedRelation.value.schema
    newIndex.value.table = selectedRelation.value.name
  }
})

let indexPreviewTimer: ReturnType<typeof setTimeout> | null = null
let indexPreviewToken = 0

watch(newIndex, () => {
  if (indexPreviewTimer) clearTimeout(indexPreviewTimer)
  indexPreviewTimer = setTimeout(async () => {
    if (!newIndex.value.name || newIndex.value.columns.length === 0) {
      indexPreview.value = []
      return
    }
    const myToken = ++indexPreviewToken
    try {
      const res = await previewDBDDL(serviceName.value, namespace.value, { create_index: { ...newIndex.value } }, activeDatabase.value)
      if (myToken !== indexPreviewToken) return
      indexPreview.value = res.ddl || []
    } catch (e: unknown) {
      if (myToken !== indexPreviewToken) return
      const err = e as { response?: { data?: { error?: string } } }
      indexPreview.value = ['-- preview error: ' + (err.response?.data?.error || 'unknown')]
    }
  }, 200)
}, { deep: true })

async function applyCreateIndex() {
  // Default the name to a sensible suggestion if the user left it
  // blank — the picker collected the columns visually so we can build
  // a unique name without asking again.
  if (!newIndex.value.name) {
    newIndex.value.name = suggestedIndexName()
  }
  if (!newIndex.value.name || newIndex.value.columns.length === 0) {
    toast.error('Name and at least one column are required')
    return
  }
  try {
    await createDBIndex(serviceName.value, namespace.value, { ...newIndex.value }, activeDatabase.value)
    toast.success(`Index ${newIndex.value.name} created`)
    showAddIndex.value = false
    newIndex.value = {
      schema: selectedRelation.value?.schema || '',
      table: selectedRelation.value?.name || '',
      name: '', columns: [], unique: false, method: '', where: '', concurrent: false,
    }
    await loadRows()
  } catch (e: unknown) {
    const err = e as { response?: { data?: { error?: string } } }
    toast.error(err.response?.data?.error || 'Create index failed')
  }
}

function dropIndexByName(idx: { name: string; primary: boolean }) {
  if (idx.primary) {
    toast.error('Cannot drop the primary-key index')
    return
  }
  if (!selectedRelation.value) return
  modal.open(ConfirmDialog, {
    title: `Drop index ${idx.name}?`,
    confirmLabel: 'Drop index',
    onConfirm: async () => {
      modal.close()
      try {
        await dropDBIndex(serviceName.value, namespace.value, selectedRelation.value!.schema, idx.name, false, activeDatabase.value)
        toast.success(`Index ${idx.name} dropped`)
        await loadRows()
      } catch (e: unknown) {
        const err = e as { response?: { data?: { error?: string } } }
        toast.error(err.response?.data?.error || 'Drop index failed')
      }
    },
  })
}
</script>

<template>
  <div class="flex flex-col h-full">
    <!-- Header -->
    <div class="sticky top-0 z-10 flex items-center justify-between gap-4 px-6 py-3 bg-white dark:bg-slate-900 border-b border-slate-200 dark:border-slate-800">
      <div class="flex items-center gap-3 min-w-0">
        <button class="p-2 -ml-2 rounded hover:bg-slate-100 dark:hover:bg-slate-800" aria-label="Back" @click="back">
          <ArrowLeft class="w-4 h-4" />
        </button>
        <Database class="w-5 h-5 text-kipper-500" />
        <div class="min-w-0">
          <h1 class="text-lg font-semibold truncate">{{ serviceName }}</h1>
          <p class="text-xs text-slate-500">SQL editor and table browser</p>
        </div>
      </div>
      <div class="flex items-center gap-2">
        <button
          class="text-xs inline-flex items-center gap-1 px-2 py-1 rounded"
          :class="showAI ? 'bg-kipper-600 text-white' : 'hover:bg-slate-100 dark:hover:bg-slate-800'"
          @click="showAI = !showAI"
        >
          <Sparkles class="w-3.5 h-3.5" /> AI
        </button>
        <button class="text-xs inline-flex items-center gap-1 px-2 py-1 rounded hover:bg-slate-100 dark:hover:bg-slate-800" @click="loadSchema">
          <RefreshCw class="w-3.5 h-3.5" :class="schemaLoading ? 'animate-spin' : ''" /> Refresh schema
        </button>
      </div>
    </div>

    <div class="flex-1 flex flex-col md:flex-row min-h-0">
      <!-- Main column -->
      <div class="flex-1 flex min-w-0 min-h-0">
      <!-- Schema sidebar (collapsible) -->
      <aside
        class="shrink-0 border-r border-slate-200 dark:border-slate-800 bg-slate-50 dark:bg-slate-950 flex flex-col transition-[width] duration-150"
        :class="schemaCollapsed ? 'w-10' : 'w-60 xl:w-64'"
      >
        <!--
          Database picker. A postgres/mysql service can hold several
          databases (the default plus per-binding ones); without this
          control the user can't tell which database the schema tree
          and SQL editor are pointed at, and can run a DROP on the
          wrong one without any signal. The selection persists in the
          URL so reloads and shared links land on the same database.
        -->
        <div
          v-if="!schemaCollapsed"
          class="px-2 py-2 border-b border-slate-200 dark:border-slate-800 space-y-1"
        >
          <label class="text-[10px] font-semibold uppercase tracking-wide text-slate-500 px-1">Database</label>
          <div class="relative">
            <select
              :value="activeDatabase || (databases.find((d) => d.default)?.name ?? '')"
              :disabled="databasesLoading || databases.length === 0"
              class="w-full text-xs rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 px-2 py-1 pr-6 disabled:opacity-50"
              @change="onDatabaseChange(($event.target as HTMLSelectElement).value)"
            >
              <option v-if="databasesLoading" disabled value="">Loading…</option>
              <option v-else-if="databases.length === 0" disabled value="">No databases</option>
              <option v-for="d in databases" :key="d.name" :value="d.name">
                {{ d.name }}{{ d.default ? ' (default)' : '' }}
              </option>
            </select>
          </div>
        </div>
        <div class="flex items-center justify-between px-2 py-2 border-b border-slate-200 dark:border-slate-800">
          <span v-if="!schemaCollapsed" class="text-xs font-semibold uppercase tracking-wide text-slate-500 px-1">Schema</span>
          <div class="flex items-center gap-1">
            <button
              v-if="!schemaCollapsed"
              class="p-1 rounded hover:bg-slate-200 dark:hover:bg-slate-800 text-kipper-600"
              title="Create a new table"
              @click="startCreateTable"
            >
              <Plus class="w-3.5 h-3.5" />
            </button>
            <button
              class="p-1 rounded hover:bg-slate-200 dark:hover:bg-slate-800"
              :title="schemaCollapsed ? 'Expand schema' : 'Collapse schema'"
              @click="schemaCollapsed = !schemaCollapsed"
            >
              <ChevronRight v-if="schemaCollapsed" class="w-3.5 h-3.5" />
              <ChevronDown v-else class="w-3.5 h-3.5 -rotate-90" />
            </button>
          </div>
        </div>
        <div v-if="!schemaCollapsed" class="overflow-y-auto flex-1">
        <div v-if="schemaLoading" class="px-3 py-2 text-xs text-slate-500"><Loader2 class="inline w-3 h-3 animate-spin mr-1" /> Loading…</div>
        <div v-else-if="schemaError" class="px-3 py-2 text-xs text-rose-600">{{ schemaError }}</div>
        <div v-else-if="!schema.databases.length" class="px-3 py-2 text-xs text-slate-500">No databases visible.</div>
        <ul v-else class="text-sm">
          <li v-for="db in schema.databases" :key="db.name">
            <button class="w-full flex items-center gap-1 px-3 py-1 hover:bg-slate-100 dark:hover:bg-slate-800 text-left" @click="toggleDb(db.name)">
              <ChevronDown v-if="expandedDbs[db.name]" class="w-3 h-3 shrink-0" />
              <ChevronRight v-else class="w-3 h-3 shrink-0" />
              <Database class="w-3.5 h-3.5 text-slate-500 shrink-0" />
              <span class="truncate">{{ db.name }}</span>
            </button>
            <ul v-if="expandedDbs[db.name]" class="ml-4">
              <li v-for="sch in db.schemas" :key="sch.name">
                <button class="w-full flex items-center gap-1 px-3 py-1 hover:bg-slate-100 dark:hover:bg-slate-800 text-left" @click="toggleSchema(db.name, sch.name)">
                  <ChevronDown v-if="expandedSchemas[db.name + '/' + sch.name]" class="w-3 h-3 shrink-0" />
                  <ChevronRight v-else class="w-3 h-3 shrink-0" />
                  <span class="text-xs text-slate-500 truncate">{{ sch.name }}</span>
                  <span class="ml-auto text-[10px] text-slate-400">{{ sch.relations.length }}</span>
                </button>
                <ul v-if="expandedSchemas[db.name + '/' + sch.name]" class="ml-5">
                  <li v-for="rel in sch.relations" :key="rel.name">
                    <button
                      class="w-full flex items-center gap-1 px-3 py-1 text-left hover:bg-kipper-50 dark:hover:bg-slate-800"
                      :class="selectedRelation?.schema === sch.name && selectedRelation?.name === rel.name ? 'bg-kipper-100 dark:bg-kipper-900/30' : ''"
                      :title="`Click to browse rows: double-click to insert name into SQL editor`"
                      @click="openInBrowse(sch.name, rel.name)"
                      @dblclick="insertRelation(sch.name, rel.name)"
                    >
                      <TableIcon v-if="rel.kind === 'table'" class="w-3.5 h-3.5 text-kipper-500 shrink-0" />
                      <Eye v-else class="w-3.5 h-3.5 text-slate-400 shrink-0" />
                      <span class="text-xs truncate">{{ rel.name }}</span>
                    </button>
                  </li>
                </ul>
              </li>
            </ul>
          </li>
        </ul>
        </div>
      </aside>

      <!-- Main pane -->
      <main class="flex-1 flex flex-col min-w-0">
        <!-- Tabs -->
        <div class="flex flex-wrap items-center gap-1 px-4 pt-2 border-b border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900">
          <button
            class="px-3 py-1.5 text-sm rounded-t border-b-2"
            :class="activeTab === 'browse' ? 'border-kipper-500 text-kipper-700 dark:text-kipper-300' : 'border-transparent text-slate-500 hover:text-slate-700 dark:hover:text-slate-300'"
            @click="activeTab = 'browse'"
          >
            Browse
            <span v-if="selectedRelation" class="ml-1 text-xs text-slate-500">({{ selectedRelation.schema }}.{{ selectedRelation.name }})</span>
          </button>
          <button
            class="px-3 py-1.5 text-sm rounded-t border-b-2"
            :class="activeTab === 'sql' ? 'border-kipper-500 text-kipper-700 dark:text-kipper-300' : 'border-transparent text-slate-500 hover:text-slate-700 dark:hover:text-slate-300'"
            @click="activeTab = 'sql'"
          >
            SQL
          </button>
          <button
            class="px-3 py-1.5 text-sm rounded-t border-b-2"
            :class="activeTab === 'designer' ? 'border-kipper-500 text-kipper-700 dark:text-kipper-300' : 'border-transparent text-slate-500 hover:text-slate-700 dark:hover:text-slate-300'"
            @click="activeTab = 'designer'"
          >
            Designer
            <span v-if="designerOps.length" class="ml-1 px-1.5 py-0.5 rounded-full bg-amber-500/20 text-amber-700 dark:text-amber-300 text-[10px]">{{ designerOps.length }}</span>
          </button>
          <button
            class="px-3 py-1.5 text-sm rounded-t border-b-2"
            :class="activeTab === 'indexes' ? 'border-kipper-500 text-kipper-700 dark:text-kipper-300' : 'border-transparent text-slate-500 hover:text-slate-700 dark:hover:text-slate-300'"
            @click="activeTab = 'indexes'"
          >
            Indexes
          </button>
        </div>

        <!-- Browse tab -->
        <section v-if="activeTab === 'browse'" class="flex-1 flex flex-col min-h-0">
          <div v-if="!selectedRelation" class="flex-1 flex items-center justify-center text-sm text-slate-500 p-8">
            Pick a table from the sidebar to browse its rows.
          </div>
          <template v-else>
            <!-- Toolbar -->
            <div class="flex flex-wrap items-center gap-2 px-4 py-2 border-b border-slate-200 dark:border-slate-800 bg-slate-50 dark:bg-slate-950">
              <button
                class="inline-flex items-center gap-1 px-2.5 py-1 rounded text-sm border border-slate-300 dark:border-slate-700 hover:bg-slate-100 dark:hover:bg-slate-800"
                @click="startInsert"
              >
                <Plus class="w-4 h-4" /> Insert row
              </button>
              <button
                v-if="selectedRows.size > 0"
                class="inline-flex items-center gap-1 px-2.5 py-1 rounded text-sm bg-rose-600 text-white hover:bg-rose-700"
                @click="deleteSelected"
              >
                <Trash2 class="w-4 h-4" /> Delete {{ selectedRows.size }}
              </button>
              <template v-if="dirtyCount > 0">
                <button
                  class="inline-flex items-center gap-1 px-2.5 py-1 rounded text-sm bg-kipper-600 text-white hover:bg-kipper-700"
                  @click="saveEdits"
                >
                  <Check class="w-4 h-4" /> Save {{ dirtyCount }} change{{ dirtyCount === 1 ? '' : 's' }}
                </button>
                <button
                  class="inline-flex items-center gap-1 px-2.5 py-1 rounded text-sm border border-slate-300 dark:border-slate-700 hover:bg-slate-100 dark:hover:bg-slate-800"
                  @click="discardEdits"
                >
                  <X class="w-4 h-4" /> Discard
                </button>
              </template>
              <button class="inline-flex items-center gap-1 px-2 py-1 rounded text-sm hover:bg-slate-100 dark:hover:bg-slate-800" @click="loadRows">
                <RefreshCw class="w-4 h-4" :class="browseLoading ? 'animate-spin' : ''" />
              </button>

              <!-- Pagination -->
              <div class="ml-auto flex items-center gap-1 text-xs text-slate-500">
                <span v-if="filterCol" class="mr-2 px-2 py-0.5 rounded bg-amber-100 dark:bg-amber-900/30 text-amber-800 dark:text-amber-300">
                  filter: {{ filterCol }} = {{ filterVal || '(empty)' }}
                  <button class="ml-1 hover:underline" @click="clearFilter">clear</button>
                </span>
                <button :disabled="offset === 0" class="p-1 rounded hover:bg-slate-100 dark:hover:bg-slate-800 disabled:opacity-30" @click="firstPage"><ChevronsLeft class="w-4 h-4" /></button>
                <button :disabled="offset === 0" class="p-1 rounded hover:bg-slate-100 dark:hover:bg-slate-800 disabled:opacity-30" @click="prevPage"><ChevronRight class="w-4 h-4 rotate-180" /></button>
                <span class="px-1">{{ currentPage }} / {{ totalPages || 1 }}</span>
                <button :disabled="!browseData || offset + pageSize >= browseData.total" class="p-1 rounded hover:bg-slate-100 dark:hover:bg-slate-800 disabled:opacity-30" @click="nextPage"><ChevronRight class="w-4 h-4" /></button>
                <button :disabled="!browseData || offset + pageSize >= browseData.total" class="p-1 rounded hover:bg-slate-100 dark:hover:bg-slate-800 disabled:opacity-30" @click="lastPage"><ChevronsRight class="w-4 h-4" /></button>
                <span v-if="browseData" class="ml-2">
                  {{ browseData.structure.columns.length }} {{ browseData.structure.columns.length === 1 ? 'col' : 'cols' }}
                  · {{ browseData.total }} {{ browseData.total === 1 ? 'row' : 'rows' }}
                  · {{ browseData.duration_ms }}ms
                </span>
              </div>
            </div>

            <!-- Filter input (inline when active) -->
            <div v-if="filterCol" class="flex items-center gap-2 px-4 py-1 border-b border-slate-200 dark:border-slate-800 bg-amber-50 dark:bg-slate-900 dark:shadow-[inset_3px_0_0_theme(colors.orange.300)]">
              <span class="text-xs text-slate-600 dark:text-slate-400">Filter <code class="font-mono">{{ filterCol }}</code> =</span>
              <input
                v-model="filterVal"
                class="px-2 py-0.5 text-xs rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 font-mono"
                placeholder="value"
                autofocus
                @keyup.enter="loadRows"
                @keyup.escape="clearFilter"
              />
              <button
                class="text-xs px-2 py-0.5 rounded bg-kipper-600 text-white disabled:opacity-50"
                :disabled="!filterVal"
                @click="loadRows"
              >Apply</button>
              <button class="text-xs px-2 py-0.5 rounded border border-slate-300 dark:border-slate-700" @click="clearFilter">Cancel</button>
            </div>

            <!-- Insert form (inline above the grid) -->
            <div v-if="showInsertForm && browseData" class="px-4 py-3 border-b border-slate-200 dark:border-slate-800 bg-kipper-50 dark:bg-kipper-950/20">
              <div class="text-xs font-semibold text-slate-700 dark:text-slate-300 mb-2">New row</div>
              <div class="grid grid-cols-1 sm:grid-cols-2 md:grid-cols-3 gap-2">
                <div v-for="c in browseData.structure.columns" :key="c.name">
                  <label class="text-[10px] uppercase tracking-wide text-slate-500 block">
                    {{ c.name }}
                    <span class="text-slate-400 lowercase ml-1">{{ c.type }}{{ !c.nullable ? ' NOT NULL' : '' }}</span>
                  </label>
                  <input
                    v-if="!c.generated"
                    :value="insertValues[c.name] ?? ''"
                    @input="insertValues[c.name] = ($event.target as HTMLInputElement).value"
                    :placeholder="c.default ? `default: ${c.default}` : (c.nullable ? '(null)' : '')"
                    class="w-full px-2 py-1 text-xs rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 font-mono"
                  />
                  <span v-else class="text-xs text-slate-400 italic">generated</span>
                </div>
              </div>
              <div class="mt-2 flex items-center gap-2">
                <button class="px-3 py-1 rounded text-sm bg-kipper-600 text-white hover:bg-kipper-700" @click="submitInsert">Insert</button>
                <button class="px-3 py-1 rounded text-sm border border-slate-300 dark:border-slate-700" @click="showInsertForm = false">Cancel</button>
              </div>
            </div>

            <!-- Status / errors -->
            <div v-if="browseError" class="px-4 py-2 text-sm text-rose-600 bg-rose-50 dark:bg-slate-900 dark:text-slate-300 border-b border-rose-200 dark:border-slate-800 dark:shadow-[inset_3px_0_0_theme(colors.rose.400)]">
              <AlertTriangle class="inline w-4 h-4 mr-1 dark:text-rose-300" /> {{ browseError }}
            </div>
            <div v-if="browseData && browseData.structure.primary_key.length === 0" class="px-4 py-2 text-xs text-amber-700 dark:text-slate-300 bg-amber-50 dark:bg-slate-900 border-b border-amber-200 dark:border-slate-800 dark:shadow-[inset_3px_0_0_theme(colors.orange.300)]">
              This table has no primary key: inline edits and deletes are disabled. Use the SQL tab to modify rows.
            </div>

            <!-- Grid -->
            <div class="flex-1 overflow-auto">
              <table v-if="browseData" class="text-xs font-mono w-full border-collapse">
                <thead class="sticky top-0 bg-slate-100 dark:bg-slate-900 z-10">
                  <tr>
                    <th class="px-2 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 w-8">
                      <input
                        type="checkbox"
                        :checked="browseData.rows.length > 0 && selectedRows.size === browseData.rows.length"
                        @change="toggleAllSelected"
                        :disabled="browseData.structure.primary_key.length === 0"
                      />
                    </th>
                    <th
                      v-for="col in browseData.structure.columns"
                      :key="col.name"
                      class="px-3 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 font-semibold relative"
                    >
                      <button
                        class="inline-flex items-center gap-1 hover:underline"
                        @click="openHeaderMenu = openHeaderMenu === col.name ? null : col.name"
                      >
                        {{ col.name }}
                        <span class="text-[10px] text-slate-400 font-normal">{{ col.type }}</span>
                        <ArrowUp v-if="sortCol === col.name && sortDir === 'ASC'" class="w-3 h-3" />
                        <ArrowDown v-else-if="sortCol === col.name && sortDir === 'DESC'" class="w-3 h-3" />
                        <ArrowUpDown v-else class="w-3 h-3 opacity-30" />
                      </button>
                      <!-- Header menu -->
                      <div
                        v-if="openHeaderMenu === col.name"
                        class="absolute left-0 mt-1 w-44 rounded-lg border border-slate-200 dark:border-slate-700 bg-white dark:bg-slate-900 shadow-lg z-20 text-xs"
                        @click.stop
                      >
                        <button class="w-full text-left px-3 py-1.5 hover:bg-slate-100 dark:hover:bg-slate-800" @click="setSort(col.name)">Sort {{ sortCol === col.name && sortDir === 'ASC' ? 'descending' : 'ascending' }}</button>
                        <button class="w-full text-left px-3 py-1.5 hover:bg-slate-100 dark:hover:bg-slate-800" @click="applyFilter(col.name)">Filter…</button>
                        <button class="w-full text-left px-3 py-1.5 hover:bg-slate-100 dark:hover:bg-slate-800" @click="copyToClipboard(col.name)">Copy column name</button>
                      </div>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  <tr
                    v-for="(row, i) in browseData.rows"
                    :key="i"
                    class="hover:bg-slate-50 dark:hover:bg-slate-900/50"
                    :class="selectedRows.has(i) ? 'bg-kipper-50 dark:bg-kipper-900/20' : ''"
                  >
                    <td class="px-2 py-1 border-b border-slate-100 dark:border-slate-800/50">
                      <input
                        type="checkbox"
                        :checked="selectedRows.has(i)"
                        @change="toggleRowSelected(i)"
                        :disabled="browseData.structure.primary_key.length === 0"
                      />
                    </td>
                    <td
                      v-for="col in browseData.structure.columns"
                      :key="col.name"
                      class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50 align-top relative"
                      :class="isCellDirty(i, col.name) ? 'bg-amber-50 dark:bg-amber-950/30' : ''"
                      @dblclick="(browseData.structure.primary_key.length > 0 && !col.generated) && startEdit(i, col.name, cellValue(i, col.name))"
                    >
                      <template v-if="editingCell?.row === i && editingCell?.col === col.name">
                        <input
                          v-model="editBuffer"
                          class="w-full px-1 py-0.5 text-xs rounded border border-kipper-500 bg-white dark:bg-slate-800 font-mono"
                          autofocus
                          @keyup.enter="commitEdit"
                          @keyup.escape="cancelEdit"
                          @blur="commitEdit"
                        />
                      </template>
                      <template v-else>
                        <span v-if="cellValue(i, col.name) === null" class="text-slate-400 italic">NULL</span>
                        <template v-else-if="fkFor(col.name)">
                          <button
                            class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded bg-kipper-50 dark:bg-kipper-900/30 text-kipper-700 dark:text-kipper-300 hover:underline"
                            :title="`→ ${fkFor(col.name)?.ref_schema}.${fkFor(col.name)?.ref_table}.${fkFor(col.name)?.ref_column}`"
                            @click.stop="navigateToFK(fkFor(col.name)!)"
                          >
                            → {{ displayCell(cellValue(i, col.name)) }}
                          </button>
                        </template>
                        <span v-else>{{ displayCell(cellValue(i, col.name)) }}</span>
                      </template>
                    </td>
                  </tr>
                  <tr v-if="!browseData.rows.length">
                    <td :colspan="browseData.structure.columns.length + 1" class="px-4 py-8 text-center text-slate-500">
                      No rows in this table.
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </template>
        </section>

        <!-- Designer tab (G3) -->
        <section v-if="activeTab === 'designer'" class="flex-1 flex flex-col min-h-0">
          <!-- Create-table form (G3.5b) -->
          <template v-if="creatingTable">
            <div class="flex flex-wrap items-center gap-2 px-4 py-2 border-b border-slate-200 dark:border-slate-800 bg-kipper-50 dark:bg-kipper-950/20">
              <Plus class="w-4 h-4 text-kipper-600" />
              <span class="text-sm font-medium">New table</span>
              <button
                class="ml-auto inline-flex items-center gap-1 px-3 py-1 rounded text-sm bg-kipper-600 text-white hover:bg-kipper-700"
                :disabled="!newTableName || newTableCols.length === 0"
                @click="applyCreateTable"
              >
                <Check class="w-4 h-4" /> Create table
              </button>
              <button class="inline-flex items-center gap-1 px-3 py-1 rounded text-sm border border-slate-300 dark:border-slate-700" @click="cancelCreateTable">
                <X class="w-4 h-4" /> Cancel
              </button>
            </div>

            <div class="px-4 py-3 border-b border-slate-200 dark:border-slate-800 grid grid-cols-1 md:grid-cols-3 gap-3">
              <div>
                <label class="text-[10px] uppercase tracking-wide text-slate-500 block mb-1">Schema</label>
                <input v-model="newTableSchema" placeholder="public" class="w-full px-2 py-1 text-sm rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 font-mono" />
              </div>
              <div class="md:col-span-2">
                <label class="text-[10px] uppercase tracking-wide text-slate-500 block mb-1">Name</label>
                <input v-model="newTableName" placeholder="domains" class="w-full px-2 py-1 text-sm rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 font-mono" autofocus />
              </div>
            </div>

            <div class="flex-1 overflow-auto">
              <table class="text-xs font-mono w-full border-collapse">
                <thead class="sticky top-0 bg-slate-100 dark:bg-slate-900 z-10">
                  <tr>
                    <th class="px-3 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 font-semibold w-1/4">Name</th>
                    <th class="px-3 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 font-semibold w-1/4">Type</th>
                    <th class="px-3 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 font-semibold w-24">Nullable</th>
                    <th class="px-3 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 font-semibold">Default</th>
                    <th class="px-3 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 font-semibold w-16">PK</th>
                    <th class="px-3 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 font-semibold w-12"></th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="(c, i) in newTableCols" :key="i" class="hover:bg-slate-50 dark:hover:bg-slate-900/50">
                    <td class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50">
                      <input
                        v-model="c.name"
                        placeholder="column_name"
                        class="w-full px-2 py-0.5 text-xs rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 font-mono"
                      />
                    </td>
                    <td class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50">
                      <div class="flex gap-1">
                        <select
                          :value="isCustomType(c.type) ? CUSTOM_SENTINEL : (c.type || 'text')"
                          class="flex-1 px-2 py-0.5 text-xs rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 font-mono"
                          @change="c.type = typeSelectValue($event)"
                        >
                          <optgroup v-for="g in TYPE_GROUPS" :key="g.label" :label="g.label">
                            <option v-for="t in g.types" :key="t" :value="t">{{ t }}</option>
                          </optgroup>
                          <option :value="CUSTOM_SENTINEL">Custom…</option>
                        </select>
                        <input
                          v-if="isCustomType(c.type) || c.type === ''"
                          v-model="c.type"
                          placeholder="varchar(50), numeric(10,2)…"
                          class="flex-1 px-2 py-0.5 text-xs rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 font-mono"
                        />
                      </div>
                    </td>
                    <td class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50">
                      <input v-model="c.nullable" type="checkbox" class="accent-kipper-600" />
                    </td>
                    <td class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50">
                      <input
                        :value="c.default ?? ''"
                        @input="c.default = ($event.target as HTMLInputElement).value || null"
                        placeholder="now() / 'pending' / 0"
                        class="w-full px-2 py-0.5 text-xs rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 font-mono"
                      />
                    </td>
                    <td class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50">
                      <input
                        type="checkbox"
                        :checked="newTablePK.includes(c.name)"
                        :disabled="!c.name"
                        class="accent-kipper-600"
                        @change="toggleNewTablePK(c.name)"
                      />
                    </td>
                    <td class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50">
                      <button class="p-1 text-rose-600 hover:bg-rose-50 dark:hover:bg-rose-950 rounded" @click="removeNewTableCol(i)" title="Remove">
                        <Trash2 class="w-3.5 h-3.5" />
                      </button>
                    </td>
                  </tr>
                  <tr>
                    <td colspan="6" class="px-3 py-2 border-b border-slate-100 dark:border-slate-800/50">
                      <button class="inline-flex items-center gap-1 text-xs text-kipper-600 hover:underline" @click="addNewTableCol">
                        <Plus class="w-3.5 h-3.5" /> Add column
                      </button>
                    </td>
                  </tr>
                </tbody>
              </table>
              <datalist id="new-table-col-types">
                <option v-for="t in dialectTypeHints()" :key="t" :value="t" />
              </datalist>
            </div>

            <div v-if="newTablePreview.length > 0" class="border-t border-slate-200 dark:border-slate-800">
              <div class="px-4 py-1.5 text-[10px] uppercase tracking-wide text-slate-500 bg-slate-50 dark:bg-slate-950">DDL preview</div>
              <pre class="text-xs font-mono p-3 bg-slate-950 text-slate-100 overflow-auto max-h-48">{{ newTablePreview.join(';\n') + ';' }}</pre>
            </div>
          </template>

          <div v-else-if="!selectedRelation || !browseData" class="flex-1 flex items-center justify-center text-sm text-slate-500 p-8">
            <div class="text-center">
              <p>Pick a table from the sidebar to design its schema.</p>
              <button class="mt-3 inline-flex items-center gap-1 px-3 py-1.5 rounded text-sm bg-kipper-600 text-white hover:bg-kipper-700" @click="startCreateTable">
                <Plus class="w-4 h-4" /> New table
              </button>
            </div>
          </div>
          <template v-else>
            <div class="flex flex-wrap items-center gap-2 px-4 py-2 border-b border-slate-200 dark:border-slate-800 bg-slate-50 dark:bg-slate-950">
              <span class="text-sm font-medium">{{ selectedRelation.schema }}.{{ selectedRelation.name }}</span>
              <button
                class="ml-auto inline-flex items-center gap-1 px-2.5 py-1 rounded text-sm border border-slate-300 dark:border-slate-700 hover:bg-slate-100 dark:hover:bg-slate-800"
                @click="showAddColumn = !showAddColumn"
              >
                <Plus class="w-4 h-4" /> Add column
              </button>
              <label v-if="designerOps.length > 0" class="inline-flex items-center gap-1 text-xs text-slate-600 dark:text-slate-400">
                <input v-model="runWithBackup" type="checkbox" class="accent-kipper-600" />
                Take a backup first
              </label>
              <button
                v-if="designerOps.length > 0"
                class="inline-flex items-center gap-1 px-3 py-1 rounded text-sm bg-kipper-600 text-white hover:bg-kipper-700 disabled:opacity-50"
                :disabled="designerApplying"
                @click="applyDesigner"
              >
                <Loader2 v-if="designerApplying" class="w-4 h-4 animate-spin" />
                <Check v-else class="w-4 h-4" />
                Apply {{ designerOps.length }} change{{ designerOps.length === 1 ? '' : 's' }}
              </button>
              <button
                v-if="designerOps.length > 0"
                class="inline-flex items-center gap-1 px-2.5 py-1 rounded text-sm border border-slate-300 dark:border-slate-700 hover:bg-slate-100 dark:hover:bg-slate-800"
                @click="designerOps = []; designerPreview = []"
              >
                <X class="w-4 h-4" /> Discard
              </button>
            </div>

            <!-- Add-column form -->
            <div v-if="showAddColumn" class="px-4 py-3 border-b border-slate-200 dark:border-slate-800 bg-kipper-50 dark:bg-kipper-950/20">
              <div class="text-xs font-semibold text-slate-700 dark:text-slate-300 mb-2">New column</div>
              <div class="grid grid-cols-1 md:grid-cols-4 gap-2 items-end">
                <div>
                  <label class="text-[10px] uppercase tracking-wide text-slate-500 block">Name</label>
                  <input v-model="newCol.name" placeholder="last_synced_at" class="w-full px-2 py-1 text-xs rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 font-mono" />
                </div>
                <div>
                  <label class="text-[10px] uppercase tracking-wide text-slate-500 block">Type</label>
                  <div class="flex gap-1">
                    <select
                      :value="isCustomType(newCol.type) ? CUSTOM_SENTINEL : (newCol.type || 'text')"
                      class="flex-1 px-2 py-1 text-xs rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 font-mono"
                      @change="newCol.type = typeSelectValue($event)"
                    >
                      <optgroup v-for="g in TYPE_GROUPS" :key="g.label" :label="g.label">
                        <option v-for="t in g.types" :key="t" :value="t">{{ t }}</option>
                      </optgroup>
                      <option :value="CUSTOM_SENTINEL">Custom…</option>
                    </select>
                    <input
                      v-if="isCustomType(newCol.type) || newCol.type === ''"
                      v-model="newCol.type"
                      placeholder="varchar(50)"
                      class="flex-1 px-2 py-1 text-xs rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 font-mono"
                    />
                  </div>
                </div>
                <div>
                  <label class="text-[10px] uppercase tracking-wide text-slate-500 block">Default</label>
                  <input
                    :value="newCol.default ?? ''"
                    @input="newCol.default = ($event.target as HTMLInputElement).value || null"
                    placeholder="now() / 'pending' / 0"
                    class="w-full px-2 py-1 text-xs rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 font-mono"
                  />
                </div>
                <div class="flex items-center gap-2">
                  <label class="inline-flex items-center gap-1 text-xs">
                    <input v-model="newCol.nullable" type="checkbox" class="accent-kipper-600" /> Nullable
                  </label>
                </div>
              </div>
              <div class="mt-2 flex gap-2">
                <button class="px-3 py-1 rounded text-sm bg-kipper-600 text-white hover:bg-kipper-700" @click="queueAddColumn">Queue</button>
                <button class="px-3 py-1 rounded text-sm border border-slate-300 dark:border-slate-700" @click="showAddColumn = false">Cancel</button>
              </div>
            </div>

            <!-- Pending ops list -->
            <div v-if="designerOps.length > 0" class="px-4 py-2 border-b border-slate-200 dark:border-slate-800 bg-amber-50 dark:bg-slate-900 dark:shadow-[inset_3px_0_0_theme(colors.orange.300)]">
              <div class="text-xs font-semibold text-amber-800 dark:text-orange-300 mb-1">Pending changes</div>
              <ul class="space-y-1">
                <li v-for="(op, i) in designerOps" :key="i" class="flex items-center gap-2 text-xs">
                  <code class="font-mono text-slate-700 dark:text-slate-300">{{ op.op }}</code>
                  <span v-if="op.column?.name" class="text-slate-600">{{ op.column.name }}</span>
                  <span v-if="op.name" class="text-slate-600">{{ op.name }}</span>
                  <span v-if="op.old_name" class="text-slate-600">{{ op.old_name }} → {{ op.new_name }}</span>
                  <button class="ml-auto text-rose-600 hover:underline" @click="removeOp(i)">Remove</button>
                </li>
              </ul>
            </div>

            <!-- Column list -->
            <div class="flex-1 overflow-auto">
              <table class="text-xs font-mono w-full border-collapse">
                <thead class="sticky top-0 bg-slate-100 dark:bg-slate-900 z-10">
                  <tr>
                    <th class="px-3 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 font-semibold">Name</th>
                    <th class="px-3 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 font-semibold">Type</th>
                    <th class="px-3 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 font-semibold">Nullable</th>
                    <th class="px-3 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 font-semibold">Default</th>
                    <th class="px-3 py-1.5 text-right border-b border-slate-200 dark:border-slate-800 font-semibold whitespace-nowrap">Actions</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="col in browseData.structure.columns" :key="col.name" class="hover:bg-slate-50 dark:hover:bg-slate-900/50">
                    <td class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50">
                      <template v-if="editingRenameFor === col.name">
                        <div class="flex items-center gap-1">
                          <input
                            v-model="editingRenameValue"
                            class="px-2 py-0.5 text-xs rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 font-mono"
                            autofocus
                            @keyup.enter="commitRenameEdit"
                            @keyup.escape="cancelRenameEdit"
                          />
                          <button
                            class="px-1.5 py-0.5 rounded text-xs bg-kipper-600 text-white hover:bg-kipper-700 disabled:opacity-50"
                            :disabled="!editingRenameValue.trim() || editingRenameValue.trim() === col.name"
                            @click="commitRenameEdit"
                          >Apply</button>
                          <button class="px-1.5 py-0.5 rounded text-xs border border-slate-300 dark:border-slate-700" @click="cancelRenameEdit">Cancel</button>
                        </div>
                      </template>
                      <template v-else>
                        <span :class="browseData.structure.primary_key.includes(col.name) ? 'font-bold' : ''">{{ col.name }}</span>
                        <span v-if="browseData.structure.primary_key.includes(col.name)" class="ml-1 text-[10px] text-amber-600">PK</span>
                        <span v-if="col.generated" class="ml-1 text-[10px] text-slate-400">generated</span>
                      </template>
                    </td>
                    <td class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50">
                      <template v-if="editingTypeFor === col.name">
                        <div class="flex items-center gap-1">
                          <select
                            :value="isCustomType(editingTypeValue) ? CUSTOM_SENTINEL : editingTypeValue"
                            class="px-2 py-0.5 text-xs rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 font-mono"
                            @change="editingTypeValue = typeSelectValue($event)"
                          >
                            <optgroup v-for="g in TYPE_GROUPS" :key="g.label" :label="g.label">
                              <option v-for="t in g.types" :key="t" :value="t">{{ t }}</option>
                            </optgroup>
                            <option :value="CUSTOM_SENTINEL">Custom…</option>
                          </select>
                          <input
                            v-if="isCustomType(editingTypeValue) || editingTypeValue === ''"
                            v-model="editingTypeValue"
                            placeholder="varchar(50)"
                            class="px-2 py-0.5 text-xs rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 font-mono w-32"
                          />
                          <button
                            class="px-1.5 py-0.5 rounded text-xs bg-kipper-600 text-white hover:bg-kipper-700 disabled:opacity-50"
                            :disabled="!editingTypeValue.trim() || editingTypeValue.trim() === col.type"
                            @click="commitTypeEdit"
                          >Apply</button>
                          <button class="px-1.5 py-0.5 rounded text-xs border border-slate-300 dark:border-slate-700" @click="cancelTypeEdit">Cancel</button>
                        </div>
                      </template>
                      <span v-else class="text-slate-500">{{ col.type }}</span>
                    </td>
                    <td class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50">
                      <span v-if="col.nullable" class="text-slate-500">YES</span>
                      <span v-else class="text-slate-700 dark:text-slate-300">NO</span>
                    </td>
                    <td class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50 text-slate-500">{{ col.default || '—' }}</td>
                    <td class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50 whitespace-nowrap text-right">
                      <button class="text-kipper-600 hover:underline mr-3" @click="startRenameEdit(col)">Rename</button>
                      <button class="text-kipper-600 hover:underline mr-3" @click="startTypeEdit(col)">Type…</button>
                      <button class="text-kipper-600 hover:underline mr-3" @click="queueToggleNullable(col)">Toggle null</button>
                      <button class="text-rose-600 hover:underline" @click="queueDropColumn(col.name)">Drop</button>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>

            <!-- DDL preview -->
            <div v-if="designerPreview.length > 0" class="border-t border-slate-200 dark:border-slate-800">
              <div class="px-4 py-1.5 text-[10px] uppercase tracking-wide text-slate-500 bg-slate-50 dark:bg-slate-950">DDL preview (one transaction)</div>
              <pre class="text-xs font-mono p-3 bg-slate-950 text-slate-100 overflow-auto max-h-48">{{ designerPreview.join(';\n') + ';' }}</pre>
            </div>
          </template>
        </section>

        <!-- Indexes tab (G3) -->
        <section v-if="activeTab === 'indexes'" class="flex-1 flex flex-col min-h-0">
          <div v-if="!selectedRelation || !browseData" class="flex-1 flex items-center justify-center text-sm text-slate-500 p-8">
            Pick a table from the sidebar to manage its indexes.
          </div>
          <template v-else>
            <div class="flex flex-wrap items-center gap-2 px-4 py-2 border-b border-slate-200 dark:border-slate-800 bg-slate-50 dark:bg-slate-950">
              <span class="text-sm font-medium">{{ selectedRelation.schema }}.{{ selectedRelation.name }}</span>
              <button
                class="ml-auto inline-flex items-center gap-1 px-2.5 py-1 rounded text-sm border border-slate-300 dark:border-slate-700 hover:bg-slate-100 dark:hover:bg-slate-800"
                @click="showAddIndex = !showAddIndex"
              >
                <Plus class="w-4 h-4" /> Create index
              </button>
            </div>

            <!-- Create index form -->
            <div v-if="showAddIndex" class="px-4 py-3 border-b border-slate-200 dark:border-slate-800 bg-kipper-50 dark:bg-kipper-950/20">
              <div class="text-xs font-semibold text-slate-700 dark:text-slate-300 mb-2">New index</div>
              <div class="grid grid-cols-1 md:grid-cols-2 gap-3 items-end">
                <div>
                  <label class="text-[10px] uppercase tracking-wide text-slate-500 block">Name</label>
                  <input
                    v-model="newIndex.name"
                    :placeholder="suggestedIndexName() || 'users_email_idx'"
                    class="w-full px-2 py-1 text-xs rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 font-mono"
                  />
                  <p v-if="!newIndex.name && suggestedIndexName()" class="mt-0.5 text-[10px] text-slate-500">
                    Leave blank to use <code class="font-mono">{{ suggestedIndexName() }}</code>.
                  </p>
                </div>
                <div>
                  <label class="text-[10px] uppercase tracking-wide text-slate-500 block">Method</label>
                  <select v-model="newIndex.method" class="w-full px-2 py-1 text-xs rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900">
                    <option value="">default (btree)</option>
                    <option value="btree">btree</option>
                    <option value="gin">gin</option>
                    <option value="gist">gist</option>
                    <option value="hash">hash</option>
                  </select>
                </div>
                <div class="md:col-span-2">
                  <label class="text-[10px] uppercase tracking-wide text-slate-500 block mb-1">Columns (in index order)</label>
                  <div class="flex flex-wrap items-center gap-1 min-h-[28px]">
                    <span
                      v-for="(col, i) in newIndex.columns"
                      :key="col"
                      class="inline-flex items-center gap-1 px-2 py-0.5 rounded text-xs font-mono bg-kipper-100 dark:bg-kipper-900/40 text-kipper-800 dark:text-kipper-200"
                    >
                      <span class="text-kipper-500/70">{{ i + 1 }}.</span>
                      {{ col }}
                      <button
                        class="px-0.5 hover:bg-kipper-200 dark:hover:bg-kipper-800 rounded disabled:opacity-30"
                        :disabled="i === 0"
                        title="Move earlier in the index"
                        @click="moveIndexColumn(i, -1)"
                      >↑</button>
                      <button
                        class="px-0.5 hover:bg-kipper-200 dark:hover:bg-kipper-800 rounded disabled:opacity-30"
                        :disabled="i === newIndex.columns.length - 1"
                        title="Move later in the index"
                        @click="moveIndexColumn(i, 1)"
                      >↓</button>
                      <button
                        class="px-0.5 hover:bg-rose-100 dark:hover:bg-rose-900/40 rounded text-rose-600"
                        title="Remove from index"
                        @click="removeIndexColumn(col)"
                      >×</button>
                    </span>
                    <select
                      v-if="availableIndexColumns.length"
                      class="px-2 py-0.5 text-xs rounded border border-dashed border-slate-300 dark:border-slate-700 bg-transparent font-mono"
                      :value="''"
                      @change="addIndexColumnFromSelect($event)"
                    >
                      <option value="">+ Add column</option>
                      <option v-for="c in availableIndexColumns" :key="c.name" :value="c.name">{{ c.name }} ({{ c.type }})</option>
                    </select>
                    <span v-else-if="!newIndex.columns.length" class="text-xs text-slate-400">No columns available. Open a table first.</span>
                  </div>
                  <p v-if="newIndex.columns.length > 1" class="mt-1 text-[10px] text-slate-500">
                    Order matters: Postgres uses the leftmost prefix of a multi-column index for query planning.
                  </p>
                </div>
                <div class="md:col-span-2">
                  <label class="text-[10px] uppercase tracking-wide text-slate-500 block">WHERE clause (partial index, Postgres only)</label>
                  <input v-model="newIndex.where" placeholder="status = 'active'" class="w-full px-2 py-1 text-xs rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900 font-mono" />
                </div>
                <div class="flex items-center gap-3">
                  <label class="inline-flex items-center gap-1 text-xs">
                    <input v-model="newIndex.unique" type="checkbox" class="accent-kipper-600" /> Unique
                  </label>
                  <label class="inline-flex items-center gap-1 text-xs">
                    <input v-model="newIndex.concurrent" type="checkbox" class="accent-kipper-600" /> Concurrent
                  </label>
                </div>
              </div>
              <div class="mt-2 flex gap-2">
                <button class="px-3 py-1 rounded text-sm bg-kipper-600 text-white hover:bg-kipper-700" @click="applyCreateIndex">Create</button>
                <button class="px-3 py-1 rounded text-sm border border-slate-300 dark:border-slate-700" @click="showAddIndex = false">Cancel</button>
              </div>
              <div v-if="indexPreview.length > 0" class="mt-2">
                <div class="text-[10px] uppercase tracking-wide text-slate-500 mb-1">DDL preview</div>
                <pre class="text-xs font-mono p-2 bg-slate-950 text-slate-100 rounded">{{ indexPreview.join(';\n') + ';' }}</pre>
              </div>
            </div>

            <!-- Index list -->
            <div class="flex-1 overflow-auto">
              <table class="text-xs font-mono w-full border-collapse">
                <thead class="sticky top-0 bg-slate-100 dark:bg-slate-900 z-10">
                  <tr>
                    <th class="px-3 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 font-semibold">Name</th>
                    <th class="px-3 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 font-semibold">Columns</th>
                    <th class="px-3 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 font-semibold">Method</th>
                    <th class="px-3 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 font-semibold w-24">Unique</th>
                    <th class="px-3 py-1.5 text-right border-b border-slate-200 dark:border-slate-800 font-semibold whitespace-nowrap">Actions</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="idx in browseData.structure.indexes" :key="idx.name" class="hover:bg-slate-50 dark:hover:bg-slate-900/50">
                    <td class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50">
                      {{ idx.name }}
                      <span v-if="idx.primary" class="ml-1 text-[10px] text-amber-600">PRIMARY</span>
                    </td>
                    <td class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50">{{ idx.columns.join(', ') }}</td>
                    <td class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50 text-slate-500">{{ idx.method || '—' }}</td>
                    <td class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50">
                      <span v-if="idx.unique" class="text-slate-700 dark:text-slate-300">YES</span>
                      <span v-else class="text-slate-500">NO</span>
                    </td>
                    <td class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50 whitespace-nowrap text-right">
                      <button v-if="!idx.primary" class="text-rose-600 hover:underline" @click="dropIndexByName(idx)">Drop</button>
                      <span v-else class="text-slate-400">—</span>
                    </td>
                  </tr>
                  <tr v-if="!browseData.structure.indexes.length">
                    <td colspan="5" class="px-4 py-8 text-center text-slate-500">No indexes on this table.</td>
                  </tr>
                </tbody>
              </table>
            </div>
          </template>
        </section>

        <!-- SQL tab (G1 + G5 sidebar) -->
        <section v-else-if="activeTab === 'sql'" class="flex-1 flex min-h-0">
          <!-- Left rail: snippets / history. Narrower than the schema
               sidebar so the editor still gets the lion's share. -->
          <aside class="w-52 shrink-0 border-r border-slate-200 dark:border-slate-800 bg-slate-50 dark:bg-slate-950 flex flex-col min-h-0">
            <div class="flex border-b border-slate-200 dark:border-slate-800 text-xs">
              <button
                class="flex-1 px-3 py-2 inline-flex items-center justify-center gap-1"
                :class="sidebarMode === 'snippets' ? 'bg-white dark:bg-slate-900 text-kipper-600 dark:text-kipper-400 font-medium' : 'text-slate-500 hover:text-slate-700 dark:hover:text-slate-300'"
                @click="sidebarMode = 'snippets'"
              >
                <BookmarkPlus class="w-3.5 h-3.5" /> Snippets
              </button>
              <button
                class="flex-1 px-3 py-2 inline-flex items-center justify-center gap-1"
                :class="sidebarMode === 'history' ? 'bg-white dark:bg-slate-900 text-kipper-600 dark:text-kipper-400 font-medium' : 'text-slate-500 hover:text-slate-700 dark:hover:text-slate-300'"
                @click="sidebarMode = 'history'; loadHistory()"
              >
                <HistoryIcon class="w-3.5 h-3.5" /> History
              </button>
            </div>

            <!-- Snippets list -->
            <div v-if="sidebarMode === 'snippets'" class="flex-1 overflow-y-auto">
              <div v-if="!snippets.length" class="px-3 py-6 text-center text-xs text-slate-500">
                No saved snippets.
                <p class="mt-1">Save the current query with the <BookmarkPlus class="inline w-3 h-3" /> button on the toolbar.</p>
              </div>
              <ul v-else class="text-xs">
                <li
                  v-for="s in snippets"
                  :key="s.name"
                  class="group border-b border-slate-200 dark:border-slate-800/50"
                >
                  <div class="px-3 py-2 hover:bg-slate-100 dark:hover:bg-slate-800/60">
                    <div class="flex items-center gap-1">
                      <Pin v-if="s.pinned" class="w-3 h-3 text-amber-500" />
                      <button class="font-medium truncate text-left" :title="s.sql" @click="loadSnippet(s)">{{ s.name }}</button>
                      <span class="ml-auto md:opacity-0 group-hover:opacity-100 flex items-center gap-1">
                        <button class="p-0.5 hover:text-kipper-600" :title="s.pinned ? 'Unpin' : 'Pin'" @click.stop="pinSnippet(s)">
                          <Pin class="w-3 h-3" :class="s.pinned ? 'text-amber-500' : ''" />
                        </button>
                        <button class="p-0.5 hover:text-rose-600" title="Delete" @click.stop="removeSnippet(s)">
                          <Trash2 class="w-3 h-3" />
                        </button>
                      </span>
                    </div>
                    <p class="mt-0.5 text-[10px] text-slate-500 truncate font-mono">{{ s.sql }}</p>
                  </div>
                </li>
              </ul>
            </div>

            <!-- History list -->
            <div v-else class="flex-1 overflow-y-auto">
              <div v-if="!history.length" class="px-3 py-6 text-center text-xs text-slate-500">
                No queries yet.
              </div>
              <ul v-else class="text-xs">
                <li
                  v-for="(h, i) in history"
                  :key="i"
                  class="border-b border-slate-200 dark:border-slate-800/50 hover:bg-slate-100 dark:hover:bg-slate-800/60"
                >
                  <button class="w-full text-left px-3 py-2" @click="loadHistoryEntry(h)" :title="h.sql">
                    <div class="flex items-center gap-1">
                      <span v-if="h.error" class="w-1.5 h-1.5 rounded-full bg-rose-500 shrink-0" />
                      <span v-else class="w-1.5 h-1.5 rounded-full bg-emerald-500 shrink-0" />
                      <span class="text-[10px] text-slate-500 shrink-0">{{ relativeTime(h.timestamp) }}</span>
                      <span class="text-[10px] text-slate-400 ml-auto">{{ h.duration_ms }}ms</span>
                    </div>
                    <p class="mt-0.5 text-[11px] truncate font-mono">{{ h.sql }}</p>
                    <p v-if="h.error" class="text-[10px] text-rose-500 truncate font-mono">{{ h.error }}</p>
                  </button>
                </li>
              </ul>
            </div>
          </aside>

          <!-- Editor + results -->
          <div class="flex-1 flex flex-col min-w-0">
          <div class="flex flex-wrap items-center gap-2 px-4 py-2 border-b border-slate-200 dark:border-slate-800 bg-slate-50 dark:bg-slate-950">
            <button
              class="inline-flex items-center gap-1 px-3 py-1.5 rounded bg-kipper-600 text-white text-sm hover:bg-kipper-700 disabled:opacity-50 whitespace-nowrap"
              :disabled="running || !sqlText.trim()"
              @click="runQuery"
              title="Run the statement at the cursor (or selection). Cmd/Ctrl+Enter"
            >
              <Loader2 v-if="running" class="w-4 h-4 animate-spin" />
              <Play v-else class="w-4 h-4" />
              Run
            </button>
            <button
              class="inline-flex items-center gap-1 px-2 py-1.5 rounded text-sm border border-slate-300 dark:border-slate-700 hover:bg-slate-100 dark:hover:bg-slate-800 disabled:opacity-50 whitespace-nowrap"
              :disabled="running || !sqlText.trim()"
              @click="runQueryAll"
              title="Run every statement in the editor. Cmd/Ctrl+Shift+Enter"
            >
              <Play class="w-4 h-4" /> Run all
            </button>
            <button
              class="inline-flex items-center gap-1 px-2 py-1.5 rounded text-sm border border-slate-300 dark:border-slate-700 hover:bg-slate-100 dark:hover:bg-slate-800 whitespace-nowrap"
              @click="formatSQL"
            >
              <Wand2 class="w-4 h-4" /> Format
            </button>
            <button
              class="inline-flex items-center gap-1 px-2 py-1.5 rounded text-sm border border-slate-300 dark:border-slate-700 hover:bg-slate-100 dark:hover:bg-slate-800 whitespace-nowrap"
              :disabled="!sqlText.trim()"
              @click="showSaveSnippet = true; newSnippetName = ''; newSnippetPinned = false"
              title="Save the current query as a reusable snippet"
            >
              <BookmarkPlus class="w-4 h-4" /> Save
            </button>
            <button
              class="inline-flex items-center gap-1 px-2 py-1.5 rounded text-sm border border-slate-300 dark:border-slate-700 hover:bg-slate-100 dark:hover:bg-slate-800 whitespace-nowrap"
              :disabled="!sqlText.trim()"
              @click="explainQuery"
              title="Ask the AI to explain the current query"
            >
              <Sparkles class="w-4 h-4" /> Explain
            </button>
            <label class="inline-flex items-center gap-1 text-xs text-slate-600 dark:text-slate-400 ml-2 whitespace-nowrap">
              <input v-model="runAsTransaction" type="checkbox" class="accent-kipper-600" />
              Run as transaction
            </label>
            <label class="inline-flex items-center gap-1 text-xs text-slate-600 dark:text-slate-400 whitespace-nowrap">
              <input v-model="noLimit" type="checkbox" class="accent-kipper-600" />
              No auto-limit
            </label>
            <span class="ml-auto text-xs text-slate-500 whitespace-nowrap" title="Run statement at cursor: ⌘/Ctrl + Enter: Run all: ⌘/Ctrl + Shift + Enter">⌘/Ctrl + Enter</span>
          </div>

          <!-- Save snippet inline form -->
          <div v-if="showSaveSnippet" class="flex items-center gap-2 px-4 py-2 border-b border-slate-200 dark:border-slate-800 bg-kipper-50 dark:bg-kipper-950/20">
            <Star class="w-4 h-4 text-kipper-600" />
            <input
              v-model="newSnippetName"
              placeholder="snippet name"
              class="flex-1 max-w-sm px-2 py-1 text-sm rounded border border-slate-300 dark:border-slate-700 bg-white dark:bg-slate-900"
              @keyup.enter="saveCurrentAsSnippet"
            />
            <label class="inline-flex items-center gap-1 text-xs">
              <input v-model="newSnippetPinned" type="checkbox" class="accent-kipper-600" /> Pin
            </label>
            <button class="px-3 py-1 rounded text-sm bg-kipper-600 text-white hover:bg-kipper-700" @click="saveCurrentAsSnippet">Save</button>
            <button class="px-3 py-1 rounded text-sm border border-slate-300 dark:border-slate-700" @click="showSaveSnippet = false">Cancel</button>
          </div>

          <!-- Editor takes 40% of available height; results take the rest. Both
               flex children share the remaining space so the result grid fills
               the viewport whatever its width. -->
          <div class="basis-2/5 shrink-0 min-h-[180px] overflow-hidden">
            <Codemirror
              v-model="sqlText"
              :extensions="editorExtensions"
              class="h-full"
              :style="{ height: '100%' }"
              placeholder="SELECT * FROM users LIMIT 10;&#10;&#10;-- Multiple statements? Cmd/Ctrl+Enter runs the one at the cursor."
              @ready="onEditorReady"
            />
          </div>

          <div class="flex-1 min-h-0 border-t border-slate-200 dark:border-slate-800 flex flex-col">
            <div class="flex items-center gap-3 px-4 py-2 text-xs text-slate-500 bg-slate-50 dark:bg-slate-950 border-b border-slate-200 dark:border-slate-800">
              <span v-if="!result">Run a query to see results.</span>
              <template v-else-if="result.error">
                <AlertTriangle class="w-4 h-4 text-rose-500" />
                <code class="text-rose-600 dark:text-rose-400 font-mono">{{ result.error }}</code>
                <span class="ml-auto">{{ result.duration_ms }} ms</span>
              </template>
              <template v-else>
                <span v-if="result.rows">{{ result.rows.length }} {{ result.rows.length === 1 ? 'row' : 'rows' }}</span>
                <span v-else>{{ result.rows_affected }} rows affected</span>
                <span v-if="result.truncated" class="text-amber-600">truncated, increase the limit to see more</span>
                <span class="ml-auto">{{ result.duration_ms }} ms</span>
                <button
                  v-if="result.rows && result.rows.length"
                  class="inline-flex items-center gap-1 px-2 py-1 rounded border border-slate-300 dark:border-slate-700 hover:bg-slate-100 dark:hover:bg-slate-800"
                  @click="exportCSV"
                >
                  <Download class="w-3 h-3" /> Export CSV
                </button>
              </template>
            </div>

            <div class="flex-1 overflow-auto">
              <table v-if="result?.columns && result.rows" class="text-xs font-mono w-full border-collapse">
                <thead class="sticky top-0 bg-slate-100 dark:bg-slate-900 z-10">
                  <tr>
                    <th class="px-3 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 w-12 text-slate-400">#</th>
                    <th
                      v-for="col in result.columns"
                      :key="col.name"
                      class="px-3 py-1.5 text-left border-b border-slate-200 dark:border-slate-800 font-semibold"
                    >
                      {{ col.name }}
                      <span class="text-[10px] text-slate-400 font-normal">{{ col.type }}</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="(row, i) in result.rows" :key="i" class="hover:bg-slate-50 dark:hover:bg-slate-900">
                    <td class="px-3 py-1 text-slate-400 border-b border-slate-100 dark:border-slate-800/50">{{ i + 1 }}</td>
                    <td
                      v-for="(val, j) in row"
                      :key="j"
                      class="px-3 py-1 border-b border-slate-100 dark:border-slate-800/50 align-top"
                    >
                      <span v-if="val === null" class="text-slate-400 italic">NULL</span>
                      <span v-else>{{ typeof val === 'object' ? JSON.stringify(val) : String(val) }}</span>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </div>
          </div>
        </section>
      </main>
      </div>

      <!-- AI side panel — narrower default so the editor stays roomy.
           Caps at 24rem so it doesn't take more than a quarter of a
           1920-wide viewport. -->
      <aside
        v-if="showAI"
        class="w-full h-1/2 md:h-auto md:w-80 lg:w-96 shrink-0 border-t md:border-t-0 md:border-l border-slate-200 dark:border-slate-800 flex flex-col"
      >
        <div class="px-3 py-2 border-b border-slate-200 dark:border-slate-800 bg-slate-50 dark:bg-slate-950 text-xs flex items-center justify-between">
          <span class="font-semibold uppercase tracking-wide text-slate-500">Kipper knows</span>
          <button class="p-0.5 rounded hover:bg-slate-200 dark:hover:bg-slate-800" @click="showAI = false">
            <X class="w-3.5 h-3.5" />
          </button>
        </div>
        <div class="px-3 py-2 text-[11px] text-slate-500 border-b border-slate-200 dark:border-slate-800 max-h-32 overflow-y-auto">
          <div>Dialect: <code class="font-mono">{{ aiSchemaContext.dialect }}</code></div>
          <div v-if="selectedRelation">
            Open table: <code class="font-mono">{{ selectedRelation.schema }}.{{ selectedRelation.name }}</code>
            <span v-if="browseData">({{ browseData.structure.columns.length }} cols, {{ browseData.structure.indexes.length }} idx)</span>
          </div>
          <div>{{ aiSchemaContext.tables.length }} tables visible</div>
        </div>
        <div class="flex-1 min-h-0">
          <AIChat
            ref="aiChatRef"
            mode="db"
            code=""
            runtime="sql"
            :db-schema="aiSchemaContext"
            :sql="sqlText"
            @apply="onAIApply"
          />
        </div>
      </aside>
    </div>
  </div>
</template>
