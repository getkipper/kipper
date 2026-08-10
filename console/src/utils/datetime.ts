// Absolute date and time in a stable en-GB, 24-hour format, e.g.
// "7 Jul 2026, 14:09". Use this for displayed timestamps so the console reads
// consistently instead of following each browser's locale default.
export function formatDateTime(value: string | number | Date): string {
  const d = new Date(value)
  if (isNaN(d.getTime())) return ''
  return d.toLocaleString('en-GB', {
    day: 'numeric',
    month: 'short',
    year: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  })
}

// Date only, same convention, e.g. "7 Jul 2026".
export function formatDate(value: string | number | Date): string {
  const d = new Date(value)
  if (isNaN(d.getTime())) return ''
  return d.toLocaleDateString('en-GB', {
    day: 'numeric',
    month: 'short',
    year: 'numeric',
  })
}
