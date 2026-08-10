/**
 * The `${NAME}` grammar, as the console has to read it to render a value.
 *
 * This mirrors `controller/pkg/envtemplate`, which is where the grammar is
 * defined and where the reconciler resolves against it. The browser cannot call
 * Go, so this is the one restatement that has to exist; everything else routes
 * through the package. Keeping the two in step matters in both directions: a
 * broader grammar here reports a real embedded password as safe, a narrower one
 * warns about the templated form the feature exists to encourage.
 *
 * The regex this replaced could not express the `$${NAME}` escape at all, because
 * JavaScript could do it and Go's RE2 has no lookbehind, so both sides carried
 * the same known gap rather than diverging. A parser has no such limit.
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
 * Reads a placeholder at the start of `s`, which must begin with `$`.
 *
 * The only modifier is `:urlencode`. A colon introduces one, so `${NAME:}` is a
 * modifier that is not one rather than a plain reference, and it is left literal
 * like `${NAME:unknown}` — a typo should be visible instead of quietly dropping
 * an encoding a credential needed.
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
 * Splits a value into the stretches that make it up, so the editor can show
 * which parts of it are references without guessing at the boundaries.
 *
 * `$${NAME}` is the escape and yields the literal `${NAME}`, and only directly
 * before a well-formed placeholder: leaving every other `$$` alone keeps this a
 * no-op on values that were never templates, such as `$$10` or an awk snippet.
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
 * Removes every reference, leaving the literal text around them.
 *
 * The credential warning asks whether what remains still carries a password of
 * its own. A templated URL resolves its credential at render time and never
 * stores one on the CR, so warning about it would argue against the safe
 * construction.
 */
export function stripPlaceholders(value: string): string {
  return parseTemplate(value)
    .filter(s => s.kind === 'literal')
    .map(s => s.text)
    .join('')
}

/**
 * The names a value references in Kubernetes' own `$(NAME)` form.
 *
 * Kipper resolves none of them, and neither does the kubelet: a workload's
 * environment reaches its pod through envFrom, and envFrom values are copied in
 * without expansion. `$(NAME)` is expanded only in a container's own env,
 * command and args, which is not where spec.env goes. So the value arrives at
 * the process exactly as typed, which is worth saying, because it looks like it
 * should work.
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
