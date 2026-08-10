import { describe, it, expect } from 'vitest'

import {
  isTemplate,
  parseTemplate,
  shellStyleRefs,
  stripPlaceholders,
  templateNames,
} from '../envTemplate'

// These are the fixtures `controller/pkg/envtemplate` is held to. The two
// grammars have to agree, so the cases that pin the Go parser pin this one.
describe('parseTemplate', () => {
  it('splits a connection string into its literals and references', () => {
    const segments = parseTemplate('postgres://${DB_USERNAME}:${DB_PASSWORD:urlencode}@${DB_HOST}/app')
    expect(segments.map(s => s.kind)).toEqual([
      'literal', 'reference', 'literal', 'reference', 'literal', 'reference', 'literal',
    ])
    expect(segments.filter(s => s.kind === 'reference').map(s => s.name))
      .toEqual(['DB_USERNAME', 'DB_PASSWORD', 'DB_HOST'])
    const encoded = segments.find(s => s.kind === 'reference' && s.name === 'DB_PASSWORD')
    expect(encoded && encoded.kind === 'reference' && encoded.urlencode).toBe(true)
  })

  it('treats $${NAME} as an escape yielding the literal text', () => {
    const segments = parseTemplate('$${LEVEL}')
    expect(segments).toEqual([{ kind: 'literal', text: '${LEVEL}' }])
    expect(templateNames('$${LEVEL}')).toEqual([])
  })

  it('only escapes a pair of dollars touching a well-formed placeholder', () => {
    // A third dollar separates the pair from the placeholder, so the pair is
    // literal and the placeholder resolves.
    expect(templateNames('$$${NAME}')).toEqual(['NAME'])
    // Values that were never templates come back untouched.
    expect(stripPlaceholders('$$10')).toBe('$$10')
    expect(stripPlaceholders("awk '{print $1}'")).toBe("awk '{print $1}'")
  })

  it('leaves anything that is not a well-formed placeholder alone', () => {
    for (const value of [
      '${1BAD}',          // a digit cannot start a name
      '${}',              // empty
      '${NAME:}',         // a modifier that is not one
      '${NAME:unknown}',  // an unknown modifier
      '${NAME',           // unclosed
      'postgresql://user${:pass}@host/db', // not a name, so the delimiters are real
      'cost: $5',
    ]) {
      expect(isTemplate(value), value).toBe(false)
      expect(stripPlaceholders(value), value).toBe(value)
    }
  })

  it('returns a value with no placeholder byte for byte', () => {
    const value = 'a plain value with $ and { and } and : in it'
    expect(parseTemplate(value)).toEqual([{ kind: 'literal', text: value }])
    expect(stripPlaceholders(value)).toBe(value)
  })
})

describe('templateNames', () => {
  it('reports each name once, in order of appearance', () => {
    expect(templateNames('${B}-${A}-${B}')).toEqual(['B', 'A'])
  })
})

// Kipper resolves ${NAME} and nothing else. $(NAME) is Kubernetes' own syntax
// and is inert here, because spec.env reaches the pod through envFrom and
// envFrom values are never expanded.
describe('shellStyleRefs', () => {
  it('reports a Kubernetes-style reference', () => {
    expect(shellStyleRefs('postgres://$(DB_HOST)/app')).toEqual(['DB_HOST'])
  })

  it('does not confuse it with Kipper syntax', () => {
    expect(shellStyleRefs('${DB_HOST}')).toEqual([])
  })

  it('leaves a command substitution alone, which is why it is not accepted as an alias', () => {
    expect(shellStyleRefs('$(date +%s)')).toEqual([])
    expect(shellStyleRefs('$(whoami)')).toEqual(['whoami'])
  })

  it('rejects what is not a name', () => {
    expect(shellStyleRefs('$(1BAD)')).toEqual([])
    expect(shellStyleRefs('$()')).toEqual([])
    expect(shellStyleRefs('$(DB_HOST')).toEqual([])
  })

  it('never rewrites the value, whatever it finds', () => {
    const value = 'redis://:$(REDIS_PASSWORD)@cache:6379'
    expect(stripPlaceholders(value)).toBe(value)
  })
})
