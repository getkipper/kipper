import { describe, it, expect, vi, beforeEach } from 'vitest'
import { setUserMonitoring } from '../users'
import client from '../client'

vi.mock('../client', () => ({
  default: {
    put: vi.fn(),
    delete: vi.fn(),
  },
}))

const mockedClient = vi.mocked(client) as unknown as {
  put: ReturnType<typeof vi.fn>
  delete: ReturnType<typeof vi.fn>
}

beforeEach(() => {
  vi.resetAllMocks()
  mockedClient.put.mockResolvedValue({})
  mockedClient.delete.mockResolvedValue({})
})

describe('users API', () => {
  it('setUserMonitoring grants with PUT', async () => {
    await setUserMonitoring('dev+ops@test.com', true)
    expect(mockedClient.put).toHaveBeenCalledWith('/users/dev%2Bops%40test.com/monitoring')
    expect(mockedClient.delete).not.toHaveBeenCalled()
  })

  it('setUserMonitoring revokes with DELETE', async () => {
    await setUserMonitoring('dev@test.com', false)
    expect(mockedClient.delete).toHaveBeenCalledWith('/users/dev%40test.com/monitoring')
    expect(mockedClient.put).not.toHaveBeenCalled()
  })
})
