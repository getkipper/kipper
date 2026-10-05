import type { Series } from '@/api/activity'

/** Maps a point index or a value onto the chart's pixels. */
export type Scale = (v: number) => number

function pt(n: number): string {
  return String(Math.round(n * 10) / 10)
}

function runs(values: Series): number[][] {
  const out: number[][] = []
  let current: number[] = []
  values.forEach((v, i) => {
    if (v === null) {
      if (current.length) out.push(current)
      current = []
    } else {
      current.push(i)
    }
  })
  if (current.length) out.push(current)
  return out
}

/** Build an SVG path, breaking at nulls to preserve gaps. */
export function linePath(values: Series, x: Scale, y: Scale): string {
  return runs(values)
    .map(run => {
      if (run.length === 1) return `M${pt(x(run[0]))},${pt(y(values[run[0]]!))} l0,0`
      return run.map((i, k) => `${k === 0 ? 'M' : 'L'}${pt(x(i))},${pt(y(values[i]!))}`).join(' ')
    })
    .join(' ')
}

/** Hold each value until the next point, including the first null after a run. */
export function stepPath(values: Series, x: Scale, y: Scale): string {
  return runs(values)
    .map(run => {
      const parts = [`M${pt(x(run[0]))},${pt(y(values[run[0]]!))}`]
      run.forEach((i, k) => {
        if (k > 0) parts.push(`V${pt(y(values[i]!))}`)
        const next = k + 1 < run.length ? run[k + 1] : i + 1 < values.length ? i + 1 : null
        if (next !== null) parts.push(`H${pt(x(next))}`)
      })
      return parts.join(' ')
    })
    .join(' ')
}

/** Build one closed SVG band per run where both edges have data. */
export function areaPaths(lower: Series, upper: Series, x: Scale, y: Scale): string[] {
  const both = lower.map((v, i) => (v === null || upper[i] === null || upper[i] === undefined ? null : v))
  return runs(both).map(run => {
    const top = run.map((i, k) => `${k === 0 ? 'M' : 'L'}${pt(x(i))},${pt(y(upper[i]!))}`)
    const bottom = [...run].reverse().map(i => `L${pt(x(i))},${pt(y(lower[i]!))}`)
    return `${top.join(' ')} ${bottom.join(' ')} Z`
  })
}

export interface Bar {
  layer: number
  index: number
  x: number
  y: number
  width: number
  height: number
}

/** Stack positive values bottom-first into bars centred on each point. */
export function stackedBars(layers: Series[], x: Scale, y: Scale, width: number): Bar[] {
  const out: Bar[] = []
  const n = Math.max(0, ...layers.map(l => l.length))
  for (let i = 0; i < n; i++) {
    let base = 0
    layers.forEach((layer, l) => {
      const v = layer[i]
      if (v === null || v === undefined || v <= 0) return
      const top = y(base + v)
      out.push({ layer: l, index: i, x: x(i) - width / 2, y: top, width, height: y(base) - top })
      base += v
    })
  }
  return out
}

/** Choose a readable axis maximum that covers the data and respects the floor. */
export function niceMax(values: Series, floor = 0): number {
  const max = Math.max(floor, ...values.filter((v): v is number => v !== null))
  if (max <= 0) return floor || 1
  const power = 10 ** Math.floor(Math.log10(max))
  for (const step of [1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) {
    const candidate = step * power
    if (candidate >= max - 1e-9) return Math.round(candidate * 1e6) / 1e6
  }
  return 10 * power
}

/** Map ascending Unix timestamps to plot coordinates between 0 and width. */
export function timeScale(timestamps: number[], width: number): Scale {
  const first = timestamps[0] ?? 0
  const span = (timestamps[timestamps.length - 1] ?? first) - first || 1
  return t => ((t - first) / span) * width
}

/** Find the nearest point on an evenly spaced grid, clamped to its endpoints. */
export function indexAtFraction(fraction: number, count: number): number | null {
  if (count <= 0) return null
  const i = Math.round(fraction * (count - 1))
  return Math.min(count - 1, Math.max(0, i))
}
