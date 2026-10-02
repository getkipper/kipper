import { describe, expect, it } from 'vitest'
import { rolloutLabel } from '../rollout'

describe('rolloutLabel', () => {
  it('says nothing once the rollout is done', () => {
    expect(rolloutLabel({})).toBeNull()
  })
  it('names a rollout in progress', () => {
    expect(rolloutLabel({ rollout_reason: 'InProgress' })).toBe('Rolling out')
  })
  it('names a stalled one', () => {
    for (const reason of ['Unschedulable', 'QuotaExceeded', 'NotBecomingReady', 'DeadlineExceeded']) {
      expect(rolloutLabel({ rollout_reason: reason })).toBe('Rollout waiting')
    }
  })
})
