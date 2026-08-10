// Pure helpers shared by the <ResourceControl> primitive and any caller that
// needs to talk about Kubernetes-style resource quantities.
//
// Memory is carried as raw bytes throughout the UI. CPU is carried as raw
// millicores. The API returns these as numbers; format* are only used for
// display.

export type ResourceKind = 'memory' | 'cpu'

export type ResourceBand = 'healthy' | 'warning' | 'critical'

const MEMORY_UNITS: Array<{ suffix: string; factor: number }> = [
  { suffix: 'Ei', factor: 1024 ** 6 },
  { suffix: 'Pi', factor: 1024 ** 5 },
  { suffix: 'Ti', factor: 1024 ** 4 },
  { suffix: 'Gi', factor: 1024 ** 3 },
  { suffix: 'Mi', factor: 1024 ** 2 },
  { suffix: 'Ki', factor: 1024 },
  { suffix: 'E', factor: 1000 ** 6 },
  { suffix: 'P', factor: 1000 ** 5 },
  { suffix: 'T', factor: 1000 ** 4 },
  { suffix: 'G', factor: 1000 ** 3 },
  { suffix: 'M', factor: 1000 ** 2 },
  { suffix: 'K', factor: 1000 },
]

// Parse a Kubernetes-style memory quantity ("128Mi", "2Gi", "1024") into raw
// bytes. Throws on malformed input.
export function parseMemoryQuantity(s: string): number {
  const trimmed = s.trim()
  if (!trimmed) throw new Error('empty memory quantity')
  for (const { suffix, factor } of MEMORY_UNITS) {
    if (trimmed.endsWith(suffix)) {
      const rest = trimmed.slice(0, -suffix.length)
      if (rest === '') throw new Error(`invalid memory quantity: ${s}`)
      const num = Number(rest)
      if (!Number.isFinite(num)) throw new Error(`invalid memory quantity: ${s}`)
      return num * factor
    }
  }
  const bare = Number(trimmed)
  if (!Number.isFinite(bare)) throw new Error(`invalid memory quantity: ${s}`)
  return bare
}

// Parse a Kubernetes-style CPU quantity ("500m", "2", "1.5") into millicores.
export function parseCpuQuantity(s: string): number {
  const trimmed = s.trim()
  if (!trimmed) throw new Error('empty cpu quantity')
  if (trimmed.endsWith('m')) {
    const rest = trimmed.slice(0, -1)
    if (rest === '') throw new Error(`invalid cpu quantity: ${s}`)
    const num = Number(rest)
    if (!Number.isFinite(num)) throw new Error(`invalid cpu quantity: ${s}`)
    return num
  }
  const cores = Number(trimmed)
  if (!Number.isFinite(cores)) throw new Error(`invalid cpu quantity: ${s}`)
  return cores * 1000
}

// Format raw bytes as a binary quantity ("128 Mi", "1.5 Gi"). Picks the
// largest unit that produces a value >= 1.
export function formatMemory(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—'
  if (bytes === 0) return '0'
  const binary = MEMORY_UNITS.filter((u) => u.suffix.endsWith('i'))
  for (const { suffix, factor } of binary) {
    if (bytes >= factor) {
      const value = bytes / factor
      const text = value >= 100 || Number.isInteger(value)
        ? value.toFixed(0)
        : value.toFixed(value >= 10 ? 1 : 2).replace(/\.?0+$/, '')
      return `${text} ${suffix}`
    }
  }
  return `${bytes} B`
}

// Format raw millicores ("500m", "1", "2.5"). Values >= 1000m become cores.
export function formatCpu(millis: number): string {
  if (!Number.isFinite(millis) || millis < 0) return '—'
  if (millis === 0) return '0'
  if (millis < 1000) return `${Math.round(millis)}m`
  const cores = millis / 1000
  if (Number.isInteger(cores)) return `${cores}`
  return cores.toFixed(2).replace(/\.?0+$/, '')
}

export function formatQuantity(value: number, kind: ResourceKind): string {
  return kind === 'memory' ? formatMemory(value) : formatCpu(value)
}

// Render a memory byte count as a Kubernetes quantity string that the API
// will accept ("2Gi", "256Mi"). Slider stops always land on clean binary
// multiples so the integer branches cover the common case; arbitrary
// inputs fall through to the largest exact-multiple unit.
export function toKubernetesMemoryQuantity(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0'
  const Ti = 1024 ** 4
  const Gi = 1024 ** 3
  const Mi = 1024 ** 2
  const Ki = 1024
  if (bytes % Ti === 0) return `${bytes / Ti}Ti`
  if (bytes % Gi === 0) return `${bytes / Gi}Gi`
  if (bytes % Mi === 0) return `${bytes / Mi}Mi`
  if (bytes % Ki === 0) return `${bytes / Ki}Ki`
  return `${bytes}`
}

// Same idea for CPU millicores → "500m" or "2".
export function toKubernetesCpuQuantity(millis: number): string {
  if (!Number.isFinite(millis) || millis <= 0) return '0'
  if (millis % 1000 === 0) return `${millis / 1000}`
  return `${millis}m`
}

// Index of the stop closest to `value` (by absolute difference in linear
// space, since stops are usually close enough that log-distance gives the
// same answer). Returns -1 for an empty stops array.
export function nearestStop(value: number, stops: number[]): number {
  if (stops.length === 0) return -1
  let bestIdx = 0
  let bestDist = Math.abs(stops[0] - value)
  for (let i = 1; i < stops.length; i++) {
    const d = Math.abs(stops[i] - value)
    if (d < bestDist) {
      bestDist = d
      bestIdx = i
    }
  }
  return bestIdx
}

export interface BandThresholds {
  warning: number
  critical: number
}

export const DEFAULT_BANDS: BandThresholds = { warning: 0.6, critical: 0.85 }

// Map a usage/limit ratio to a color band. Ratios above 1 are critical (over
// limit). NaN or negative input is treated as healthy so the gauge degrades
// quietly when usage data hasn't loaded yet.
export function ratioBand(ratio: number, bands: BandThresholds = DEFAULT_BANDS): ResourceBand {
  if (!Number.isFinite(ratio) || ratio < 0) return 'healthy'
  if (ratio >= bands.critical) return 'critical'
  if (ratio >= bands.warning) return 'warning'
  return 'healthy'
}

// Default slider stops for memory (binary) and CPU (millicores). Each
// component declares its own min/max from this list (or a subset).
export const MEMORY_STOPS: number[] = [
  128 * 1024 ** 2,
  256 * 1024 ** 2,
  512 * 1024 ** 2,
  1 * 1024 ** 3,
  2 * 1024 ** 3,
  4 * 1024 ** 3,
  8 * 1024 ** 3,
  16 * 1024 ** 3,
]

export const CPU_STOPS: number[] = [50, 100, 250, 500, 1000, 2000, 4000]
