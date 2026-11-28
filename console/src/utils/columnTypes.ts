import type { DBColumnSpec } from '@/api/database'

export type SQLDialect = 'postgres' | 'mysql'

export interface TypeGroup {
  label: string
  types: string[]
}

const POSTGRES_TYPE_GROUPS: TypeGroup[] = [
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

// MySQL varchar presets include a required length, and auto-increment
// presets use integer types with an explicit AUTO_INCREMENT attribute.
const MYSQL_TYPE_GROUPS: TypeGroup[] = [
  { label: 'Text', types: ['varchar(255)', 'text', 'char(36)', 'mediumtext', 'longtext'] },
  { label: 'Numbers', types: ['int', 'bigint', 'smallint', 'tinyint', 'decimal(10,2)', 'double', 'float'] },
  { label: 'Auto-increment', types: ['bigint unsigned auto_increment', 'int unsigned auto_increment'] },
  { label: 'Boolean', types: ['boolean'] },
  { label: 'Date/Time', types: ['datetime', 'timestamp', 'date', 'time', 'year'] },
  { label: 'JSON', types: ['json'] },
  { label: 'Binary', types: ['blob', 'varbinary(255)', 'binary(16)'] },
]

/**
 * Returns the designer's column type presets in display order for the dialect.
 * The arrays and group objects are shared across calls. Treat them as read-only.
 */
export function typeGroupsFor(dialect: SQLDialect): TypeGroup[] {
  return dialect === 'mysql' ? MYSQL_TYPE_GROUPS : POSTGRES_TYPE_GROUPS
}

/**
 * Returns fresh, editable column objects for the dialect, ordered as id then created_at.
 * Both columns are non-nullable. The id auto-increments, and created_at defaults to
 * the current time. Callers must add the primary key constraint for id separately.
 */
export function defaultNewTableColumns(dialect: SQLDialect): DBColumnSpec[] {
  if (dialect === 'mysql') {
    return [
      { name: 'id', type: 'bigint unsigned auto_increment', nullable: false, default: null },
      { name: 'created_at', type: 'datetime', nullable: false, default: 'CURRENT_TIMESTAMP' },
    ]
  }
  return [
    { name: 'id', type: 'bigserial', nullable: false, default: null },
    { name: 'created_at', type: 'timestamptz', nullable: false, default: 'now()' },
  ]
}
