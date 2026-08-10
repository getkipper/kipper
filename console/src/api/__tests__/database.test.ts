import { describe, it, expect, vi, beforeEach } from 'vitest'
import {
  fetchDBDatabases,
  fetchDBSchema,
  fetchDBRows,
  runDBQuery,
  alterDBTable,
} from '../database'
import client from '../client'

vi.mock('../client', () => ({
  default: {
    get: vi.fn(),
    post: vi.fn(),
    patch: vi.fn(),
    delete: vi.fn(),
  },
}))

const mockedClient = vi.mocked(client) as unknown as {
  get: ReturnType<typeof vi.fn>
  post: ReturnType<typeof vi.fn>
  patch: ReturnType<typeof vi.fn>
  delete: ReturnType<typeof vi.fn>
}

beforeEach(() => {
  vi.resetAllMocks()
  mockedClient.get.mockResolvedValue({ data: [] })
  mockedClient.post.mockResolvedValue({ data: {} })
  mockedClient.patch.mockResolvedValue({ data: {} })
  mockedClient.delete.mockResolvedValue({ data: {} })
})

describe('database API', () => {
  it('threads the database query param through fetchDBSchema', async () => {
    await fetchDBSchema('db', 'blog-test', 'domain_service_test')
    expect(mockedClient.get).toHaveBeenCalledTimes(1)
    const url = mockedClient.get.mock.calls[0][0] as string
    expect(url).toContain('database=domain_service_test')
    expect(url).toContain('namespace=blog-test')
  })

  it('omits the database param when none is provided', async () => {
    await fetchDBSchema('db', 'ns')
    const url = mockedClient.get.mock.calls[0][0] as string
    expect(url).not.toContain('database=')
  })

  it('lists databases via /db/databases', async () => {
    mockedClient.get.mockResolvedValue({
      data: [
        { name: 'app', default: true },
        { name: 'domain_service_test', default: false },
      ],
    })
    const result = await fetchDBDatabases('db', 'ns')
    expect(mockedClient.get).toHaveBeenCalledWith(
      expect.stringMatching(/^\/services\/db\/db\/databases\?/),
    )
    expect(result).toHaveLength(2)
    expect(result[0]).toEqual({ name: 'app', default: true })
  })

  it('sends database in the request body for runDBQuery', async () => {
    mockedClient.post.mockResolvedValue({ data: { rows_affected: 0, duration_ms: 1, truncated: false, sql: '' } })
    await runDBQuery('db', 'ns', { sql: 'SELECT 1', database: 'app' })
    expect(mockedClient.post).toHaveBeenCalledTimes(1)
    const body = mockedClient.post.mock.calls[0][1]
    expect(body).toEqual({ sql: 'SELECT 1', database: 'app' })
  })

  it('threads database through row endpoints', async () => {
    mockedClient.get.mockResolvedValue({
      data: { structure: { schema: 'public', name: 'users', columns: [], primary_key: [], foreign_keys: [], indexes: [] }, rows: [], total: 0, limit: 50, offset: 0, duration_ms: 1 },
    })
    await fetchDBRows('db', 'ns', 'public', 'users', { limit: 10 }, 'app')
    const url = mockedClient.get.mock.calls[0][0] as string
    expect(url).toContain('database=app')
    expect(url).toContain('limit=10')
  })

  it('threads database through alterDBTable', async () => {
    mockedClient.patch.mockResolvedValue({ data: { ddl: [] } })
    await alterDBTable('db', 'ns', 'public', 'users', [{ op: 'drop_column', name: 'legacy' }], 'app')
    const url = mockedClient.patch.mock.calls[0][0] as string
    expect(url).toContain('database=app')
  })
})
