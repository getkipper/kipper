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

/**
 * The runs of one job.
 *
 * A job name is unique inside a namespace and nowhere wider, so every call here
 * names the namespace. The cluster-wide list is what supplies it: each row
 * carries the namespace it runs in.
 */
export async function fetchJobHistory(namespace: string, name: string): Promise<Job[]> {
  const { data } = await client.get<Job[]>(`/projects/${encodeURIComponent(namespace)}/jobs/${encodeURIComponent(name)}/history`)
  return data
}

export async function triggerJob(namespace: string, name: string): Promise<void> {
  await client.post(`/projects/${encodeURIComponent(namespace)}/jobs/${encodeURIComponent(name)}/trigger`)
}

export interface JobResources {
  memory_limit: string
  memory_request: string
  cpu_limit: string
  cpu_request: string
}

export async function fetchJobResources(namespace: string, name: string): Promise<JobResources> {
  const { data } = await client.get<JobResources>(`/projects/${encodeURIComponent(namespace)}/jobs/${encodeURIComponent(name)}/resources`)
  return data
}

export async function updateJobResources(namespace: string, name: string, resources: { memory_limit: string; cpu_limit: string }): Promise<void> {
  await client.put(`/projects/${encodeURIComponent(namespace)}/jobs/${encodeURIComponent(name)}/resources`, resources)
}
