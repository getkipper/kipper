const NUMERIC_TYPE = /^((tiny|small|medium|big)?int(eger)?|numeric|decimal|real|double|float)\b/
const BOOLEAN_TYPE = /^bool(ean)?\b/

/**
 * Converts cell input to a value for submission based on the database column type.
 * Numeric types convert input that parses as a finite number. Boolean types convert
 * only exact 'true' and 'false' strings. Other input, including blank input, stays
 * unchanged. Type matching starts at the beginning of the trimmed type name so enum
 * and set labels such as enum('print') stay strings.
 */
export function coerceCellValue(type: string, buf: string): unknown {
  const lower = type.trim().toLowerCase()
  if (NUMERIC_TYPE.test(lower) && buf.trim() !== '') {
    const n = Number(buf)
    if (Number.isFinite(n)) return n
  }
  if (BOOLEAN_TYPE.test(lower)) {
    if (buf === 'true') return true
    if (buf === 'false') return false
  }
  return buf
}
