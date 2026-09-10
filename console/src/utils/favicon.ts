const ICON_HREF = '/logo.svg'

// A browser may prefer a PNG to the SVG, so all of them carry the colour.
const ICON_SELECTOR = 'link[rel="icon"], link[rel="alternate icon"]'

const RASTER_SIZE = 192

const SOURCE_TINTS = ['#0EA5E9', '#7DC3F5', '#BDE4FF'] as const

export const DEFAULT_FAVICON_COLOUR = 'blue'

export const FAVICON_TINTS = {
  blue: SOURCE_TINTS,
  green: ['#10B981', '#6EE7B7', '#A7F3D0'],
  purple: ['#8B5CF6', '#C4B5FD', '#DDD6FE'],
  orange: ['#F97316', '#FDBA74', '#FED7AA'],
  yellow: ['#F59E0B', '#FCD34D', '#FDE68A'],
  pink: ['#EC4899', '#F9A8D4', '#FBCFE8'],
  red: ['#EF4444', '#FCA5A5', '#FECACA'],
  brown: ['#92400E', '#C08A5C', '#E0C9B0'],
  grey: ['#475569', '#94A3B8', '#CBD5E1'],
} as const satisfies Record<string, readonly [string, string, string]>

export type FaviconColour = keyof typeof FAVICON_TINTS

export const FAVICON_COLOURS = Object.keys(FAVICON_TINTS) as FaviconColour[]

export function isFaviconColour(value: string): value is FaviconColour {
  return Object.prototype.hasOwnProperty.call(FAVICON_TINTS, value)
}

export function recolour(svg: string, tints: readonly string[]): string {
  // Case-insensitive: the artwork writes its hex in capitals, and an editor
  // that lowercases it should not quietly stop the swap.
  return SOURCE_TINTS.reduce((out, from, i) => out.replace(new RegExp(from, 'gi'), tints[i]), svg)
}

let originalIcons: string | null = null

let generatedUrls: string[] = []

// Two recolours can be in flight at once. The tab takes the colour chosen last,
// not the one that finishes last.
let generation = 0

function releaseGenerated(): void {
  for (const url of generatedUrls) URL.revokeObjectURL(url)
  generatedUrls = []
}

function iconLinks(): HTMLLinkElement[] {
  return Array.from(document.querySelectorAll<HTMLLinkElement>(ICON_SELECTOR))
}

// Replacing the elements is what makes a browser fetch the icon again. Several
// ignore an href that changes underneath them.
function replaceIcons(markup: string): void {
  const head = document.head
  for (const link of iconLinks()) link.remove()
  head.insertAdjacentHTML('beforeend', markup)
}

/** rasterise returns a PNG of the artwork, or null where there is no canvas. */
async function rasterise(svgUrl: string): Promise<string | null> {
  const canvas = document.createElement('canvas')
  const context = typeof canvas.getContext === 'function' ? canvas.getContext('2d') : null
  if (!context) return null
  canvas.width = RASTER_SIZE
  canvas.height = RASTER_SIZE

  const image = new Image()
  const loaded = await new Promise<boolean>(resolve => {
    const done = (ok: boolean) => () => resolve(ok)
    image.onload = done(true)
    image.onerror = done(false)
    // A decode that never settles must not hold the caller.
    setTimeout(() => resolve(false), 3000)
    image.src = svgUrl
  })
  if (!loaded) return null

  context.drawImage(image, 0, 0, RASTER_SIZE, RASTER_SIZE)
  if (typeof canvas.toBlob !== 'function') return null
  return new Promise<string | null>(resolve => {
    canvas.toBlob(blob => resolve(blob ? URL.createObjectURL(blob) : null), 'image/png')
  })
}

/** applyFaviconColour repaints every icon the page offers and reports whether
 *  it changed anything. It never throws. */
export async function applyFaviconColour(colour: string): Promise<boolean> {
  const links = iconLinks()
  if (links.length === 0 || !isFaviconColour(colour)) return false

  if (originalIcons === null) {
    originalIcons = links.map(link => link.outerHTML).join('\n')
  }

  const mine = ++generation

  // Blue is the page's own markup, so hand that back rather than rebuild it.
  if (colour === DEFAULT_FAVICON_COLOUR) {
    releaseGenerated()
    replaceIcons(originalIcons)
    return true
  }

  try {
    const response = await fetch(ICON_HREF)
    if (!response.ok) return false
    const svg = recolour(await response.text(), FAVICON_TINTS[colour])
    if (mine !== generation) return false

    const svgUrl = URL.createObjectURL(new Blob([svg], { type: 'image/svg+xml' }))
    const pngUrl = await rasterise(svgUrl)
    if (mine !== generation) {
      URL.revokeObjectURL(svgUrl)
      if (pngUrl) URL.revokeObjectURL(pngUrl)
      return false
    }

    releaseGenerated()
    generatedUrls = pngUrl ? [svgUrl, pngUrl] : [svgUrl]
    replaceIcons(
      [
        `<link rel="icon" type="image/svg+xml" href="${svgUrl}">`,
        pngUrl ? `<link rel="icon" type="image/png" sizes="${RASTER_SIZE}x${RASTER_SIZE}" href="${pngUrl}">` : '',
      ]
        .filter(Boolean)
        .join('\n'),
    )
    return true
  } catch {
    return false
  }
}
