import { ref } from 'vue'
import type { ResourceDetail } from '@/api/resources'
import { changedResources, forLegacyApi, formValues, isEmptyEdit, type ResourceEdit, type ResourceValues } from '@/utils/resourceEdits'

// LoadedResources is a resources GET response: the four resolved values, and
// on a console-api that knows resource bounds, the per-resource details.
export interface LoadedResources {
  memory_request: string
  memory_limit: string
  cpu_request: string
  cpu_limit: string
  memory?: ResourceDetail
  cpu?: ResourceDetail
  partial_edits?: boolean
}

const empty: ResourceValues = { memoryRequest: '', memoryLimit: '', cpuRequest: '', cpuLimit: '' }

// PendingEdit tells the caller to save and record the values, skip an unchanged
// form, or wait for a successful load before submitting resource edits.
export type PendingEdit = { edit: ResourceEdit; values: ResourceValues } | 'unchanged' | 'unknown'

// useResourceForm tracks edited resource pairs across simple and advanced
// modes. Older servers also need the untouched pair in each save.
export function useResourceForm() {
  const memoryRequest = ref('')
  const memoryLimit = ref('')
  const cpuRequest = ref('')
  const cpuLimit = ref('')
  const advanced = ref(false)
  const loaded = ref<ResourceValues>({ ...empty })
  const known = ref(false)
  const legacyApi = ref(false)

  function typed(): ResourceValues {
    return { memoryRequest: memoryRequest.value, memoryLimit: memoryLimit.value, cpuRequest: cpuRequest.value, cpuLimit: cpuLimit.value }
  }

  function show(v: ResourceValues) {
    memoryRequest.value = v.memoryRequest
    memoryLimit.value = v.memoryLimit
    cpuRequest.value = v.cpuRequest
    cpuLimit.value = v.cpuLimit
  }

  // load takes a resources GET response, or null when the read failed.
  function load(r: LoadedResources | null) {
    const values = r
      ? { memoryRequest: r.memory_request || '', memoryLimit: r.memory_limit || '', cpuRequest: r.cpu_request || '', cpuLimit: r.cpu_limit || '' }
      : { ...empty }
    show(values)
    loaded.value = values
    advanced.value = values.cpuRequest !== values.cpuLimit || values.memoryRequest !== values.memoryLimit
    known.value = r !== null
    legacyApi.value = !!r && !r.partial_edits
  }

  function pendingEdit(): PendingEdit {
    const values = formValues(loaded.value, typed(), advanced.value)
    const changed = changedResources(loaded.value, values)
    if (isEmptyEdit(changed)) return 'unchanged'
    if (!known.value) return 'unknown'
    return { edit: legacyApi.value ? forLegacyApi(changed, loaded.value) : changed, values }
  }

  // Update both the inputs and baseline after saving, including values
  // mirrored by simple mode.
  function saved(values: ResourceValues) {
    show(values)
    loaded.value = { ...values }
  }

  return { memoryRequest, memoryLimit, cpuRequest, cpuLimit, advanced, load, pendingEdit, saved }
}
