import client from './client'

export interface FileEntry {
  name: string
  size: number
  permissions: string
  modified: string
  is_dir: boolean
}

export interface FileListResponse {
  path: string
  entries: FileEntry[]
  pod: string
  pod_count: number
}

export async function listFiles(
  project: string,
  app: string,
  path: string = '/',
): Promise<FileListResponse> {
  const params = new URLSearchParams({ path })
  const { data } = await client.get<FileListResponse>(
    `/projects/${project}/apps/${app}/files?${params.toString()}`,
  )
  return data
}

export async function readFileContent(
  project: string,
  app: string,
  path: string,
): Promise<string> {
  const params = new URLSearchParams({ path })
  const { data } = await client.get<string>(
    `/projects/${project}/apps/${app}/files/content?${params.toString()}`,
    { responseType: 'text', transformResponse: [(d: string) => d] },
  )
  return data
}

export async function downloadFile(
  project: string,
  app: string,
  path: string,
): Promise<void> {
  const token = localStorage.getItem('kipper_token')
  const base = client.defaults.baseURL || '/api/v1'
  const params = new URLSearchParams({ path })
  const url = `${base}/projects/${project}/apps/${app}/files/download?${params.toString()}`

  const response = await fetch(url, {
    headers: { Authorization: `Bearer ${token}` },
  })
  if (!response.ok) throw new Error(`download failed: ${response.status}`)

  const blob = await response.blob()
  const filename = path.split('/').pop() || path
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = filename
  a.click()
  URL.revokeObjectURL(a.href)
}

export async function saveFileContent(
  project: string,
  app: string,
  path: string,
  content: string,
): Promise<void> {
  const params = new URLSearchParams({ path })
  await client.put(
    `/projects/${project}/apps/${app}/files/content?${params.toString()}`,
    content,
    { headers: { 'Content-Type': 'text/plain' } },
  )
}

export async function uploadFile(
  project: string,
  app: string,
  dirPath: string,
  file: File,
): Promise<void> {
  const params = new URLSearchParams({ path: dirPath })
  const formData = new FormData()
  formData.append('file', file)
  await client.post(
    `/projects/${project}/apps/${app}/files/upload?${params.toString()}`,
    formData,
  )
}

export function formatFileSize(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  const size = bytes / Math.pow(1024, i)
  return `${size.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}
