const ICON_HREF = '/logo.svg'
const ICON_SELECTOR = 'link[rel="icon"][type="image/svg+xml"]'

/** The tints the artwork ships with, in the order every palette entry repeats. */
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

/** recolour swaps the artwork's three tints for another entry's, leaving the
 *  white cut-outs and every path alone. */
export function recolour(svg: string, tints: readonly string[]): string {
  // Case-insensitive: the artwork writes its hex in capitals, and an editor
  // that lowercases it should not quietly stop the swap.
  return SOURCE_TINTS.reduce((out, from, i) => out.replace(new RegExp(from, 'gi'), tints[i]), svg)
}

let generatedUrl: string | null = null

// Recolouring is asynchronous, so two choices can be in flight at once: the
// one made at start-up and the one made in Settings, or two quick clicks. The
// tab must end on the colour chosen last, not the one whose fetch happened to
// finish last.
let generation = 0

function releaseGenerated(): void {
  if (generatedUrl) {
    URL.revokeObjectURL(generatedUrl)
    generatedUrl = null
  }
}

/**
 * applyFaviconColour points the page's icon at a recoloured copy of the
 * artwork.
 */
export async function applyFaviconColour(colour: string): Promise<boolean> {
  const link = document.querySelector<HTMLLinkElement>(ICON_SELECTOR)
  if (!link || !isFaviconColour(colour)) return false

  const mine = ++generation

  // Blue is the file the page already links, so hand the original back rather
  // than building a copy of it.
  if (colour === DEFAULT_FAVICON_COLOUR) {
    releaseGenerated()
    link.setAttribute('href', ICON_HREF)
    return true
  }

  try {
    const response = await fetch(ICON_HREF)
    if (!response.ok) return false
    const svg = recolour(await response.text(), FAVICON_TINTS[colour])
    if (mine !== generation) return false
    releaseGenerated()
    generatedUrl = URL.createObjectURL(new Blob([svg], { type: 'image/svg+xml' }))
    link.setAttribute('href', generatedUrl)
    return true
  } catch {
    return false
  }
}
