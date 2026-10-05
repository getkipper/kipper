import type { AppActivity, ChangeCause, ReplicaChange, Series } from '@/api/activity'
import type { AutoscaleConfig } from '@/api/apps'
import { formatCpu, formatMemory } from '@/utils/resources'

/** Kubernetes' default autoscaler tolerance; a cluster can change it. */
export const DEFAULT_TOLERANCE = 0.1

export type MarkerStyle = 'solid' | 'dashed' | 'dotted'

export interface MarkerLook {
  style: MarkerStyle
  /** A short label drawn next to the marker. */
  label?: string
}

/** Marker styles distinguish recorded actors, inferred context and zero transitions. */
export function markerLook(cause: ChangeCause): MarkerLook {
  switch (cause) {
    case 'autoscaler':
      return { style: 'solid' }
    case 'while_autoscaling':
      return { style: 'dashed' }
    case 'to_zero':
      return { style: 'solid', label: 'to 0' }
    case 'from_zero':
      return { style: 'solid', label: 'from 0' }
    case 'bounds_change':
      return { style: 'solid', label: 'bounds' }
    default:
      return { style: 'dotted' }
  }
}

function pods(n: number): string {
  return n === 1 ? '1 pod' : `${n} pods`
}

/** Describe the recorded context; only a matching event identifies the autoscaler as the actor. */
export function changeSummary(change: ReplicaChange): string {
  const move = `from ${pods(change.from)} to ${pods(change.to)}`
  switch (change.cause) {
    case 'autoscaler':
      return `The autoscaler changed the count ${move}.`
    case 'while_autoscaling':
      return `The count changed ${move} while autoscaling was on. No record says who changed it.`
    case 'bounds_change':
      return `The count changed ${move} when an autoscaling bound changed at the same time.`
    case 'to_zero':
      return change.autoscaled
        ? `The count went ${move}. Kipper takes an autoscaled app to 0 only by stopping it.`
        : `The count went ${move}.`
    case 'from_zero':
      return change.autoscaled
        ? `The count rose ${move}. An autoscaled app rises from 0 only when it is started.`
        : `The count rose ${move}.`
    default:
      return `The count changed ${move}. Autoscaling was not on at both sides of this change.`
  }
}

function pct(v: number): string {
  return `${Math.round(v)}%`
}

function minutes(seconds: number): string {
  const m = Math.round(seconds / 60)
  return m === 1 ? 'minute' : `${m} minutes`
}

function metricSentence(name: string, m: { peak_pct: number | null; avg_pct: number | null; target_pct: number | null }): string | null {
  if (m.peak_pct === null) return null
  const extras: string[] = []
  if (m.target_pct !== null) extras.push(`target ${pct(m.target_pct)}`)
  if (m.avg_pct !== null) extras.push(`average ${pct(m.avg_pct)}`)
  return `${name} peaked at ${pct(m.peak_pct)} of its request${extras.length ? ` (${extras.join(', ')})` : ''}.`
}

function requestSentence(r: { total: number; not_found: number; aborted: number; server_error: number }): string {
  const total = Math.round(r.total)
  if (total === 0) return 'No requests.'
  const parts: string[] = []
  if (Math.round(r.aborted) > 0) parts.push(`${Math.round(r.aborted)} abandoned`)
  if (Math.round(r.not_found) > 0) parts.push(`${Math.round(r.not_found)} not found`)
  if (Math.round(r.server_error) > 0) parts.push(Math.round(r.server_error) === 1 ? '1 server error' : `${Math.round(r.server_error)} server errors`)
  const noun = total === 1 ? 'request' : 'requests'
  if (!parts.length) return `About ${total} ${noun}.`
  const last = parts.pop()
  const list = parts.length ? `${parts.join(', ')} and ${last}` : last
  return `About ${total} ${noun}, of which ${list}.`
}

/**
 * Summarize pre-change measurements or explain why they are unavailable. Scale-outs
 * below the estimated threshold get a note about independent HPA sampling.
 */
export function changeObservation(change: ReplicaChange): string | null {
  const a = change.around
  if (!a) return change.around_unavailable ?? null
  const sentences: string[] = []
  const cpu = a.cpu ? metricSentence('CPU', a.cpu) : null
  const memory = a.memory ? metricSentence('Memory', a.memory) : null
  if (cpu) sentences.push(cpu)
  if (memory) sentences.push(memory)
  if (a.requests) sentences.push(requestSentence(a.requests))
  if (!sentences.length) return null
  let text = `In the ${minutes(a.window_seconds)} before: ${sentences.join(' ')}`
  const measured = [a.cpu, a.memory].filter(m => m && m.peak_pct !== null && m.target_pct !== null)
  const scaleOut = change.to > change.from
  if (scaleOut && measured.length && measured.every(m => m!.peak_pct! < m!.target_pct! * (1 + DEFAULT_TOLERANCE))) {
    text += ' The autoscaler takes its own samples, which can catch a short spike that these figures smooth out.'
  }
  return text
}

function latest(series: Series | undefined): number | null {
  for (let i = (series?.length ?? 0) - 1; i >= 0; i--) {
    const v = series![i]
    if (v !== null) return v
  }
  return null
}

/**
 * Estimate scale-out thresholds from current targets and the latest summed requests.
 * Use the default tolerance; hide the guide when autoscaling is disabled or the app is stopped.
 */
export function targetGuide(config: AutoscaleConfig, activity: AppActivity | null, atMax: boolean): string[] {
  if (!config.enabled || config.stopped === true) return []
  const lines: string[] = []
  const tolerance = `With Kubernetes' default ${pct(DEFAULT_TOLERANCE * 100)} tolerance`
  const metrics: { name: string; target: number; requested: number | null; format: (v: number) => string }[] = []
  if (config.cpu_target > 0) metrics.push({ name: 'CPU', target: config.cpu_target, requested: latest(activity?.cpu_requested_millis), format: formatCpu })
  if (config.memory_target > 0) metrics.push({ name: 'Memory', target: config.memory_target, requested: latest(activity?.memory_requested_bytes), format: formatMemory })
  for (const m of metrics) {
    const above = m.target * (1 + DEFAULT_TOLERANCE)
    const absolute = m.requested !== null && m.requested > 0 ? ` (${m.format((m.requested * above) / 100)} of ${m.format(m.requested)})` : ''
    const end = atMax ? `, but it is at the maximum of ${config.max_replicas} and adds no more.` : `, up to the maximum of ${config.max_replicas}.`
    lines.push(`Target: ${m.name} at ${pct(m.target)} of request. ${tolerance}, the autoscaler adds pods above about ${pct(above)}${absolute}${end}`)
  }
  if (metrics.length > 1) lines.push('Either metric can add a pod.')
  return lines
}

/** Explain missing traffic data; return null when no explanation applies. */
export function trafficNote(activity: AppActivity): string | null {
  switch (activity.traffic) {
    case 'no_route':
      return 'This app has no route, so there is no request traffic to show.'
    case 'not_attributed':
      return 'Requests are shown only while this app alone held its route name, which it did not long enough in this range.'
    case 'unavailable':
      return 'The request figures could not be read.'
    default:
      return null
  }
}

/** Report gaps in available traffic, ignoring missing points in the last 90 seconds. */
export function trafficPartial(activity: AppActivity): boolean {
  if (activity.traffic !== 'available' || !activity.requests_per_min) return false
  const ok = activity.requests_per_min.ok
  const ts = activity.timestamps ?? []
  // Allow time for route ownership checks before flagging recent gaps.
  const settled = (ts[ts.length - 1] ?? 0) - 90
  const covered = ok.filter((v, i) => v !== null || (ts[i] ?? 0) < settled)
  return covered.some(v => v !== null) && covered.some(v => v === null)
}
