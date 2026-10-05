import client from './client'

export const ACTIVITY_RANGES = ['1h', '6h', '24h', '3d'] as const
export type ActivityRange = (typeof ACTIVITY_RANGES)[number]

/**
 * `no_route`: no current route or ownership history in the range.
 * `not_attributed`: no full input window with exclusive route-name ownership.
 * `unavailable`: attribution, query or scrape data could not be read.
 */
export type TrafficState = 'available' | 'no_route' | 'not_attributed' | 'unavailable'

/** What the data shows about a change of the pod count. Only `autoscaler` names an actor. */
export type ChangeCause = 'to_zero' | 'from_zero' | 'bounds_change' | 'autoscaler' | 'while_autoscaling' | 'unknown'

/** One value per timestamp; null marks missing, invalid or unattributed data. */
export type Series = (number | null)[]

export interface RequestsByClass {
  ok: Series
  redirect: Series
  client_error: Series
  aborted: Series
  server_error: Series
}

/** Historical targets and bounds; null marks absent settings or missing data. */
export interface ActivityPolicy {
  cpu_target_pct: Series
  memory_target_pct: Series
  min: Series
  max: Series
}

/** A metric in the window before a change, as a share of its request. */
export interface AroundMetric {
  peak_pct: number | null
  avg_pct: number | null
  target_pct: number | null
}

/** Estimated request counts in the window before a change. */
export interface AroundRequests {
  total: number
  not_found: number
  aborted: number
  server_error: number
  other: number
}

export interface ChangeAround {
  window_seconds: number
  cpu: AroundMetric | null
  memory: AroundMetric | null
  requests: AroundRequests | null
}

export interface ReplicaChange {
  /** Unix seconds of the first 30s evaluation showing the new count. */
  time: number
  from: number
  to: number
  cause: ChangeCause
  /** Whether the app had an autoscaler at the 30s samples on both sides. */
  autoscaled: boolean
  around: ChangeAround | null
  /** Why `around` is null although the change was detailed. */
  around_unavailable?: string
}

/** Activity response. When unavailable, `reason` explains why chart data is absent. */
export interface AppActivity {
  available: boolean
  reason?: string
  range?: ActivityRange
  step_seconds?: number
  /** Unix seconds, one per chart point. */
  timestamps?: number[]
  traffic?: TrafficState
  requests_per_min?: RequestsByClass
  requests_peak_per_min?: Series
  cpu_pct_of_request?: Series
  cpu_peak_pct_of_request?: Series
  cpu_used_millis?: Series
  cpu_requested_millis?: Series
  memory_pct_of_request?: Series
  memory_used_bytes?: Series
  memory_requested_bytes?: Series
  replicas?: Series
  policy?: ActivityPolicy
  changes: ReplicaChange[]
  /** Changes selected for detail lookup, including failed or unavailable lookups. */
  changes_detailed: number
  /** Failed or timed-out query groups, including `around` detail lookups. */
  degraded: string[]
}

export async function fetchAppActivity(project: string, app: string, range: ActivityRange): Promise<AppActivity> {
  const { data } = await client.get<AppActivity>(`/projects/${project}/apps/${app}/activity`, { params: { range } })
  return data
}
