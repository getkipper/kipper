/**
 * Browser implementation of the grammar in controller/pkg/envtemplate.
 * Keep parsing and escapes aligned so rendering and credential warnings agree.
 */

/** A stretch of a value: text as written, or a reference to be resolved. */
export type TemplateSegment =
  | { kind: 'literal'; text: string }
  | { kind: 'reference'; text: string; name: string; urlencode: boolean }

interface ParsedReference {
  name: string
  urlencode: boolean
  width: number
}

const NAME_START = /[A-Za-z_]/
const NAME_CHAR = /[A-Za-z0-9_]/

function validName(name: string): boolean {
  if (!name) return false
  if (!NAME_START.test(name[0])) return false
  return [...name].every(c => NAME_CHAR.test(c))
}

/**
 * Parse a leading ${NAME} placeholder with an optional :urlencode modifier.
 * Invalid names and unknown or empty modifiers remain literal.
 */
function parsePlaceholder(s: string): ParsedReference | null {
  if (s.length < 4 || s[0] !== '$' || s[1] !== '{') return null
  const close = s.indexOf('}')
  if (close < 0) return null

  const body = s.slice(2, close)
  const colon = body.indexOf(':')
  const name = colon >= 0 ? body.slice(0, colon) : body
  const modifier = colon >= 0 ? body.slice(colon + 1) : null

  if (!validName(name)) return null
  if (modifier === null) return { name, urlencode: false, width: close + 1 }
  if (modifier === 'urlencode') return { name, urlencode: true, width: close + 1 }
  return null
}

/**
 * Split literal text and references for the editor. $${NAME} escapes a valid
 * placeholder to literal ${NAME}; other $$ sequences remain unchanged.
 */
export function parseTemplate(value: string): TemplateSegment[] {
  const segments: TemplateSegment[] = []
  const pushLiteral = (text: string) => {
    const last = segments[segments.length - 1]
    if (last?.kind === 'literal') last.text += text
    else segments.push({ kind: 'literal', text })
  }

  let i = 0
  while (i < value.length) {
    if (value[i] !== '$') {
      pushLiteral(value[i])
      i++
      continue
    }

    if (value[i + 1] === '$') {
      const escaped = parsePlaceholder(value.slice(i + 1))
      if (escaped) {
        pushLiteral(value.slice(i + 1, i + 1 + escaped.width))
        i += 1 + escaped.width
        continue
      }
      pushLiteral('$$')
      i += 2
      continue
    }

    const ref = parsePlaceholder(value.slice(i))
    if (!ref) {
      pushLiteral('$')
      i++
      continue
    }
    segments.push({
      kind: 'reference',
      text: value.slice(i, i + ref.width),
      name: ref.name,
      urlencode: ref.urlencode,
    })
    i += ref.width
  }

  return segments
}

/** The names a value references, in order of appearance and deduplicated. */
export function templateNames(value: string): string[] {
  const seen = new Set<string>()
  for (const segment of parseTemplate(value)) {
    if (segment.kind === 'reference') seen.add(segment.name)
  }
  return [...seen]
}

/** Reports whether a value holds anything Kipper will resolve. */
export function isTemplate(value: string): boolean {
  return parseTemplate(value).some(s => s.kind === 'reference')
}

/**
 * Return literal text for credential checks, preserving escaped placeholders.
 */
export function stripPlaceholders(value: string): string {
  return parseTemplate(value)
    .filter(s => s.kind === 'literal')
    .map(s => s.text)
    .join('')
}

/**
 * Return deduplicated $(NAME) references for diagnostics. They remain literal
 * in Kipper's envFrom values; ${NAME} is the supported template syntax.
 */
export function shellStyleRefs(value: string): string[] {
  const names = new Set<string>()
  let i = 0
  while (i + 3 < value.length) {
    if (value[i] !== '$' || value[i + 1] !== '(') {
      i++
      continue
    }
    const close = value.indexOf(')', i)
    if (close < 0) break
    const name = value.slice(i + 2, close)
    if (!validName(name)) {
      i++
      continue
    }
    names.add(name)
    i = close + 1
  }
  return [...names]
}
