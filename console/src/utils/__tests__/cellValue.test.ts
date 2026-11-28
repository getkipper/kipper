import { describe, expect, it } from 'vitest'
import { coerceCellValue } from '../cellValue'

describe('coerceCellValue', () => {
  it('turns numeric input into a number for numeric column types', () => {
    for (const type of ['integer', 'bigint', 'smallint', 'numeric', 'real', 'double precision',
      'int', 'tinyint(1)', 'bigint unsigned', 'mediumint', 'decimal(10,2)', 'double', 'float']) {
      expect(coerceCellValue(type, '42'), type).toBe(42)
    }
  })

  it('turns true and false into booleans for boolean columns', () => {
    expect(coerceCellValue('boolean', 'true')).toBe(true)
    expect(coerceCellValue('boolean', 'false')).toBe(false)
  })

  it('keeps enum and set labels as strings even when a label spells a type name', () => {
    expect(coerceCellValue("enum('print','001')", '001')).toBe('001')
    expect(coerceCellValue("set('bool','true')", 'true')).toBe('true')
  })

  it('keeps text, non-numeric input and interval values as strings', () => {
    expect(coerceCellValue('varchar(255)', '42')).toBe('42')
    expect(coerceCellValue('integer', 'abc')).toBe('abc')
    expect(coerceCellValue('interval', '1')).toBe('1')
  })

  it('keeps blank and non-finite input on numeric columns as typed', () => {
    expect(coerceCellValue('integer', '')).toBe('')
    expect(coerceCellValue('int', '  ')).toBe('  ')
    expect(coerceCellValue('double', 'Infinity')).toBe('Infinity')
  })
})
