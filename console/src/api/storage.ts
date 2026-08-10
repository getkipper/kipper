import client from './client'

export interface Bucket {
  name: string
  created_at: string
}

export interface StorageObject {
  key: string
  size: number
  last_modified: string
  is_dir: boolean
  is_public?: boolean
}

export interface ObjectsListResponse {
  objects: StorageObject[]
  prefix: string
  bucket: string
}

export interface ShareLink {
  url: string
  expires: string
}

export async function fetchBuckets(service: string, namespace: string): Promise<Bucket[]> {
  const params = new URLSearchParams({ namespace })
  const { data } = await client.get<Bucket[]>(`/storage/${service}/buckets?${params.toString()}`)
  return data || []
}

export async function createBucket(service: string, namespace: string, name: string): Promise<void> {
  const params = new URLSearchParams({ namespace })
  await client.post(`/storage/${service}/buckets?${params.toString()}`, { name })
}

export async function fetchObjects(
  service: string,
  namespace: string,
  bucket: string,
  prefix: string = '',
): Promise<ObjectsListResponse> {
  const params = new URLSearchParams({ namespace, bucket })
  if (prefix) params.set('prefix', prefix)
  const { data } = await client.get<ObjectsListResponse>(
    `/storage/${service}/objects?${params.toString()}`,
  )
  return data
}

export async function uploadFile(
  service: string,
  namespace: string,
  bucket: string,
  key: string,
  file: File,
): Promise<void> {
  const params = new URLSearchParams({ namespace, bucket, key })
  const form = new FormData()
  form.append('file', file)
  await client.post(`/storage/${service}/upload?${params.toString()}`, form, {
    headers: { 'Content-Type': 'multipart/form-data' },
  })
}

export function downloadURL(service: string, namespace: string, bucket: string, key: string): string {
  const base = client.defaults.baseURL || '/api/v1'
  const params = new URLSearchParams({ namespace, bucket, key })
  return `${base}/storage/${service}/download?${params.toString()}`
}

export async function downloadFile(
  service: string,
  namespace: string,
  bucket: string,
  key: string,
): Promise<void> {
  const token = localStorage.getItem('kipper_token')
  const url = downloadURL(service, namespace, bucket, key)
  const response = await fetch(url, {
    headers: { Authorization: `Bearer ${token}` },
  })
  if (!response.ok) throw new Error(`download failed: ${response.status}`)

  const blob = await response.blob()
  const filename = key.split('/').pop() || key
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = filename
  a.click()
  URL.revokeObjectURL(a.href)
}

export async function createFolder(
  service: string,
  namespace: string,
  bucket: string,
  prefix: string,
): Promise<void> {
  const params = new URLSearchParams({ namespace, bucket, prefix })
  await client.post(`/storage/${service}/folder?${params.toString()}`)
}

export async function deleteObject(
  service: string,
  namespace: string,
  bucket: string,
  key: string,
): Promise<void> {
  const params = new URLSearchParams({ namespace, bucket, key })
  await client.delete(`/storage/${service}/objects?${params.toString()}`)
}

export async function makePublic(
  service: string,
  namespace: string,
  bucket: string,
  key: string,
): Promise<{ url: string }> {
  const params = new URLSearchParams({ namespace, bucket, key })
  const { data } = await client.put<{ url: string }>(`/storage/${service}/public?${params.toString()}`)
  return data
}

export async function makePrivate(
  service: string,
  namespace: string,
  bucket: string,
  key: string,
): Promise<void> {
  const params = new URLSearchParams({ namespace, bucket, key })
  await client.delete(`/storage/${service}/public?${params.toString()}`)
}

export async function shareObject(
  service: string,
  namespace: string,
  bucket: string,
  key: string,
  expires: string = '24h',
): Promise<ShareLink> {
  const params = new URLSearchParams({ namespace, bucket, key, expires })
  const { data } = await client.post<ShareLink>(
    `/storage/${service}/share?${params.toString()}`,
  )
  return data
}
