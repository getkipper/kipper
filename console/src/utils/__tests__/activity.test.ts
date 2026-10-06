import { describe, expect, it } from 'vitest'
import type { AppActivity, ReplicaChange } from '@/api/activity'
import type { AutoscaleConfig } from '@/api/apps'
import { changeObservation, changeSummary, markerLook, targetGuide, trafficNote, trafficPartial } from '../activity'

function activity(over: Partial<AppActivity> = {}): AppActivity {
  return { available: true, timestamps: [0, 30, 60], changes: [], changes_detailed: 0, degraded: [], ...over }
}

function change(over: Partial<ReplicaChange> = {}): ReplicaChange {
  return { time: 30, from: 2, to: 3, cause: 'autoscaler', autoscaled: true, around: null, ...over }
}

const config = (over: Partial<AutoscaleConfig> = {}): AutoscaleConfig => ({
  enabled: true, min_replicas: 1, max_replicas: 3, cpu_target: 70, memory_target: 0,
  current_replicas: 2, current_cpu: '', current_memory: '', ...over,
})

describe('markerLook', () => {
  it('draws an evidenced change solid, an unrecorded one dashed, and labels stops and bounds', () => {
    expect(markerLook('autoscaler')).toEqual({ style: 'solid' })
    expect(markerLook('while_autoscaling')).toEqual({ style: 'dashed' })
    expect(markerLook('to_zero').label).toBe('to 0')
    expect(markerLook('bounds_change').label).toBe('bounds')
    expect(markerLook('unknown').style).toBe('dotted')
  })
})

describe('changeSummary', () => {
  it('names the autoscaler only when the cause is its event', () => {
    expect(changeSummary(change())).toBe('The autoscaler changed the desired count from 2 pods to 3 pods.')
    expect(changeSummary(change({ cause: 'while_autoscaling' }))).toContain('The cause was not recorded.')
  })

  it('explains stop and minimum behaviour when autoscaling data is present', () => {
    expect(changeSummary(change({ to: 0, cause: 'to_zero' }))).toContain('Stop an app to take it to 0 pods')
    expect(changeSummary(change({ to: 0, cause: 'to_zero', autoscaled: false }))).toBe('The desired count changed from 2 pods to 0 pods.')
    expect(changeSummary(change({ from: 0, to: 2, cause: 'from_zero' }))).toContain('Autoscaling keeps an active app at or above its minimum.')
    expect(changeSummary(change({ from: 0, to: 2, cause: 'from_zero', autoscaled: false }))).toBe('The desired count changed from 0 pods to 2 pods.')
  })
})

describe('changeObservation', () => {
  const around = {
    window_seconds: 120,
    cpu: { peak_pct: 97, avg_pct: 41, target_pct: 70 },
    memory: null,
    requests: { total: 57, not_found: 17, aborted: 37, server_error: 0, other: 3 },
  }

  it('phrases the figures as what was measured', () => {
    expect(changeObservation(change({ around: { ...around, requests: { total: 3, not_found: 0, aborted: 0, server_error: 1, other: 2 } } }))).toContain('including 1 server error.')
    expect(changeObservation(change({ around }))).toBe(
      'In the 2 minutes before this change: CPU peaked at 97% of its request (target 70%, average 41%). About 57 requests, including 37 abandoned and 17 not found.',
    )
  })

  it('adds the sampling note when no peak reached the target plus tolerance', () => {
    const low = { ...around, cpu: { peak_pct: 75, avg_pct: 40, target_pct: 70 } }
    expect(changeObservation(change({ around: low }))).toContain('The autoscaler measures usage separately')
    expect(changeObservation(change({ around }))).not.toContain('measures usage separately')
    expect(changeObservation(change({ from: 3, to: 2, cause: 'while_autoscaling', around: low }))).not.toContain('measures usage separately')
  })

  it('gives the reason when the figures are missing', () => {
    expect(changeObservation(change({ around_unavailable: 'Measurements from before this change are no longer available.' }))).toContain('no longer available')
    expect(changeObservation(change())).toBeNull()
  })
})

describe('targetGuide', () => {
  const withRequest = activity({ cpu_requested_millis: [360, 360, null] })

  it('gives the trigger with the default tolerance named and the absolute figures', () => {
    expect(targetGuide(config(), withRequest, false)).toEqual([
      "CPU target: 70% of the resource request. With the default 10% tolerance, pods may be added above about 77% (277m of 360m), up to 3 pods.",
    ])
  })

  it('says no more pods are added at the maximum', () => {
    expect(targetGuide(config(), withRequest, true)[0]).toContain('maximum of 3 pods prevents further autoscaling increases')
  })

  it('says either metric can add a pod when both are tracked', () => {
    expect(targetGuide(config({ memory_target: 80 }), withRequest, false)).toContain('Either target can trigger more pods.')
  })

  it('is empty while autoscaling is off or the app is stopped', () => {
    expect(targetGuide(config({ enabled: false }), withRequest, false)).toEqual([])
    expect(targetGuide(config({ stopped: true }), withRequest, false)).toEqual([])
  })
})

describe('trafficNote and trafficPartial', () => {
  it('explains each state without data in one sentence', () => {
    expect(trafficNote(activity({ traffic: 'no_route' }))).toContain('no route')
    expect(trafficNote(activity({ traffic: 'not_attributed' }))).toBe('No request figures are available for this range.')
    expect(trafficNote(activity({ traffic: 'not_attributed', traffic_reason: 'shared' }))).toContain('Use a different app name.')
    expect(trafficNote(activity({ traffic: 'unavailable' }))).toContain('could not be loaded')
    expect(trafficNote(activity({ traffic: 'available' }))).toBeNull()
  })

  it('reports a range covered only in part', () => {
    const series = (ok: (number | null)[]) => ({ ok, redirect: ok, client_error: ok, aborted: ok, server_error: ok })
    expect(trafficPartial(activity({ traffic: 'available', timestamps: [0, 100, 200], requests_per_min: series([null, 1, 2]) }))).toBe(true)
    expect(trafficPartial(activity({ traffic: 'available', requests_per_min: series([0, 1, 2]) }))).toBe(false)
    expect(trafficPartial(activity({ traffic: 'available', timestamps: [0, 30, 60], requests_per_min: series([0, 1, null]) }))).toBe(false)
    expect(trafficPartial(activity({ traffic: 'available', timestamps: [0, 100, 200, 300], requests_per_min: series([0, 1, null, null]) }))).toBe(true)
  })
})
