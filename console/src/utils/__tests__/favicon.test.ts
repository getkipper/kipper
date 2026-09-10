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

// The real page offers five icons. A browser picks whichever it likes, so the
// tests use the same set rather than a single convenient link.
function setIconLinks() {
  document.head.innerHTML = [
    '<link rel="icon" type="image/svg+xml" href="/logo.svg">',
    '<link rel="alternate icon" type="image/x-icon" href="/favicon.ico">',
    '<link rel="icon" type="image/png" sizes="192x192" href="/icon-192.png">',
    '<link rel="icon" type="image/png" sizes="512x512" href="/icon-512.png">',
  ].join('\n')
}

function iconHrefs(): string[] {
  return Array.from(
    document.head.querySelectorAll<HTMLLinkElement>('link[rel="icon"], link[rel="alternate icon"]'),
  ).map(link => link.getAttribute('href') as string)
}

beforeEach(() => {
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
  it('repaints every icon the page offers, not just the SVG one', async () => {
    setIconLinks()
    expect(await applyFaviconColour('purple')).toBe(true)

    const hrefs = iconHrefs()
    expect(hrefs.length).toBeGreaterThan(0)
    for (const href of hrefs) {
      // A blob URL is resolved by the page and a browser reads the icon outside
      // it, so only a data URL actually reaches the tab.
      expect(href, 'a stale icon left behind keeps the old colour on the tab').toMatch(/^data:image\//)
    }
    expect(fetch).toHaveBeenCalledWith('/logo.svg')
  })

  it('leaves no icon behind that still points at the original artwork', async () => {
    setIconLinks()
    await applyFaviconColour('red')
    expect(iconHrefs()).not.toContain('/icon-192.png')
    expect(iconHrefs()).not.toContain('/favicon.ico')
    expect(iconHrefs()).not.toContain('/logo.svg')
  })

  it('puts the page\'s own icons back for the default colour', async () => {
    setIconLinks()
    const before = iconHrefs()
    await applyFaviconColour('orange')
    expect(await applyFaviconColour(DEFAULT_FAVICON_COLOUR)).toBe(true)
    expect(iconHrefs()).toEqual(before)
  })

  it('carries the chosen tints in the icon it publishes', async () => {
    setIconLinks()
    await applyFaviconColour('pink')
    const href = iconHrefs()[0]
    expect(decodeURIComponent(href)).toContain(FAVICON_TINTS.pink[0])
    expect(decodeURIComponent(href)).not.toContain('#0EA5E9')
  })

  it('keeps the colour chosen last when two recolours overlap', async () => {
    setIconLinks()
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
    const redHrefs = iconHrefs()

    // The earlier request answers late. Publishing it now would put the colour
    // nobody asked for last on the tab.
    finish[0]()
    expect(await purple).toBe(false)
    expect(iconHrefs()).toEqual(redHrefs)
  })

  it('leaves the icons alone for a colour it cannot draw', async () => {
    setIconLinks()
    const before = iconHrefs()
    expect(await applyFaviconColour('chartreuse')).toBe(false)
    expect(iconHrefs()).toEqual(before)
    expect(fetch).not.toHaveBeenCalled()
  })

  it('leaves the icons alone when the artwork cannot be read', async () => {
    setIconLinks()
    const before = iconHrefs()
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        throw new Error('offline')
      }),
    )
    expect(await applyFaviconColour('green')).toBe(false)
    expect(iconHrefs()).toEqual(before)
  })

  it('leaves the icons alone when the artwork answers with an error', async () => {
    setIconLinks()
    const before = iconHrefs()
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => ({ ok: false, text: async () => 'nope' })),
    )
    expect(await applyFaviconColour('green')).toBe(false)
    expect(iconHrefs()).toEqual(before)
  })

  it('does nothing when the page has no icon link', async () => {
    document.head.innerHTML = ''
    expect(await applyFaviconColour('green')).toBe(false)
  })
})
