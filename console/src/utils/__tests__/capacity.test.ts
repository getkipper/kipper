import { describe, expect, it } from 'vitest'
import { activityLines, boundsMove, capacityBadges, checkCapacity, planCapacitySave, sizingNotice, type CapacityForm } from '../capacity'
import type { AutoscaleConfig, ScaleActivity } from '@/api/apps'

const fixed: CapacityForm = { policy: 'none', desired: 3, min: 2, max: 6, cpu: 0, memory: 0 }
const tracking: CapacityForm = { policy: 'tracking', desired: 4, min: 2, max: 6, cpu: 70, memory: 0 }
const unbounded: CapacityForm = { policy: 'none', desired: 3, min: null, max: null, cpu: 0, memory: 0 }

describe('checkCapacity', () => {
  it('accepts a fixed count within the bounds', () => {
    expect(checkCapacity(fixed)).toEqual({})
  })

  it('accepts a fixed count with no bounds at all', () => {
    expect(checkCapacity(unbounded)).toEqual({})
  })

  it('refuses a desired count of 0 with or without bounds, pointing at Stop app', () => {
    expect(checkCapacity({ ...unbounded, desired: 0 }).error).toContain('Stop app')
    expect(checkCapacity({ ...fixed, desired: 0 }).error).toContain('Stop app')
  })

  it('leaves an unedited desired count of 0 alone, since the save does not write it', () => {
    const atZero: CapacityForm = { ...unbounded, desired: 0 }
    expect(checkCapacity({ ...atZero, max: null }, atZero)).toEqual({})
  })

  it('offers to raise the maximum when desired is above it', () => {
    const check = checkCapacity({ ...fixed, desired: 8 })
    expect(check.error).toContain('above the maximum of 6')
    expect(check.raiseMaxTo).toBe(8)
  })

  it('refuses a desired count below the minimum without an offer', () => {
    const check = checkCapacity({ ...fixed, desired: 1 })
    expect(check.error).toContain('below the minimum of 2')
    expect(check.raiseMaxTo).toBeUndefined()
  })

  it('counts an empty minimum as 1', () => {
    expect(checkCapacity({ ...fixed, min: null, desired: 1 })).toEqual({})
  })

  it('refuses a minimum above the maximum', () => {
    expect(checkCapacity({ ...fixed, min: 7 }).error).toBe('The minimum (7) must not exceed the maximum (6).')
  })

  it('refuses a minimum without a maximum', () => {
    expect(checkCapacity({ ...unbounded, min: 2 }).error).toContain('Set a maximum with the minimum')
  })

  it('refuses bounds below 1 and fractions', () => {
    expect(checkCapacity({ ...fixed, min: 0 }).error).toContain('minimum must be a whole number of at least 1')
    expect(checkCapacity({ ...fixed, max: 2.5 }).error).toContain('maximum must be a whole number of at least 1')
    expect(checkCapacity({ ...unbounded, desired: -1 }).error).toContain('desired count must be a whole number')
  })

  it('needs a maximum and at least one target for target tracking', () => {
    expect(checkCapacity({ ...tracking, max: null, min: null }).error).toContain('needs a maximum')
    expect(checkCapacity({ ...tracking, cpu: 0, memory: 0 }).error).toContain('Set a CPU or memory target')
    expect(checkCapacity({ ...tracking, cpu: 0, memory: 80 })).toEqual({})
  })

  it('takes targets above 100 percent and refuses negative ones', () => {
    expect(checkCapacity({ ...tracking, cpu: 150 })).toEqual({})
    expect(checkCapacity({ ...tracking, memory: -5 }).error).toContain('whole percentages')
  })

  it('ignores the desired count while target tracking sets it', () => {
    expect(checkCapacity({ ...tracking, desired: null })).toEqual({})
    expect(checkCapacity({ ...tracking, desired: 99 })).toEqual({})
  })

  it('leaves an unknown desired count unchecked while it stays unedited, since the save does not write it', () => {
    const off: CapacityForm = { ...tracking, policy: 'none', desired: null }
    expect(checkCapacity(off, { ...tracking, desired: null })).toEqual({})
    expect(checkCapacity(off, tracking).error).toContain('desired count must be a whole number')
    expect(checkCapacity(off).error).toContain('desired count must be a whole number')
  })

  it('leaves a known desired count unchecked while it stays unedited, since the server moves it into the bounds', () => {
    const atZero: CapacityForm = { ...tracking, desired: 0 }
    expect(checkCapacity({ ...atZero, policy: 'none' }, atZero)).toEqual({})
    const above: CapacityForm = { ...tracking, desired: 9 }
    expect(checkCapacity({ ...above, policy: 'none' }, above)).toEqual({})
    expect(checkCapacity({ ...tracking, policy: 'none', max: 3 }, tracking)).toEqual({})
    expect(checkCapacity({ ...fixed, min: 4 }, fixed)).toEqual({})
  })

  it('still checks a desired count once it is edited', () => {
    expect(checkCapacity({ ...tracking, policy: 'none', desired: 0 }, tracking).error).toContain('Stop app')
    expect(checkCapacity({ ...tracking, policy: 'none', desired: 9 }, tracking).raiseMaxTo).toBe(9)
    expect(checkCapacity({ ...fixed, min: 4 }, { ...fixed, desired: 5 }).error).toContain('below the minimum of 4')
  })
})

describe('boundsMove', () => {
  it('says where the server moves an unedited count that sits outside the bounds', () => {
    const atZero: CapacityForm = { ...tracking, desired: 0 }
    const off: CapacityForm = { ...atZero, policy: 'none' }
    expect(boundsMove(atZero, planCapacitySave(atZero, off, true))).toEqual({ from: 0, to: 2 })
    const above: CapacityForm = { ...tracking, desired: 9 }
    expect(boundsMove(above, planCapacitySave(above, { ...above, policy: 'none' }, true))).toEqual({ from: 9, to: 6 })
    expect(boundsMove(fixed, planCapacitySave(fixed, { ...fixed, min: 4 }, true))).toEqual({ from: 3, to: 4 })
  })

  it('reports nothing when the count fits, is edited, is unknown or the bounds go away', () => {
    expect(boundsMove(tracking, planCapacitySave(tracking, { ...tracking, policy: 'none' }, true))).toBeUndefined()
    const above: CapacityForm = { ...tracking, desired: 9 }
    expect(boundsMove(above, planCapacitySave(above, { ...above, policy: 'none', desired: 5 }, true))).toBeUndefined()
    const unknown: CapacityForm = { ...tracking, desired: null }
    expect(boundsMove(unknown, planCapacitySave(unknown, { ...unknown, policy: 'none' }, true))).toBeUndefined()
    const atZero: CapacityForm = { ...fixed, desired: 0 }
    expect(boundsMove(atZero, planCapacitySave(atZero, { ...atZero, min: null, max: null }, true))).toBeUndefined()
    expect(boundsMove(above, planCapacitySave(above, { ...above, policy: 'none' }, false))).toBeUndefined()
  })

  it('leaves a move under target tracking to the server, since the live count is not the stored one', () => {
    expect(boundsMove(tracking, planCapacitySave(tracking, { ...tracking, max: 3 }, true))).toBeUndefined()
    expect(boundsMove(tracking, planCapacitySave(tracking, { ...tracking, min: 5 }, true))).toBeUndefined()
    const off: CapacityForm = { ...fixed, desired: 9, max: 10 }
    expect(boundsMove(off, planCapacitySave(off, { ...tracking, max: 6 }, true))).toBeUndefined()
  })
})

describe('planCapacitySave', () => {
  it('sends a pure desired change with policy None through /scale on every API', () => {
    for (const capacityApi of [true, false]) {
      expect(planCapacitySave(fixed, { ...fixed, desired: 5 }, capacityApi)).toEqual({ steps: [{ kind: 'scale', replicas: 5 }], atomic: true })
    }
  })

  it('saves target tracking with one PUT in the shape every API reads', () => {
    expect(planCapacitySave(fixed, { ...tracking, min: null }, false)).toEqual({
      steps: [{ kind: 'put', body: { min_replicas: 1, max_replicas: 6, cpu_target: 70, memory_target: 0 } }],
      atomic: true,
    })
  })

  it('switches the policy off with one combined PUT on a capacity API, leaving the count to the server', () => {
    expect(planCapacitySave(tracking, { ...tracking, policy: 'none' }, true)).toEqual({
      steps: [{ kind: 'put', body: { enabled: false, min_replicas: 2, max_replicas: 6, cpu_target: 70, memory_target: 0 } }],
      atomic: true,
    })
  })

  it('sends an edited desired count with the switch-off', () => {
    const plan = planCapacitySave(tracking, { ...tracking, policy: 'none', desired: 5 }, true)
    expect(plan).toMatchObject({ steps: [{ kind: 'put', body: { enabled: false, replicas: 5 } }] })
  })

  it('saves bounds with policy None in one PUT on a capacity API', () => {
    expect(planCapacitySave(fixed, { ...fixed, max: 8, desired: 7 }, true)).toEqual({
      steps: [{ kind: 'put', body: { enabled: false, replicas: 7, min_replicas: 2, max_replicas: 8, cpu_target: 0, memory_target: 0 } }],
      atomic: true,
    })
  })

  it('removes the bounds by leaving them out of the PUT', () => {
    expect(planCapacitySave(fixed, { ...fixed, min: null, max: null }, true)).toEqual({
      steps: [{ kind: 'put', body: { enabled: false, cpu_target: 0, memory_target: 0 } }],
      atomic: true,
    })
  })

  it('falls back to DELETE and then /scale on an older API, and says the save is not atomic', () => {
    expect(planCapacitySave(tracking, { ...tracking, policy: 'none', desired: 5 }, false)).toEqual({
      steps: [{ kind: 'delete' }, { kind: 'scale', replicas: 5 }],
      atomic: false,
    })
    expect(planCapacitySave(tracking, { ...tracking, policy: 'none' }, false)).toEqual({ steps: [{ kind: 'delete' }], atomic: true })
  })

  it('cannot store bounds without a policy on an older API', () => {
    expect(planCapacitySave(fixed, { ...fixed, max: 8 }, false)).toMatchObject({ unsupported: expect.stringContaining('older') })
    expect(planCapacitySave(tracking, { ...tracking, policy: 'none', max: 8 }, false)).toMatchObject({ unsupported: expect.any(String) })
  })

  it('plans nothing when nothing changed', () => {
    expect(planCapacitySave(fixed, { ...fixed }, true)).toEqual({ steps: [], atomic: true })
  })
})

const base: AutoscaleConfig = {
  enabled: true, min_replicas: 2, max_replicas: 6, cpu_target: 70, memory_target: 0,
  current_replicas: 6, current_cpu: '', current_memory: '', capacity_api: 1, stopped: false, quota_blocked: false,
}

function labels(config: Partial<AutoscaleConfig>): string[] {
  return capacityBadges({ ...base, ...config }).map(b => b.label)
}

function hpaCondition(type: string, status: string, reason: string) {
  return { type, status, reason, message: `${type} ${reason}` }
}

describe('capacityBadges', () => {
  it('shows none for a healthy policy', () => {
    expect(labels({ conditions: [hpaCondition('ScalingActive', 'True', 'ValidMetricFound')] })).toEqual([])
  })

  it('reads the limits from ScalingLimited', () => {
    expect(labels({ conditions: [hpaCondition('ScalingLimited', 'True', 'TooManyReplicas')] })).toEqual(['At maximum'])
    expect(labels({ conditions: [hpaCondition('ScalingLimited', 'True', 'TooFewReplicas')] })).toEqual(['At minimum'])
    expect(labels({ conditions: [hpaCondition('ScalingLimited', 'False', 'DesiredWithinRange')] })).toEqual([])
  })

  it('reads missing metrics and a scale at zero from ScalingActive', () => {
    expect(labels({ conditions: [hpaCondition('ScalingActive', 'False', 'FailedGetResourceMetric')] })).toEqual(['Waiting for metrics'])
    expect(labels({ conditions: [hpaCondition('ScalingActive', 'False', 'ScalingDisabled')] })).toEqual(['Not scaling at 0'])
  })

  it('carries the condition message as the badge title', () => {
    const [badge] = capacityBadges({ ...base, conditions: [hpaCondition('ScalingLimited', 'True', 'TooManyReplicas')] })
    expect(badge.title).toBe('ScalingLimited TooManyReplicas')
  })

  it('reports a quota block, an invalid policy and a stopped app', () => {
    expect(labels({ quota_blocked: true })).toEqual(['Quota blocks new pods'])
    expect(labels({ autoscaling_ready: hpaCondition('AutoscalingReady', 'False', 'InvalidPolicy') })).toEqual(['Policy invalid'])
    expect(labels({ stopped: true })).toEqual(['Stopped, applies on start'])
  })

  it('shows every false AutoscalingReady condition, whatever the policy', () => {
    const ready = (reason: string) => hpaCondition('AutoscalingReady', 'False', reason)
    expect(labels({ autoscaling_ready: ready('ReplicasOutsideBounds') })).toEqual(['Desired outside bounds'])
    expect(labels({ autoscaling_ready: ready('AutoscalerReconcileFailed') })).toEqual(['Autoscaler not applied'])
    expect(labels({ enabled: false, autoscaling_ready: ready('AutoscalerDeleteFailed') })).toEqual(['Autoscaler not removed'])
    expect(labels({ enabled: false, autoscaling_ready: ready('InvalidPolicy') })).toEqual(['Policy invalid'])
    expect(labels({ autoscaling_ready: hpaCondition('AutoscalingReady', 'True', 'PolicyApplied') })).toEqual([])
  })

  it("carries the server's message, and for an unknown reason the reason too", () => {
    const [known] = capacityBadges({ ...base, autoscaling_ready: hpaCondition('AutoscalingReady', 'False', 'AutoscalerReconcileFailed') })
    expect(known).toMatchObject({ tone: 'danger', title: 'AutoscalingReady AutoscalerReconcileFailed' })
    const [outside] = capacityBadges({ ...base, autoscaling_ready: hpaCondition('AutoscalingReady', 'False', 'ReplicasOutsideBounds') })
    expect(outside.tone).toBe('warning')
    const [unknown] = capacityBadges({ ...base, autoscaling_ready: hpaCondition('AutoscalingReady', 'False', 'SomethingNew') })
    expect(unknown).toMatchObject({ label: 'Autoscaling not ready', tone: 'danger', title: 'SomethingNew: AutoscalingReady SomethingNew' })
  })

  it('lets a stop explain a scale at zero', () => {
    expect(labels({ stopped: true, conditions: [hpaCondition('ScalingActive', 'False', 'ScalingDisabled')] })).toEqual(['Stopped, applies on start'])
  })

  it('ignores autoscaler conditions while the policy is off', () => {
    expect(labels({ enabled: false, conditions: [hpaCondition('ScalingLimited', 'True', 'TooManyReplicas')] })).toEqual([])
  })
})

describe('activityLines', () => {
  const event = (time: string, reason: string, message: string): ScaleActivity => ({ time, source: 'hpa_event', type: 'Warning', reason, message })
  const noMetrics = 'failed to get cpu utilization: did not receive metrics for targeted pods (pods might be unready)'
  const invalid = 'invalid metrics (1 invalid out of 1), first error is: failed to get cpu resource metric value: failed to get cpu utilization: did not receive metrics for targeted pods (pods might be unready)'
  const noneReturned = 'failed to get cpu utilization: unable to get metrics for resource cpu: no metrics returned from resource metrics API'
  const gap = 'Metrics for new pods are not available yet; the autoscaler waits for them'

  it('shows the startup metric gaps as one plain line with the raw messages as its title', () => {
    const lines = activityLines([
      event('2026-10-04T09:32:00Z', 'FailedComputeMetricsReplicas', invalid),
      event('2026-10-04T09:31:00Z', 'FailedGetResourceMetric', noMetrics),
      event('2026-10-04T09:30:30Z', 'FailedGetResourceMetric', noneReturned),
      event('2026-10-04T09:30:00Z', 'FailedGetResourceMetric', noMetrics),
    ])
    expect(lines).toEqual([{ time: '2026-10-04T09:32:00Z', text: gap, title: [invalid, noMetrics, noneReturned].join('\n') }])
  })

  it('collapses only consecutive gaps and keeps every other event as it is', () => {
    const rescale: ScaleActivity = { time: '2026-10-04T09:31:00Z', source: 'hpa_event', type: 'Normal', reason: 'SuccessfulRescale', message: 'New size: 3' }
    const logged: ScaleActivity = { time: '2026-10-04T09:29:00Z', source: 'scale_log', reason: 'HPA autoscaling', message: '' }
    const lines = activityLines([
      event('2026-10-04T09:32:00Z', 'FailedGetResourceMetric', noMetrics),
      rescale,
      event('2026-10-04T09:30:00Z', 'FailedGetResourceMetric', noMetrics),
      logged,
    ])
    expect(lines.map(l => l.text)).toEqual([gap, 'New size: 3', gap, 'HPA autoscaling'])
    expect(lines[1].title).toBeUndefined()
  })

  it('keeps a metric failure that is not a startup gap', () => {
    const missingRequest = 'failed to get cpu utilization: missing request for cpu in container web of Pod web-1'
    const serverDown = 'failed to get cpu utilization: unable to get metrics for resource cpu: unable to fetch metrics from resource metrics API: the server is currently unable to handle the request'
    const lines = activityLines([
      event('2026-10-04T09:31:00Z', 'FailedGetResourceMetric', missingRequest),
      event('2026-10-04T09:30:00Z', 'FailedGetResourceMetric', serverDown),
      event('2026-10-04T09:29:00Z', 'SomethingElse', noMetrics),
    ])
    expect(lines.map(l => l.text)).toEqual([missingRequest, serverDown, noMetrics])
  })
})

describe('sizingNotice', () => {
  const base: AutoscaleConfig = {
    enabled: true, min_replicas: 1, max_replicas: 3, cpu_target: 0, memory_target: 0,
    current_replicas: 3, current_cpu: '', current_memory: '', capacity_api: 1, deployment_replicas: 3,
  }
  const both = 'CPU and memory are adjusted based on usage.'

  it('says that a tracked metric is left to the autoscaler and the other one is adjusted', () => {
    expect(sizingNotice({ ...base, cpu_target: 70 }, 1).summary).toBe(
      'Resources are managed automatically. The autoscaler tracks CPU, so CPU is left to it and only memory is adjusted based on usage.')
    expect(sizingNotice({ ...base, memory_target: 80 }, 1).summary).toBe(
      'Resources are managed automatically. The autoscaler tracks memory, so memory is left to it and only CPU is adjusted based on usage. Memory is still raised after an out-of-memory kill.')
    expect(sizingNotice({ ...base, cpu_target: 70, memory_target: 80 }, 1).summary).toBe(
      'Resources are managed automatically. The autoscaler tracks CPU and memory, so neither is adjusted routinely. Memory is still raised after an out-of-memory kill.')
  })

  it('adjusts both when nothing is tracked', () => {
    expect(sizingNotice({ ...base, enabled: false, cpu_target: 70 }, 1).summary).toContain(both)
    expect(sizingNotice(null, 1).summary).toContain(both)
  })

  it("takes the tracked metrics from the server over the stored targets", () => {
    const invalid = { type: 'AutoscalingReady', status: 'False', reason: 'InvalidPolicy', message: 'no maximum' }
    const config = { ...base, cpu_target: 70, current_cpu: '', autoscaling_ready: invalid }
    expect(sizingNotice({ ...config, tracked: { cpu: false, memory: true } }, 1).summary).toContain('The autoscaler tracks memory,')
    expect(sizingNotice({ ...config, memory_target: 80, tracked: { cpu: true, memory: true } }, 1).summary).toContain('tracks CPU and memory')
  })

  it('reads the targets from an older console-api that does not say what is tracked', () => {
    expect(sizingNotice({ ...base, cpu_target: 70, tracked: undefined }, 1).summary).toContain('The autoscaler tracks CPU,')
  })

  it('mentions the single-replica pause only when the app runs one replica', () => {
    expect(sizingNotice({ ...base, cpu_target: 70, deployment_replicas: 3 }, 1).singleReplica).toBe(false)
    expect(sizingNotice({ ...base, cpu_target: 70, deployment_replicas: 1 }, 3).singleReplica).toBe(true)
    expect(sizingNotice({ ...base, enabled: false, deployment_replicas: 0 }, 1).singleReplica).toBe(false)
    expect(sizingNotice({ ...base, enabled: false, deployment_replicas: null }, 1).singleReplica).toBe(true)
    expect(sizingNotice(null, 2).singleReplica).toBe(false)
  })

  it('leaves the pause out when nothing is adjusted routinely', () => {
    expect(sizingNotice({ ...base, cpu_target: 70, memory_target: 80, deployment_replicas: 1 }, 1).singleReplica).toBe(false)
  })
})
