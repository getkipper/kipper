// Tiny human-readable cron descriptions for the common shapes.
// Avoids a runtime dep (cronstrue/cron-parser); falls back to a plain
// "Custom schedule" label for anything unusual.

export interface CronDescription {
  text: string
  // nextRuns are the next ISO timestamps the schedule will fire.
  // Empty when we can't compute it cheaply.
  nextRuns: string[]
}

const PRESETS: Array<{ pattern: RegExp; text: (m: RegExpMatchArray) => string }> = [
  {
    pattern: /^\* \* \* \* \*$/,
    text: () => 'Every minute',
  },
  {
    pattern: /^\*\/(\d+) \* \* \* \*$/,
    text: (m) => `Every ${m[1]} minutes`,
  },
  {
    pattern: /^0 \* \* \* \*$/,
    text: () => 'Every hour, on the hour',
  },
  {
    pattern: /^0 \*\/(\d+) \* \* \*$/,
    text: (m) => `Every ${m[1]} hours`,
  },
  {
    pattern: /^(\d+) (\d+) \* \* \*$/,
    text: (m) => `Every day at ${pad(m[2])}:${pad(m[1])} UTC`,
  },
  {
    pattern: /^(\d+) (\d+) \* \* (\d+)$/,
    text: (m) => `Every ${dayName(Number(m[3]))} at ${pad(m[2])}:${pad(m[1])} UTC`,
  },
  {
    pattern: /^(\d+) (\d+) (\d+) \* \*$/,
    text: (m) => `Day ${m[3]} of every month at ${pad(m[2])}:${pad(m[1])} UTC`,
  },
]

const COMMON_PRESETS: Array<{ label: string; expr: string }> = [
  { label: 'Every minute (testing)', expr: '* * * * *' },
  { label: 'Every 5 minutes', expr: '*/5 * * * *' },
  { label: 'Every 15 minutes', expr: '*/15 * * * *' },
  { label: 'Every hour, on the hour', expr: '0 * * * *' },
  { label: 'Every day at midnight UTC', expr: '0 0 * * *' },
  { label: 'Every day at 02:00 UTC', expr: '0 2 * * *' },
  { label: 'Every Monday at 09:00 UTC', expr: '0 9 * * 1' },
  { label: 'First of the month at 03:00 UTC', expr: '0 3 1 * *' },
]

export function commonCronPresets(): Array<{ label: string; expr: string }> {
  return COMMON_PRESETS
}

export function describeCron(expr: string): CronDescription {
  const trimmed = expr.trim()
  if (!trimmed) return { text: '', nextRuns: [] }

  for (const preset of PRESETS) {
    const match = trimmed.match(preset.pattern)
    if (match) {
      return {
        text: preset.text(match),
        nextRuns: computeNextRuns(trimmed, 5),
      }
    }
  }
  return {
    text: 'Custom schedule',
    nextRuns: computeNextRuns(trimmed, 5),
  }
}

// computeNextRuns produces the next N firing times by iterating minute by
// minute and matching the cron expression. Cheap, correct for the patterns
// we expose, and good enough for a UI preview.
export function computeNextRuns(expr: string, count: number): string[] {
  const fields = expr.trim().split(/\s+/)
  if (fields.length !== 5) return []
  const matchers = fields.map(parseField)
  if (matchers.some((m) => !m)) return []

  const out: string[] = []
  const now = new Date()
  // Start from the next whole minute.
  const cursor = new Date(Date.UTC(
    now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate(),
    now.getUTCHours(), now.getUTCMinutes() + 1, 0, 0,
  ))

  // Hard cap iterations to a year of minutes so a malformed pattern never hangs.
  const maxIterations = 60 * 24 * 366
  for (let i = 0; i < maxIterations && out.length < count; i++) {
    const minute = cursor.getUTCMinutes()
    const hour = cursor.getUTCHours()
    const day = cursor.getUTCDate()
    const month = cursor.getUTCMonth() + 1
    const weekday = cursor.getUTCDay()
    const [m0, m1, m2, m3, m4] = matchers as Set<number>[]
    if (m0.has(minute) && m1.has(hour) && m2.has(day) && m3.has(month) && m4.has(weekday)) {
      out.push(cursor.toISOString())
    }
    cursor.setUTCMinutes(cursor.getUTCMinutes() + 1)
  }
  return out
}

function parseField(field: string): Set<number> | null {
  // Supports: *, */N, A, A-B, A,B,C
  const ranges: Array<[number, number]> = []
  for (const part of field.split(',')) {
    const m = part.match(/^\*(?:\/(\d+))?$/)
    if (m) {
      ranges.push([0, 999])
      // Step matching is handled below via modulo.
      continue
    }
    const r = part.match(/^(\d+)(?:-(\d+))?$/)
    if (!r) return null
    const a = Number(r[1])
    const b = r[2] !== undefined ? Number(r[2]) : a
    if (Number.isNaN(a) || Number.isNaN(b)) return null
    ranges.push([a, b])
  }
  // For correctness with steps we evaluate per-value.
  const stepMatch = field.match(/\*\/(\d+)/)
  const step = stepMatch ? Number(stepMatch[1]) : 1
  const set = new Set<number>()
  for (const [a, b] of ranges) {
    for (let v = a; v <= Math.min(b, 999); v++) {
      if ((v - a) % step === 0) set.add(v)
    }
  }
  return set
}

function pad(s: string | number): string {
  return String(s).padStart(2, '0')
}

function dayName(n: number): string {
  return ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'][n % 7]
}
