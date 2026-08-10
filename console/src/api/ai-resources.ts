import client from './client'

export async function streamOptimisation(
  project: string,
  app: string,
  onChunk: (content: string) => void,
  onDone: () => void,
  onError: (error: string) => void,
): Promise<void> {
  const token = localStorage.getItem('kipper_token')
  const baseURL = client.defaults.baseURL || ''

  const response = await fetch(`${baseURL}/projects/${project}/apps/${app}/optimise`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'Authorization': `Bearer ${token}`,
    },
  })

  if (!response.ok) {
    const body = await response.json().catch(() => ({ error: 'unknown error' }))
    onError(body.error || `HTTP ${response.status}`)
    return
  }

  const reader = response.body?.getReader()
  if (!reader) {
    onError('streaming not supported')
    return
  }

  const decoder = new TextDecoder()
  let buffer = ''

  while (true) {
    const { done, value } = await reader.read()
    if (done) break

    buffer += decoder.decode(value, { stream: true })
    const lines = buffer.split('\n')
    buffer = lines.pop() || ''

    for (const line of lines) {
      if (!line.startsWith('data: ')) continue
      const data = line.slice(6)

      try {
        const parsed = JSON.parse(data)
        if (parsed.done) { onDone(); return }
        if (parsed.error) { onError(parsed.error); return }
        if (parsed.content) { onChunk(parsed.content) }
      } catch {
        // skip
      }
    }
  }

  onDone()
}
