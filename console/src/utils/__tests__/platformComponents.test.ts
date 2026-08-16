import { describe, expect, it } from 'vitest'
import {
  PLATFORM_COMPONENTS,
  PLATFORM_DEFAULT_BOUNDS,
  platformConfig,
} from '../platformComponents'

describe('platformConfig', () => {
  it('returns the entry for a known component', () => {
    const c = platformConfig('prometheus')
    expect(c).not.toBeNull()
    expect(c!.namespace).toBe('monitoring')
    expect(c!.selector).toBe('app.kubernetes.io/name=prometheus')
    expect(c!.memoryMin).toBeGreaterThan(0)
    expect(c!.memoryMax).toBeGreaterThan(c!.memoryMin)
  })

  it('returns null for an unknown component', () => {
    expect(platformConfig('madeup')).toBeNull()
  })

  it('covers every component the plan enumerates', () => {
    const expected = [
      'prometheus',
      'loki',
      'grafana',
      'kube-state-metrics',
      'longhorn',
      'dex',
      'zot',
      'console-api',
      'traefik',
      'cert-manager',
      'keda',
      'velero',
      'promtail',
    ]
    for (const name of expected) {
      expect(PLATFORM_COMPONENTS[name]).toBeDefined()
    }
    expect(Object.keys(PLATFORM_COMPONENTS).sort()).toEqual(expected.sort())
  })

  it('has sensible default bounds', () => {
    expect(PLATFORM_DEFAULT_BOUNDS.memoryMin).toBeLessThan(PLATFORM_DEFAULT_BOUNDS.memoryMax)
  })
})
