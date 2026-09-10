// @vitest-environment happy-dom
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import {
  applyFaviconColour,
  recolour,
  FAVICON_TINTS,
  FAVICON_COLOURS,
  DEFAULT_FAVICON_COLOUR,
  isFaviconColour,
} from '../favicon'

const ARTWORK = `<svg xmlns="http://www.w3.org/2000/svg">
 <style>.fil1 {fill:#0EA5E9} .fil4 {fill:#7DC3F5} .fil0 {fill:#BDE4FF} .fil3 {fill:white}</style>
 <circle class="fil1" r="10"/>
</svg>`

let created: string[] = []
let revoked: string[] = []

function setIconLink(href = '/logo.svg') {
  document.head.innerHTML = `<link rel="icon" type="image/svg+xml" href="${href}">`
  return document.head.querySelector('link') as HTMLLinkElement
}

beforeEach(() => {
  created = []
  revoked = []
  let n = 0
  vi.stubGlobal('URL', {
    ...URL,
    createObjectURL: () => {
      const url = `blob:generated-${++n}`
      created.push(url)
      return url
    },
    revokeObjectURL: (url: string) => revoked.push(url),
  })
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => ({ ok: true, text: async () => ARTWORK })),
  )
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('recolour', () => {
  it('swaps all three tints and leaves the white cut-outs alone', () => {
    const out = recolour(ARTWORK, FAVICON_TINTS.green)
    expect(out).toContain('#10B981')
    expect(out).toContain('#6EE7B7')
    expect(out).toContain('#A7F3D0')
    expect(out).not.toContain('#0EA5E9')
    expect(out).toContain('fill:white')
  })

  it('matches the artwork whichever case the hex is written in', () => {
    const out = recolour('<path fill="#0ea5e9"/><path fill="#0EA5E9"/>', FAVICON_TINTS.red)
    expect(out).not.toMatch(/0ea5e9/i)
    expect(out.match(/#EF4444/g)).toHaveLength(2)
  })
})

describe('the palette', () => {
  it('offers a colour for every entry, and no white', () => {
    expect(FAVICON_COLOURS).toContain(DEFAULT_FAVICON_COLOUR)
    expect(FAVICON_COLOURS).not.toContain('white')
    expect(FAVICON_COLOURS.length).toBe(9)
  })

  it('gives every colour three distinct tints', () => {
    for (const colour of FAVICON_COLOURS) {
      const tints = FAVICON_TINTS[colour]
      expect(tints, colour).toHaveLength(3)
      expect(new Set(tints).size, `${colour} repeats a tint`).toBe(3)
      for (const tint of tints) expect(tint, colour).toMatch(/^#[0-9A-F]{6}$/i)
    }
  })

  it('gives every colour other than blue its own main tint', () => {
    const mains = FAVICON_COLOURS.map((c) => FAVICON_TINTS[c][0])
    expect(new Set(mains).size).toBe(mains.length)
  })

  it('recognises its own names and nothing else', () => {
    expect(isFaviconColour('green')).toBe(true)
    expect(isFaviconColour('white')).toBe(false)
    expect(isFaviconColour('toString')).toBe(false)
  })
})

describe('applyFaviconColour', () => {
  it('points the icon at a recoloured copy', async () => {
    const link = setIconLink()
    expect(await applyFaviconColour('purple')).toBe(true)
    expect(link.getAttribute('href')).toBe(created[0])
    expect(fetch).toHaveBeenCalledWith('/logo.svg')
  })

  it('hands back the original file for the default colour', async () => {
    const link = setIconLink()
    await applyFaviconColour('orange')
    expect(await applyFaviconColour(DEFAULT_FAVICON_COLOUR)).toBe(true)
    expect(link.getAttribute('href')).toBe('/logo.svg')
  })

  it('releases the copy it replaces rather than leaking it', async () => {
    setIconLink()
    await applyFaviconColour('pink')
    await applyFaviconColour('brown')
    expect(revoked).toEqual([created[0]])
  })

  it('keeps the colour chosen last when two recolours overlap', async () => {
    const link = setIconLink()
    const finish: Array<() => void> = []
    vi.stubGlobal(
      'fetch',
      vi.fn(
        () =>
          new Promise(resolve => {
            finish.push(() => resolve({ ok: true, text: async () => ARTWORK }))
          }),
      ),
    )

    const purple = applyFaviconColour('purple')
    const red = applyFaviconColour('red')
    await vi.waitFor(() => expect(finish).toHaveLength(2))

    finish[1]()
    expect(await red).toBe(true)
    const redHref = link.getAttribute('href')

    // The earlier request answers late. Publishing it now would put the colour
    // nobody asked for last on the tab.
    finish[0]()
    expect(await purple).toBe(false)
    expect(link.getAttribute('href')).toBe(redHref)
  })

  it('leaves the icon alone for a colour it cannot draw', async () => {
    const link = setIconLink()
    expect(await applyFaviconColour('chartreuse')).toBe(false)
    expect(link.getAttribute('href')).toBe('/logo.svg')
    expect(fetch).not.toHaveBeenCalled()
  })

  it('leaves the icon alone when the artwork cannot be read', async () => {
    const link = setIconLink()
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        throw new Error('offline')
      }),
    )
    expect(await applyFaviconColour('green')).toBe(false)
    expect(link.getAttribute('href')).toBe('/logo.svg')
  })

  it('leaves the icon alone when the artwork answers with an error', async () => {
    const link = setIconLink()
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => ({ ok: false, text: async () => 'nope' })),
    )
    expect(await applyFaviconColour('green')).toBe(false)
    expect(link.getAttribute('href')).toBe('/logo.svg')
  })

  it('does nothing when the page has no icon link', async () => {
    document.head.innerHTML = ''
    expect(await applyFaviconColour('green')).toBe(false)
  })
})
