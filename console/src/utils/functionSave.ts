export interface SavePart {
  label: string
  run: () => Promise<unknown>
  skip?: boolean
  blocked?: string
}

export interface SaveResult {
  saved: string[]
  failed?: { label: string; error: string }
  notTried: string[]
}

export function sameMap(a: Record<string, string>, b: Record<string, string>): boolean {
  const keys = Object.keys(a)
  return keys.length === Object.keys(b).length && keys.every((k) => b[k] === a[k])
}

// Serialize this save's requests to avoid conflicts on the shared Function resource.
// Stop at the first failure so the user can see which parts still need saving.
export async function saveInOrder(parts: SavePart[]): Promise<SaveResult> {
  const result: SaveResult = { saved: [], failed: undefined, notTried: [] }
  const todo = parts.filter((p) => !p.skip)
  for (let i = 0; i < todo.length; i++) {
    if (todo[i].blocked) {
      result.failed = { label: todo[i].label, error: todo[i].blocked as string }
      result.notTried = todo.slice(i + 1).map((p) => p.label)
      break
    }
    try {
      await todo[i].run()
      result.saved.push(todo[i].label)
    } catch (err) {
      result.failed = { label: todo[i].label, error: errorText(err) }
      result.notTried = todo.slice(i + 1).map((p) => p.label)
      break
    }
  }
  return result
}

export function saveSummary(result: SaveResult): string {
  const parts: string[] = []
  if (result.saved.length) parts.push(`Saved ${result.saved.join(', ')}.`)
  if (result.failed) parts.push(`Could not save ${result.failed.label}: ${result.failed.error}.`)
  if (result.notTried.length) parts.push(`Not saved: ${result.notTried.join(', ')}.`)
  return parts.join(' ')
}

function errorText(err: unknown): string {
  const response = (err as { response?: { data?: { error?: string } } })?.response
  if (response?.data?.error) return response.data.error
  return err instanceof Error ? err.message : String(err)
}
