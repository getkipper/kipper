import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fetchRabbitMQVhosts } from '../services'
import client from '../client'

vi.mock('../client', () => ({
  default: {
    get: vi.fn(),
  },
}))

const mockedClient = vi.mocked(client) as unknown as {
  get: ReturnType<typeof vi.fn>
}

beforeEach(() => {
  vi.resetAllMocks()
  mockedClient.get.mockResolvedValue({ data: [] })
})

describe('services API', () => {
  it('fetchRabbitMQVhosts hits the rabbitmq vhost endpoint with namespace', async () => {
    mockedClient.get.mockResolvedValue({
      data: [
        { name: '/', default: true },
        { name: 'orders', default: false },
      ],
    })
    const result = await fetchRabbitMQVhosts('rabbit', 'blog-test')
    expect(mockedClient.get).toHaveBeenCalledTimes(1)
    const url = mockedClient.get.mock.calls[0][0] as string
    expect(url).toContain('/services/rabbit/rabbitmq/vhosts')
    expect(url).toContain('namespace=blog-test')
    expect(result).toEqual([
      { name: '/', default: true },
      { name: 'orders', default: false },
    ])
  })

  it('fetchRabbitMQVhosts returns [] on a null body so callers can iterate safely', async () => {
    mockedClient.get.mockResolvedValue({ data: null })
    const result = await fetchRabbitMQVhosts('rabbit', 'ns')
    expect(result).toEqual([])
  })
})
