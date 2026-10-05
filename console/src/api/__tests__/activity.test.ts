import { beforeEach, describe, expect, it, vi } from 'vitest'

import client from '../client'
import { fetchAppActivity } from '../activity'

vi.mock('../client', () => ({
  default: {
    get: vi.fn().mockResolvedValue({ data: { available: false, changes: [], changes_detailed: 0, degraded: [] } }),
  },
}))

beforeEach(() => {
  vi.clearAllMocks()
})

describe('fetchAppActivity', () => {
  it('reads the range under the project and app', async () => {
    const got = await fetchAppActivity('shop-prod', 'web', '6h')
    expect(client.get).toHaveBeenCalledWith('/projects/shop-prod/apps/web/activity', { params: { range: '6h' } })
    expect(got.available).toBe(false)
  })
})
