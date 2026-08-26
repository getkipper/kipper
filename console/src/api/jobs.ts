import client from './client'

export interface Job {
  name: string
  type: string
  /** The namespace it runs in, which names the project whose capabilities gate it. */
  namespace: string
  schedule: string
  last: string
  status: string
  image: string
}

export interface CreateJobPayload {
  name: string
  image: string
  command?: string
  schedule?: string
  namespace: string
  memory?: string
  cpu?: string
}

export async function createJob(payload: CreateJobPayload): Promise<void> {
  await client.post('/jobs', payload)
}

export async function fetchJobs(): Promise<Job[]> {
  const { data } = await client.get<Job[]>('/jobs')
  return data || []
}

export async function fetchJobHistory(name: string): Promise<Job[]> {
  const { data } = await client.get<Job[]>(`/jobs/${name}/history`)
  return data
}

export async function triggerJob(name: string): Promise<void> {
  await client.post(`/jobs/${name}/trigger`)
}

export interface JobResources {
  memory_limit: string
  memory_request: string
  cpu_limit: string
  cpu_request: string
}

export async function fetchJobResources(name: string): Promise<JobResources> {
  const { data } = await client.get<JobResources>(`/jobs/${name}/resources`)
  return data
}

export async function updateJobResources(name: string, resources: { memory_limit: string; cpu_limit: string }): Promise<void> {
  await client.put(`/jobs/${name}/resources`, resources)
}
