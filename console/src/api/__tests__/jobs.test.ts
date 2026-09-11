import { beforeEach, describe, expect, it, vi } from 'vitest'

import client from '../client'
import { fetchJobHistory, triggerJob, fetchJobResources, updateJobResources } from '../jobs'

// Every other job test mocks this module, so the paths themselves are asserted
// nowhere: a typo in one of them would ship with the whole suite green.
vi.mock('../client', () => ({
  default: {
    get: vi.fn().mockResolvedValue({ data: {} }),
    post: vi.fn().mockResolvedValue({ data: {} }),
    put: vi.fn().mockResolvedValue({ data: {} }),
  },
}))

const NS = 'shop-prod'
const JOB = 'nightly-cleanup'

beforeEach(() => {
  vi.clearAllMocks()
})

describe('the job routes name the project a job belongs to', () => {
  it('reads history under the project', async () => {
    await fetchJobHistory(NS, JOB)
    expect(client.get).toHaveBeenCalledWith(`/projects/${NS}/jobs/${JOB}/history`)
  })

  it('triggers under the project', async () => {
    await triggerJob(NS, JOB)
    expect(client.post).toHaveBeenCalledWith(`/projects/${NS}/jobs/${JOB}/trigger`)
  })

  it('reads resources under the project', async () => {
    await fetchJobResources(NS, JOB)
    expect(client.get).toHaveBeenCalledWith(`/projects/${NS}/jobs/${JOB}/resources`)
  })

  it('writes resources under the project', async () => {
    const limits = { memory_limit: '512Mi', cpu_limit: '500m' }
    await updateJobResources(NS, JOB, limits)
    expect(client.put).toHaveBeenCalledWith(`/projects/${NS}/jobs/${JOB}/resources`, limits)
  })

  it('does not reach for the bare-name route when the namespaced one is absent', async () => {
    // Against an older console-api the namespaced routes do not exist. Retrying
    // the bare name there would land on the first-match resolution this whole
    // change removes, so the call fails and nothing else is sent.
    vi.mocked(client.post).mockRejectedValueOnce({ response: { status: 404, data: '404 page not found' } })

    await expect(triggerJob(NS, JOB)).rejects.toBeTruthy()
    expect(client.post).toHaveBeenCalledTimes(1)
    expect(client.get).not.toHaveBeenCalled()
  })

  it('escapes a name that would otherwise change the path', async () => {
    await triggerJob(NS, 'a/b')
    expect(client.post).toHaveBeenCalledWith(`/projects/${NS}/jobs/a%2Fb/trigger`)
  })
})
