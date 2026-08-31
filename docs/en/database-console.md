---
title: 'Browse and query your database in the browser'
description: 'Run SQL against a Kipper-managed database from the console, browse tables and export results, without exposing the database to the internet.'
---

# Database Console

Kipper has a built-in database client in the web console for Postgres and MySQL services. You can browse tables, edit rows, design schema, manage indexes, run SQL with autocomplete, and ask the AI assistant to write queries that actually use your real schema. No desktop tool needed.

To open it: go to **Services** in the sidebar, hover any Postgres or MySQL row, click the code-icon button on the right (or open the side panel and click the same button next to AI Diagnose). You can also navigate directly to `/services/<name>/data`.

## Layout

```
┌──────────────┬──────────────────────────────────┬───────────────┐
│              │  Browse  ·  SQL  ·  Designer  ·  Indexes          │
│              │                                                   │
│  Schema      │  ────────────────────────────────                 │
│  sidebar     │                                                   │
│              │  (active tab content)                             │
│              │                                                   │
└──────────────┴──────────────────────────────────┴───────────────┘
                                                  ↑
                                              optional AI side
                                              panel slides out
                                              from the right
```

The **database picker** at the top of the sidebar lets you switch between the databases that live in this service. A postgres or mysql service holds the default `app` database plus one per app binding, so the picker is how you tell the schema tree, SQL editor, row browser, and designer which database to operate on. The selection lives in the URL as `?database=`, so reloads and shared links land on the same database. The service's default database (the one new app bindings attach to when no name is given) is marked `(default)` in the list, so you can tell at a glance whether destructive SQL would land on a binding-specific database or on the shared default.

The **schema sidebar** on the left shows every schema and relation in the picked database. Click a table to load it; double-click to insert its qualified name into the SQL editor. There's a `+` button at the top of the sidebar to create a new table, and a chevron to collapse the sidebar entirely when you want more horizontal space. A schema with no tables still appears in the tree, so you can tell at a glance that you're on an empty database vs. one that simply has nothing under that schema.

The **AI button** in the header opens a side panel on the right that knows your live schema, the open table, and any SQL currently in the editor.

## Browse

Click a table in the sidebar to open it in **Browse** mode.

- Pagination controls in the toolbar (first / prev / next / last). Default page size is 50 rows.
- Header indicator shows `<n> cols · <m> rows · <ms>` so you can see at a glance whether the table has data and how long the query took.
- **Sort:** click a column header → menu → Sort. The toolbar gets a sort indicator.
- **Filter:** column header → Filter, type a value, hit Enter. Esc cancels. Empty values do nothing (so a `bigint` column doesn't get queried with `''`).
- **Edit a cell:** double-click. Inline input opens. Enter to commit, Esc to cancel. Edited cells get an amber tint until you Save them.
- **Insert a row:** click `+ Insert row`. Inline form auto-builds from column metadata, with `(default)` markers on server-generated columns (sequences, GENERATED ALWAYS).
- **Delete rows:** check the boxes on the rows you want, click `Delete N`. Runs in a single transaction; rolls back on any failure.
- **Foreign keys** render as clickable chips (`→ 42`). Click to navigate to the referenced row.
- **Save changes:** the toolbar shows `Save N changes` once any cell is dirty. Save runs an UPDATE per row, all parameterised.
- **Discard:** drops local edits without touching the database.
- **CSV export** of the current page: appears in the SQL tab's result toolbar. (For larger exports, use the SQL tab with `LIMIT` / `OFFSET` then export.)

Tables without a primary key get a banner. Inline edits and deletes are disabled because we can't address rows safely. Use the SQL tab.

## SQL

The SQL editor is a CodeMirror pane with PostgreSQL or MySQL syntax highlighting (auto-detected from the service type) and **schema-aware autocomplete**.

### Autocomplete

- Tables from your schema appear by name.
- Columns of the currently-open table appear under their name.
- Columns of *other* tables get loaded in the background after the schema loads, so a `JOIN orders ON o.user_id = u.id` autocompletes `o.id`, `o.user_id`, etc.
- Schema-qualified names (`public.users`) are indexed too.
- Dialect-specific keywords (jsonb, RETURNING, gen_random_uuid for Postgres; AUTO_INCREMENT, JSON for MySQL).
- `upperCaseKeywords` is on, so SELECT / FROM / WHERE render uppercase as you type.

### Running queries

- **Run** button: executes the statement spanning the cursor (or the selection if there is one).
- **Cmd / Ctrl + Enter:** same as Run. Bound through CodeMirror so it never inserts a newline.
- **Cmd / Ctrl + Shift + Enter:** runs everything in the editor.
- **Run all** button: same as the shortcut above.

If there are multiple statements separated by `;`, the cursor's position picks which one runs. Selecting a fragment with the mouse forces that exact text. Naive split-on-semicolons handles 95% of cases; for the awkward 5% (semicolons inside string literals) just select the statement explicitly.

### Toolbar toggles

- **Run as transaction** wraps the executed SQL in `BEGIN ... COMMIT`. Multi-statement scripts commit or roll back together.
- **No auto-limit** disables the automatic `LIMIT 1000` that's added to bare `SELECT *`. Use it when you want all rows.

### Save snippet, Explain

- **Save** opens an inline form to name the current query and pin it. Snippets live per service, shared across the team.
- **Explain** opens the AI panel and asks it to walk you through the query. The model already has the schema and the editor SQL in its system prompt, so the answer is grounded.

### Result grid

Below the editor, columns sized to content with full type info (`id bigint`, `created_at timestamp with time zone`). NULL renders italic. Long values are visible at full width. Status bar shows row count, duration, and a banner when the auto-limit was hit ("truncated, increase the limit to see more"). CSV export of the result is a single click.

If a query fails, the error appears with the exact SQL that was sent and the duration. Iterate, hit Run again.

## Designer

The Designer tab is the visual table editor. Two modes:

### Editing an existing table

Click a table in the sidebar, switch to Designer. Each column is a row showing Name, Type, Nullable, Default, and Actions:

- **Rename:** the Name cell becomes an editable input. Enter applies; Esc cancels.
- **Type…:** the Type cell becomes a grouped dropdown (Text / Numbers / Auto-increment / Boolean / Date/Time / JSON / UUID / Binary / Network / Search) with a **Custom…** option for parameterised types like `varchar(50)` or `numeric(10,2)`.
- **Toggle null:** queues a NOT NULL on/off change.
- **Drop:** queues a DROP COLUMN. Asks for confirmation because data is lost.

Each action queues an op into a pending list shown above the column rows. The **DDL preview** at the bottom regenerates server-side on every change, so you see the exact `ALTER TABLE` statements before applying.

- **Apply N changes** runs everything in one transaction. If any statement fails, the whole batch rolls back.
- **Take a backup first** (toggle): future hook for triggering a Kipper Backup before running the DDL.
- **Discard** clears the queue.

### Creating a new table

Click the `+` button at the top of the schema sidebar, or hit `New table` in the Designer empty state. The Designer switches into create mode with a fresh column-row form, pre-populated with sensible defaults:

- `id bigserial NOT NULL` (primary key)
- `created_at timestamptz NOT NULL DEFAULT now()`

Both can be removed; new columns are added with `+ Add column`. Each row has Name, Type (same picker as the alter side), Nullable, Default, and a PK checkbox. Live DDL preview at the bottom updates as you type. Click **Create table** to commit; the new table is auto-selected so you can keep designing or open the row browser straight away.

## Indexes

Index list shows every index on the open table with method (btree / gin / gist / hash), columns, and unique flag. PRIMARY indexes are read-only.

### Creating an index

Click `+ Create index`. The form has:

- **Name** (optional). Placeholder shows a sensible suggestion like `<table>_<col1>_<col2>_idx`. Leave blank to use it.
- **Columns.** A chip-style picker. `+ Add column` lists every real column of the table with its type. Selected columns appear as numbered chips (`1.`, `2.`, …) with up/down arrows to reorder and an `×` to remove. Order matters because Postgres uses the leftmost prefix for query planning.
- **Method.** btree (default), gin, gist, hash.
- **WHERE clause.** Partial index predicate, Postgres only.
- **Unique** and **Concurrent** toggles.

DDL preview shows the `CREATE INDEX` statement live. **Concurrent** runs outside any transaction (Postgres forbids `CREATE INDEX CONCURRENTLY` inside one) so it doesn't lock the table on busy systems.

## AI side panel

Click **AI** in the header. A panel slides out on the right with the chat. The panel header shows a "Kipper knows" block with the dialect, the open table (with column and index counts), and the total table count. That's how you know what the model can see.

What the AI gets in its system prompt:

- Dialect (postgres or mysql), with dialect-specific guidance baked in.
- Every table name in the schema.
- Full columns + indexes + foreign keys for the open table.
- Other tables ship shape-only so the model can write joins.
- The current SQL editor contents, so "Explain this query" or "Make this faster" prompts have something to work with.

Secret values, env values, and row data are never sent. Names only.

When the AI returns a SQL block, **Apply to editor** drops it into the SQL tab and switches focus there. **Copy** copies it.

## Snippets and history

Two panels in the SQL tab's left rail.

**Snippets** are saved queries scoped per service. Click the bookmark icon on the SQL toolbar, name the query, optionally pin it. Pinned snippets bubble to the top with an amber pin icon. Click a snippet to load it into the editor. Hover for a delete button.

**History** is your last 100 unique queries against the open service. Each entry shows a green or red dot (success / error), relative timestamp ("3m ago"), duration, and the SQL preview. Click an entry to load it into the editor. Re-running a query bumps its existing entry to the top with a fresh timestamp instead of creating a duplicate.

History is per-user (keyed by your JWT email). Snippets are shared across the team.

## DDL safety

Several rails to keep destructive changes deliberate:

- **Confirm dialog** on `DROP`, `TRUNCATE`, and `DELETE` without `WHERE` from the SQL tab.
- **Run as transaction** toggle so multi-statement DDL commits or rolls back together.
- **DDL preview pane** in the Designer and Indexes editors so you see the SQL before pressing Apply.
- **Auto-refresh**: when the SQL tab runs DDL (`CREATE`, `ALTER`, `DROP`, `TRUNCATE`, `COMMENT ON`, `GRANT`, `REVOKE`), the schema sidebar, the open table's structure, and the autocomplete cache all reload automatically. Designer and Indexes tabs reflect reality on the next click.
- **Audit log**: every query emits a stdout line on the console-api with `service`, `user`, `sql_hash` (SHA-256 prefix), `duration_ms`, and `status`. Loki picks it up. SQL values are never logged.

## Permissions

The database console runs SQL through the console-api using the service's existing credentials Secret. The browser never sees credentials.

- **Read-only operations** (schema browsing, row reads, query history) need any authenticated role.
- **Write operations** (rows, DDL, snippets) require the deployer role.
- **Audit** of past queries is admin-visible via Loki.

## Supported services

Postgres and MySQL.

For other database types (MongoDB, Redis, OpenSearch, RabbitMQ), connect a desktop tool through `kip tunnel` as before. See [Stateful Services / Connecting from your machine](/en/services#connecting-from-your-machine).

## Tips

- **Multi-statement scripts.** Leave several queries in the editor separated by `;`. Cmd+Enter runs the one your cursor is in. Useful for iterating on a JOIN while the seed INSERT stays at the top.
- **Schema-aware AI.** If the AI invents a column name, it'll usually be because that column wasn't in the open table or the cache hadn't loaded yet. Click the table in the sidebar so its full schema is sent.
- **Cmd+Enter on a single query.** Works the same as the Run button. No need to select.
- **Cancel a filter.** Esc inside the filter input clears it. Saves a click.
- **Take a backup before destructive DDL.** For production-shaped changes, run `kip backup create <service>` from the CLI before applying.
