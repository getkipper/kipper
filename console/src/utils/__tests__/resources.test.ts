import { describe, expect, it } from 'vitest'
import {
  CPU_STOPS,
  DEFAULT_BANDS,
  MEMORY_STOPS,
  formatCpu,
  formatMemory,
  formatQuantity,
  nearestStop,
  parseCpuQuantity,
  parseMemoryQuantity,
  ratioBand,
  toKubernetesCpuQuantity,
  toKubernetesMemoryQuantity,
} from '../resources'

describe('parseMemoryQuantity', () => {
  it('parses binary suffixes (Ki/Mi/Gi/Ti)', () => {
    expect(parseMemoryQuantity('128Mi')).toBe(128 * 1024 ** 2)
    expect(parseMemoryQuantity('2Gi')).toBe(2 * 1024 ** 3)
    expect(parseMemoryQuantity('1Ti')).toBe(1024 ** 4)
    expect(parseMemoryQuantity('512Ki')).toBe(512 * 1024)
  })

  it('parses decimal suffixes (K/M/G)', () => {
    expect(parseMemoryQuantity('1G')).toBe(1000 ** 3)
    expect(parseMemoryQuantity('500M')).toBe(500 * 1000 ** 2)
  })

  it('parses bare bytes', () => {
    expect(parseMemoryQuantity('1024')).toBe(1024)
    expect(parseMemoryQuantity('  2048 ')).toBe(2048)
  })

  it('throws on malformed input', () => {
    expect(() => parseMemoryQuantity('')).toThrow()
    expect(() => parseMemoryQuantity('Gi')).toThrow()
    expect(() => parseMemoryQuantity('apples')).toThrow()
  })
})

describe('parseCpuQuantity', () => {
  it('parses millicores', () => {
    expect(parseCpuQuantity('500m')).toBe(500)
    expect(parseCpuQuantity('50m')).toBe(50)
  })

  it('parses cores', () => {
    expect(parseCpuQuantity('2')).toBe(2000)
    expect(parseCpuQuantity('1.5')).toBe(1500)
  })

  it('throws on malformed input', () => {
    expect(() => parseCpuQuantity('')).toThrow()
    expect(() => parseCpuQuantity('many')).toThrow()
  })
})

describe('formatMemory', () => {
  it('picks the largest binary unit', () => {
    expect(formatMemory(128 * 1024 ** 2)).toBe('128 Mi')
    expect(formatMemory(2 * 1024 ** 3)).toBe('2 Gi')
    expect(formatMemory(1.5 * 1024 ** 3)).toBe('1.5 Gi')
  })

  it('handles zero and edge values', () => {
    expect(formatMemory(0)).toBe('0')
    expect(formatMemory(512)).toBe('512 B')
  })

  it('handles invalid input gracefully', () => {
    expect(formatMemory(NaN)).toBe('—')
    expect(formatMemory(-1)).toBe('—')
  })
})

describe('formatCpu', () => {
  it('formats millicores under one core', () => {
    expect(formatCpu(500)).toBe('500m')
    expect(formatCpu(50)).toBe('50m')
  })

  it('formats cores when >= 1000m', () => {
    expect(formatCpu(1000)).toBe('1')
    expect(formatCpu(2500)).toBe('2.5')
    expect(formatCpu(4000)).toBe('4')
  })

  it('handles invalid input gracefully', () => {
    expect(formatCpu(NaN)).toBe('—')
    expect(formatCpu(-1)).toBe('—')
  })
})

describe('formatQuantity', () => {
  it('dispatches by kind', () => {
    expect(formatQuantity(1024 ** 3, 'memory')).toBe('1 Gi')
    expect(formatQuantity(500, 'cpu')).toBe('500m')
  })
})

describe('nearestStop', () => {
  it('returns the index of the closest stop', () => {
    expect(nearestStop(128 * 1024 ** 2, MEMORY_STOPS)).toBe(0)
    expect(nearestStop(1.7 * 1024 ** 3, MEMORY_STOPS)).toBe(4) // 2Gi
    expect(nearestStop(20 * 1024 ** 3, MEMORY_STOPS)).toBe(7) // 16Gi, the max
  })

  it('snaps CPU values', () => {
    expect(nearestStop(120, CPU_STOPS)).toBe(1) // 100m
    expect(nearestStop(700, CPU_STOPS)).toBe(3) // 500m (dist 200) vs 1000 (dist 300)
    expect(nearestStop(900, CPU_STOPS)).toBe(4) // 1000 (dist 100) vs 500 (dist 400)
  })

  it('returns -1 for empty stops', () => {
    expect(nearestStop(123, [])).toBe(-1)
  })
})

describe('toKubernetesMemoryQuantity', () => {
  it('renders binary multiples cleanly', () => {
    const Gi = 1024 ** 3
    const Mi = 1024 ** 2
    expect(parseMemoryQuantity('2Gi')).toBe(2 * Gi)
    expect(toKubernetesMemoryQuantity(2 * Gi)).toBe('2Gi')
    expect(toKubernetesMemoryQuantity(256 * Mi)).toBe('256Mi')
    expect(toKubernetesMemoryQuantity(1024)).toBe('1Ki')
  })

  it('falls back to bytes for non-multiples', () => {
    expect(toKubernetesMemoryQuantity(123)).toBe('123')
  })

  it('handles zero and invalid input', () => {
    expect(toKubernetesMemoryQuantity(0)).toBe('0')
    expect(toKubernetesMemoryQuantity(-1)).toBe('0')
    expect(toKubernetesMemoryQuantity(NaN)).toBe('0')
  })
})

describe('toKubernetesCpuQuantity', () => {
  it('renders whole cores without suffix', () => {
    expect(toKubernetesCpuQuantity(1000)).toBe('1')
    expect(toKubernetesCpuQuantity(4000)).toBe('4')
  })

  it('renders sub-core values as millicores', () => {
    expect(toKubernetesCpuQuantity(500)).toBe('500m')
    expect(toKubernetesCpuQuantity(50)).toBe('50m')
  })

  it('handles zero and invalid input', () => {
    expect(toKubernetesCpuQuantity(0)).toBe('0')
    expect(toKubernetesCpuQuantity(-1)).toBe('0')
  })
})

describe('ratioBand', () => {
  it('classifies by default thresholds', () => {
    expect(ratioBand(0.1)).toBe('healthy')
    expect(ratioBand(0.59)).toBe('healthy')
    expect(ratioBand(0.6)).toBe('warning')
    expect(ratioBand(0.84)).toBe('warning')
    expect(ratioBand(0.85)).toBe('critical')
    expect(ratioBand(1.2)).toBe('critical')
  })

  it('treats invalid input as healthy', () => {
    expect(ratioBand(NaN)).toBe('healthy')
    expect(ratioBand(-1)).toBe('healthy')
  })

  it('accepts custom thresholds', () => {
    const bands = { warning: 0.7, critical: 0.95 }
    expect(ratioBand(0.69, bands)).toBe('healthy')
    expect(ratioBand(0.8, bands)).toBe('warning')
    expect(ratioBand(0.96, bands)).toBe('critical')
  })

  it('DEFAULT_BANDS is reasonable', () => {
    expect(DEFAULT_BANDS.warning).toBeLessThan(DEFAULT_BANDS.critical)
  })
})
