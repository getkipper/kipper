import type { AutoscaleConfig, AutoscalerCondition, AutoscaleRequest, ScaleActivity } from '@/api/apps'

export type ScalingPolicy = 'none' | 'tracking'

/** The capacity panel's form. An empty bound is null, a target of 0 is unused. */
export interface CapacityForm {
  policy: ScalingPolicy
  desired: number | null
  min: number | null
  max: number | null
  cpu: number
  memory: number
}

export interface CapacityCheck {
  error?: string
  /** Set when desired is above the maximum, so the panel can offer to raise it. */
  raiseMaxTo?: number
}

export type CapacitySaveStep =
  | { kind: 'put'; body: AutoscaleRequest }
  | { kind: 'delete' }
  | { kind: 'scale'; replicas: number }

export type CapacitySavePlan =
  | { steps: CapacitySaveStep[]; atomic: boolean }
  | { unsupported: string }

export interface CapacityBadge {
  key: string
  label: string
  tone: 'warning' | 'danger' | 'info'
  title: string
}

function isCount(n: number | null, least: number): n is number {
  return n !== null && Number.isInteger(n) && n >= least
}

/**
 * Validates edits before saving. An unchanged desired count is left for the
 * server to resolve within bounds; an explicitly entered zero directs the
 * user to Stop app.
 */
export function checkCapacity(form: CapacityForm, saved?: CapacityForm): CapacityCheck {
  if (form.min !== null && !isCount(form.min, 1)) return { error: 'The minimum must be a whole number of at least 1.' }
  if (form.max !== null && !isCount(form.max, 1)) return { error: 'The maximum must be a whole number of at least 1.' }
  if (form.min !== null && form.max === null) return { error: 'Set a maximum with the minimum, or clear both to remove the bounds.' }
  if (form.min !== null && form.max !== null && form.min > form.max) {
    return { error: `The minimum (${form.min}) must not exceed the maximum (${form.max}).` }
  }
  const lo = form.min ?? 1
  const hi = form.max

  if (form.policy === 'tracking') {
    if (hi === null) return { error: 'Target tracking needs a maximum.' }
    if (!isCount(form.cpu, 0) || !isCount(form.memory, 0)) return { error: 'Targets must be whole percentages of 0 or more.' }
    if (form.cpu === 0 && form.memory === 0) return { error: 'Set a CPU or memory target to use target tracking.' }
    return {}
  }

  if (saved !== undefined && form.desired === saved.desired) return {}
  if (!isCount(form.desired, 0)) return { error: 'The desired count must be a whole number of 0 or more.' }
  if (form.desired === 0) return { error: 'Desired cannot be 0. To run no pods, use Stop app at the top of the page.' }
  if (hi === null) return {}
  if (form.desired > hi) return { error: `Desired (${form.desired}) is above the maximum of ${hi}.`, raiseMaxTo: form.desired }
  if (form.desired < lo) return { error: `Desired (${form.desired}) is below the minimum of ${lo}.` }
  return {}
}

function boundsOrTargetsChanged(from: CapacityForm, to: CapacityForm): boolean {
  return from.min !== to.min || from.max !== to.max || from.cpu !== to.cpu || from.memory !== to.memory
}

function bodyBounds(form: CapacityForm): Pick<AutoscaleRequest, 'min_replicas' | 'max_replicas'> {
  if (form.max === null) return {}
  return { min_replicas: form.min ?? 1, max_replicas: form.max }
}

/**
 * Plans requests for a form validated by checkCapacity. A desired-only edit
 * uses /scale. Other edits use one PUT with capacity_api, or DELETE followed
 * by /scale on older APIs; unsupported bounds edits are reported separately.
 */
export function planCapacitySave(saved: CapacityForm, edited: CapacityForm, capacityApi: boolean): CapacitySavePlan {
  const desiredChanged = edited.desired !== saved.desired

  if (edited.policy === 'tracking') {
    const unchanged = saved.policy === 'tracking' && !boundsOrTargetsChanged(saved, edited)
    if (unchanged) return { steps: [], atomic: true }
    return {
      steps: [{ kind: 'put', body: { ...bodyBounds(edited), cpu_target: edited.cpu, memory_target: edited.memory } }],
      atomic: true,
    }
  }

  const switchingOff = saved.policy === 'tracking'
  const boundsChanged = boundsOrTargetsChanged(saved, edited)
  if (!switchingOff && !boundsChanged) {
    return { steps: desiredChanged ? [{ kind: 'scale', replicas: edited.desired as number }] : [], atomic: true }
  }

  if (capacityApi) {
    const body: AutoscaleRequest = { enabled: false, cpu_target: edited.cpu, memory_target: edited.memory, ...bodyBounds(edited) }
    if (desiredChanged) body.replicas = edited.desired as number
    return { steps: [{ kind: 'put', body }], atomic: true }
  }

  if (boundsChanged) {
    return { unsupported: 'This cluster runs an older console-api that stores bounds only together with target tracking. Save the desired count on its own, or turn target tracking on.' }
  }
  const steps: CapacitySaveStep[] = [{ kind: 'delete' }]
  if (desiredChanged) steps.push({ kind: 'scale', replicas: edited.desired as number })
  return { steps, atomic: steps.length === 1 }
}

/** Estimates the bounds adjustment from the loaded count for the save preview. */
export function boundsMove(saved: CapacityForm, plan: CapacitySavePlan): { from: number; to: number } | undefined {
  if (!('steps' in plan) || saved.desired === null) return undefined
  const put = plan.steps.find(step => step.kind === 'put')
  if (put?.kind !== 'put' || put.body.enabled !== false || put.body.replicas !== undefined) return undefined
  const { min_replicas: lo, max_replicas: hi } = put.body
  if (lo === undefined || hi === undefined) return undefined
  const to = Math.min(Math.max(saved.desired, lo), hi)
  return to === saved.desired ? undefined : { from: saved.desired, to }
}

// Labels for the App's AutoscalingReady reasons. Any other reason gets a
// general label with the reason in its title.
const notReadyLabels: Record<string, Pick<CapacityBadge, 'label' | 'tone'>> = {
  AutoscalerReconcileFailed: { label: 'Autoscaler not applied', tone: 'danger' },
  AutoscalerDeleteFailed: { label: 'Autoscaler not removed', tone: 'danger' },
  ReplicasOutsideBounds: { label: 'Desired outside bounds', tone: 'warning' },
}

function notReadyBadge(ready: AutoscalerCondition): CapacityBadge {
  if (ready.reason === 'InvalidPolicy') return { key: 'policy-invalid', label: 'Policy invalid', tone: 'danger', title: ready.message }
  const known = notReadyLabels[ready.reason]
  if (known) return { key: 'autoscaling-not-ready', ...known, title: ready.message }
  return { key: 'autoscaling-not-ready', label: 'Autoscaling not ready', tone: 'danger', title: `${ready.reason}: ${ready.message}` }
}

/**
 * Shows App readiness and quota problems before HPA conditions. HPA conditions
 * apply only while the policy is enabled; disabling it removes the HPA.
 */
export function capacityBadges(config: AutoscaleConfig): CapacityBadge[] {
  const badges: CapacityBadge[] = []
  const stopped = config.stopped === true
  if (config.autoscaling_ready?.status === 'False') badges.push(notReadyBadge(config.autoscaling_ready))
  if (config.quota_blocked === true) {
    badges.push({ key: 'quota', label: 'Quota blocks new pods', tone: 'danger', title: 'The project quota refuses new pods for this app.' })
  }
  if (config.enabled) {
    for (const c of config.conditions ?? []) {
      if (c.type === 'ScalingLimited' && c.status === 'True' && c.reason === 'TooManyReplicas') {
        badges.push({ key: 'at-max', label: 'At maximum', tone: 'warning', title: c.message })
      } else if (c.type === 'ScalingLimited' && c.status === 'True' && c.reason === 'TooFewReplicas') {
        badges.push({ key: 'at-min', label: 'At minimum', tone: 'info', title: c.message })
      } else if (c.type === 'ScalingActive' && c.status === 'False' && c.reason === 'FailedGetResourceMetric') {
        badges.push({ key: 'waiting-metrics', label: 'Waiting for metrics', tone: 'warning', title: c.message })
      } else if (c.type === 'ScalingActive' && c.status === 'False' && c.reason === 'ScalingDisabled' && !stopped) {
        badges.push({ key: 'not-scaling-zero', label: 'Not scaling at 0', tone: 'warning', title: c.message })
      }
    }
  }
  if (stopped) {
    badges.push({ key: 'stopped', label: 'Stopped, applies on start', tone: 'info', title: 'Changes saved now take effect when the app is started.' })
  }
  return badges
}

/** One line of the capacity panel's activity list. */
export interface ActivityLine {
  time: string
  text: string
  /** The autoscaler's own messages behind a line that summarises them. */
  title?: string
}

const metricsGapReasons = new Set(['FailedGetResourceMetric', 'FailedComputeMetricsReplicas'])
// The messages the autoscaler gives while new pods have no metrics yet. Other
// metric failures, such as a container without a CPU request, stay as they are.
const metricsGapMessages = ['did not receive metrics for targeted pods', 'no metrics returned from resource metrics API']
const metricsGapText = 'Metrics for new pods are not available yet; the autoscaler waits for them'

function isMetricsGap(entry: ScaleActivity): boolean {
  return metricsGapReasons.has(entry.reason) && metricsGapMessages.some(m => entry.message.includes(m))
}

/**
 * Turns scale activity, newest first, into the lines the panel shows. A run of
 * consecutive startup metric gaps becomes one plain line at the newest time,
 * with the distinct raw messages as its title.
 *
 * @param activity the activity from GET /autoscale, newest first
 * @returns one line per entry, with each run of metric gaps folded into one
 */
export function activityLines(activity: ScaleActivity[]): ActivityLine[] {
  const lines: ActivityLine[] = []
  let gap: { line: ActivityLine; messages: string[] } | null = null
  for (const entry of activity) {
    if (!isMetricsGap(entry)) {
      gap = null
      lines.push({ time: entry.time, text: entry.message || entry.reason })
      continue
    }
    if (gap === null) {
      gap = { line: { time: entry.time, text: metricsGapText }, messages: [] }
      lines.push(gap.line)
    }
    if (!gap.messages.includes(entry.message)) gap.messages.push(entry.message)
    gap.line.title = gap.messages.join('\n')
  }
  return lines
}

/** What the Scale tab says about automatic sizing for one app. */
export interface SizingNotice {
  summary: string
  /** Whether to say that scale-down is paused for a single replica. */
  singleReplica: boolean
}

/** Uses reported tracking state, falling back to enabled targets on older APIs. */
export function trackedMetrics(config: AutoscaleConfig | null): { cpu: boolean; memory: boolean } {
  if (config?.tracked) return config.tracked
  if (!config?.enabled) return { cpu: false, memory: false }
  return { cpu: config.cpu_target > 0, memory: config.memory_target > 0 }
}

/**
 * Describes automatic sizing for an app. A metric the autoscaler tracks keeps
 * its size, apart from the memory raise after an out-of-memory kill, and
 * decreases need at least two replicas.
 *
 * @param config the capacity state from GET /autoscale, or null before it loads
 * @param listedReplicas the app's stored count, used when the Deployment's is unknown
 * @returns the summary, and whether the single-replica pause applies
 */
export function sizingNotice(config: AutoscaleConfig | null, listedReplicas: number): SizingNotice {
  const tracked = trackedMetrics(config)
  const oomRaise = 'Memory is still raised after an out-of-memory kill.'
  let detail: string
  if (tracked.cpu && tracked.memory) {
    detail = `The autoscaler tracks CPU and memory, so neither is adjusted routinely. ${oomRaise}`
  } else if (tracked.cpu) {
    detail = 'The autoscaler tracks CPU, so CPU is left to it and only memory is adjusted based on usage.'
  } else if (tracked.memory) {
    detail = `The autoscaler tracks memory, so memory is left to it and only CPU is adjusted based on usage. ${oomRaise}`
  } else {
    detail = 'CPU and memory are adjusted based on usage.'
  }
  const replicas = config?.deployment_replicas ?? listedReplicas
  return {
    summary: `Resources are managed automatically. ${detail}`,
    singleReplica: replicas === 1 && !(tracked.cpu && tracked.memory),
  }
}
