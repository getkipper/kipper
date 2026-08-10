import client from './client'

export interface ChatMessage {
  role: 'user' | 'assistant'
  content: string
}

export interface ChatBinding {
  service: string
  type: string
  env: string[]
}

// Database-mode context. Names + types only — no row data.
export interface ChatDBColumn {
  name: string
  type: string
  nullable: boolean
  pk?: boolean
}

export interface ChatDBIndex {
  name: string
  columns: string[]
  unique?: boolean
}

export interface ChatDBForeignKey {
  column: string
  ref_schema: string
  ref_table: string
  ref_column: string
}

export interface ChatDBTable {
  schema: string
  name: string
  columns: ChatDBColumn[]
  indexes?: ChatDBIndex[]
  foreign_keys?: ChatDBForeignKey[]
}

export interface ChatDBSchema {
  dialect: string  // "postgres" | "mysql"
  tables: ChatDBTable[]
}

export interface ChatRequest {
  messages: ChatMessage[]
  code: string
  runtime: string
  error?: string
  // mode picks the system prompt: "code" (default) or "db".
  mode?: 'code' | 'db'
  // Function environment context (mode=code).
  bindings?: ChatBinding[]
  env_keys?: string[]
  secret_keys?: string[]
  dependencies?: Record<string, string>
  // Database context (mode=db).
  db_schema?: ChatDBSchema
  sql?: string
}

export async function streamChat(
  req: ChatRequest,
  onChunk: (content: string) => void,
  onDone: () => void,
  onError: (error: string) => void,
): Promise<void> {
  const token = localStorage.getItem('kipper_token')
  const baseURL = client.defaults.baseURL || ''

  const response = await fetch(`${baseURL}/ai/chat`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'Authorization': `Bearer ${token}`,
    },
    body: JSON.stringify(req),
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
        if (parsed.done) {
          onDone()
          return
        }
        if (parsed.error) {
          onError(parsed.error)
          return
        }
        if (parsed.content) {
          onChunk(parsed.content)
        }
      } catch {
        // skip malformed lines
      }
    }
  }

  onDone()
}
