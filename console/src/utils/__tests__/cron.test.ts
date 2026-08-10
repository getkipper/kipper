import { describe, expect, it } from 'vitest'
import { describeCron, computeNextRuns } from '../cron'

describe('describeCron', () => {
  it('describes preset patterns', () => {
    expect(describeCron('0 2 * * *').text).toBe('Every day at 02:00 UTC')
    expect(describeCron('*/5 * * * *').text).toBe('Every 5 minutes')
    expect(describeCron('0 9 * * 1').text).toBe('Every Monday at 09:00 UTC')
    expect(describeCron('0 * * * *').text).toBe('Every hour, on the hour')
    expect(describeCron('0 3 1 * *').text).toBe('Day 1 of every month at 03:00 UTC')
  })

  it('falls back to Custom schedule for unrecognised expressions', () => {
    expect(describeCron('1-15/2 * * * *').text).toBe('Custom schedule')
  })

  it('returns empty for blank input', () => {
    expect(describeCron('').text).toBe('')
    expect(describeCron('').nextRuns).toEqual([])
  })
})

describe('computeNextRuns', () => {
  it('produces N runs for hourly cron', () => {
    const runs = computeNextRuns('0 * * * *', 3)
    expect(runs).toHaveLength(3)
    for (const iso of runs) {
      const d = new Date(iso)
      expect(d.getUTCMinutes()).toBe(0)
    }
  })

  it('produces N runs for every-5-minutes cron', () => {
    const runs = computeNextRuns('*/5 * * * *', 4)
    expect(runs).toHaveLength(4)
    for (const iso of runs) {
      const d = new Date(iso)
      expect(d.getUTCMinutes() % 5).toBe(0)
    }
  })

  it('produces N runs for daily-at-02:00 cron in chronological order', () => {
    const runs = computeNextRuns('0 2 * * *', 3)
    expect(runs).toHaveLength(3)
    const t0 = new Date(runs[0]).getTime()
    const t1 = new Date(runs[1]).getTime()
    const t2 = new Date(runs[2]).getTime()
    expect(t1).toBeGreaterThan(t0)
    expect(t2).toBeGreaterThan(t1)
    for (const iso of runs) {
      const d = new Date(iso)
      expect(d.getUTCHours()).toBe(2)
      expect(d.getUTCMinutes()).toBe(0)
    }
  })

  it('returns [] for malformed cron', () => {
    expect(computeNextRuns('not a cron', 3)).toEqual([])
    expect(computeNextRuns('* * * *', 3)).toEqual([])
  })
})
