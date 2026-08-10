export function severityColor(severity: string): string {
  switch (severity) {
    case 'critical': return 'bg-red-500'
    case 'warning': return 'bg-amber-500'
    default: return 'bg-emerald-500'
  }
}

export function severityLabel(severity: string): string {
  switch (severity) {
    case 'critical': return 'Critical'
    case 'warning': return 'Warning'
    default: return 'Info'
  }
}

export function relativeTime(iso: string): string {
  const now = Date.now()
  const then = new Date(iso).getTime()
  const diffSeconds = Math.floor((now - then) / 1000)

  if (diffSeconds < 60) return 'just now'
  const diffMinutes = Math.floor(diffSeconds / 60)
  if (diffMinutes < 60) return `${diffMinutes} min ago`
  const diffHours = Math.floor(diffMinutes / 60)
  if (diffHours < 24) return `${diffHours} hour${diffHours > 1 ? 's' : ''} ago`
  const diffDays = Math.floor(diffHours / 24)
  return `${diffDays} day${diffDays > 1 ? 's' : ''} ago`
}
