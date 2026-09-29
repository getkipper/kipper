import type { ResourceDetail } from '@/api/resources'
import type { ResourceKind } from '@/utils/resources'

// ResourceValues are the four request and limit fields as the editor holds them.
export interface ResourceValues {
  memoryRequest: string
  memoryLimit: string
  cpuRequest: string
  cpuLimit: string
}

// ResourceEdit is a resources PUT body. Omit both fields to preserve a resource;
// send both empty to clear it. A single nonempty value sets a fixed size.
export interface ResourceEdit {
  memory_request?: string
  memory_limit?: string
  cpu_request?: string
  cpu_limit?: string
}

// changedResources sends both values for each edited resource. Sending an
// untouched resource would claim its displayed values as user bounds.
export function changedResources(before: ResourceValues, after: ResourceValues): ResourceEdit {
  const edit: ResourceEdit = {}
  if (before.memoryRequest !== after.memoryRequest || before.memoryLimit !== after.memoryLimit) {
    edit.memory_request = after.memoryRequest
    edit.memory_limit = after.memoryLimit
  }
  if (before.cpuRequest !== after.cpuRequest || before.cpuLimit !== after.cpuLimit) {
    edit.cpu_request = after.cpuRequest
    edit.cpu_limit = after.cpuLimit
  }
  return edit
}

// fixedSize sets one resource to a single size, request and limit alike.
export function fixedSize(kind: ResourceKind, quantity: string): ResourceEdit {
  return kind === 'memory'
    ? { memory_request: quantity, memory_limit: quantity }
    : { cpu_request: quantity, cpu_limit: quantity }
}

// automatic clears one resource's values, so Kipper sizes it on its own.
export function automatic(kind: ResourceKind): ResourceEdit {
  return kind === 'memory' ? { memory_request: '', memory_limit: '' } : { cpu_request: '', cpu_limit: '' }
}

// confirm saves a held resource's current values unchanged, which makes them
// the user's.
export function confirm(kind: ResourceKind, detail: ResourceDetail): ResourceEdit {
  const request = detail.request.value || detail.limit.value
  const limit = detail.limit.value || detail.request.value
  return kind === 'memory' ? { memory_request: request, memory_limit: limit } : { cpu_request: request, cpu_limit: limit }
}

export function isEmptyEdit(edit: ResourceEdit): boolean {
  return Object.keys(edit).length === 0
}

// forLegacyApi completes an edit for a console-api from before resource
// bounds, which replaces all four values on every save and would clear any it
// is not sent. The untouched resource goes along as it was loaded.
export function forLegacyApi(edit: ResourceEdit, loaded: ResourceValues): ResourceEdit {
  return {
    memory_request: edit.memory_request ?? loaded.memoryRequest,
    memory_limit: edit.memory_limit ?? loaded.memoryLimit,
    cpu_request: edit.cpu_request ?? loaded.cpuRequest,
    cpu_limit: edit.cpu_limit ?? loaded.cpuLimit,
  }
}

// formValues preserves loaded pairs in simple mode unless their limit changed.
// A changed limit sets a fixed size. Advanced mode uses all four input values.
export function formValues(loaded: ResourceValues, typed: ResourceValues, advanced: boolean): ResourceValues {
  if (advanced) return { ...typed }
  const memoryChanged = typed.memoryLimit !== loaded.memoryLimit
  const cpuChanged = typed.cpuLimit !== loaded.cpuLimit
  return {
    memoryRequest: memoryChanged ? typed.memoryLimit : loaded.memoryRequest,
    memoryLimit: memoryChanged ? typed.memoryLimit : loaded.memoryLimit,
    cpuRequest: cpuChanged ? typed.cpuLimit : loaded.cpuRequest,
    cpuLimit: cpuChanged ? typed.cpuLimit : loaded.cpuLimit,
  }
}

// resourcesPending checks whether the pod template satisfies the saved spec.
// Missing details on a current server require another read. Older servers
// cannot report this state, so polling stops.
export function resourcesPending(r: { partial_edits?: boolean, memory?: ResourceDetail, cpu?: ResourceDetail }): boolean {
  if (!r.partial_edits) return false
  if (!r.memory || !r.cpu) return true
  return r.memory.pending || r.cpu.pending
}
