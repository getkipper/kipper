import { describe, expect, it } from 'vitest'
import { defaultNewTableColumns, typeGroupsFor } from '../columnTypes'

const flat = (dialect: 'postgres' | 'mysql') => typeGroupsFor(dialect).flatMap((g) => g.types)

describe('typeGroupsFor', () => {
  it('keeps the Postgres presets', () => {
    expect(flat('postgres')).toEqual(expect.arrayContaining(['text', 'bigserial', 'timestamptz', 'jsonb', 'uuid', 'bytea']))
  })

  it('offers only types MySQL accepts', () => {
    const types = flat('mysql')
    expect(types).toEqual(expect.arrayContaining(['varchar(255)', 'int', 'bigint unsigned auto_increment', 'datetime', 'json', 'blob']))
    for (const pgOnly of ['bigserial', 'timestamptz', 'jsonb', 'bytea', 'citext', 'uuid', 'interval', 'inet', 'tsvector', 'varchar']) {
      expect(types, pgOnly).not.toContain(pgOnly)
    }
  })
})

describe('defaultNewTableColumns', () => {
  it('starts a Postgres table with bigserial and timestamptz', () => {
    expect(defaultNewTableColumns('postgres')).toEqual([
      { name: 'id', type: 'bigserial', nullable: false, default: null },
      { name: 'created_at', type: 'timestamptz', nullable: false, default: 'now()' },
    ])
  })

  it('starts a MySQL table with an auto-increment id and a datetime', () => {
    expect(defaultNewTableColumns('mysql')).toEqual([
      { name: 'id', type: 'bigint unsigned auto_increment', nullable: false, default: null },
      { name: 'created_at', type: 'datetime', nullable: false, default: 'CURRENT_TIMESTAMP' },
    ])
  })
})
