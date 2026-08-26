<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { Timer, Play, Clock, RefreshCw, Plus } from 'lucide-vue-next'
import { useToast } from '@/composables/useToast'
import { useCapabilities } from '@/composables/useCapabilities'
import { fetchJobs, createJob, type Job } from '@/api/jobs'
import { fetchProjects, type Project } from '@/api/projects'
import JobDetail from '@/components/JobDetail.vue'

const toast = useToast()
const { canInNamespace } = useCapabilities()

const jobs = ref<Job[]>([])
const loading = ref(false)
const refreshing = ref(false)
const selectedJob = ref<Job | null>(null)

// Create form
const showCreate = ref(false)
const creating = ref(false)
const allProjects = ref<Project[]>([])
const newName = ref('')
const newImage = ref('')
const newCommand = ref('')
const newSchedule = ref('')
const newNamespace = ref('default')
const newMemory = ref('')
const newCPU = ref('')

// Somewhere the caller may create a job. The button opens the form; the form's
// own namespace field decides where, and the submit asks again for that one.
const canCreateSomewhere = computed(() =>
  namespaceOptions.value.some(o => canInNamespace(o.value, 'kipper.write')),
)

const namespaceOptions = computed(() => {
  const options: { label: string; value: string }[] = [
    { label: 'default', value: 'default' },
  ]
  for (const p of allProjects.value) {
    for (const env of p.environments) {
      options.push({ label: `${p.display_name || p.name}, ${env.name}`, value: env.namespace })
    }
  }
  return options
})

onMounted(async () => {
  await loadJobs()
})

async function handleCreate() {
  if (!canInNamespace(newNamespace.value, 'kipper.write')) return
  if (!newName.value || !newImage.value) return
  creating.value = true
  try {
    await createJob({
      name: newName.value,
      image: newImage.value,
      command: newCommand.value || undefined,
      schedule: newSchedule.value || undefined,
      namespace: newNamespace.value,
      memory: newMemory.value || undefined,
      cpu: newCPU.value || undefined,
    })
    toast.success(`Job ${newName.value} created`)
    const createdName = newName.value
    newName.value = ''
    newImage.value = ''
    newCommand.value = ''
    newSchedule.value = ''
    newMemory.value = ''
    newCPU.value = ''
    showCreate.value = false
    for (let i = 0; i < 10; i++) {
      await loadJobs()
      if (jobs.value.some(j => j.name === createdName)) break
      await new Promise(r => setTimeout(r, 500))
    }
  } catch {
    toast.error('Failed to create job')
  } finally {
    creating.value = false
  }
}

async function loadJobs() {
  loading.value = true
  try {
    const [j, p] = await Promise.all([fetchJobs(), fetchProjects()])
    jobs.value = j
    allProjects.value = p
  } catch {
    toast.error('Failed to load jobs')
  } finally {
    loading.value = false
  }
}

async function refreshList() {
  refreshing.value = true
  try {
    jobs.value = await fetchJobs()
    toast.success('Jobs refreshed')
  } catch {
    toast.error('Failed to refresh')
  } finally {
    refreshing.value = false
  }
}

function statusColor(status: string): string {
  switch (status) {
    case 'completed': return 'text-emerald-700 dark:text-emerald-400'
    case 'failed': return 'text-red-700 dark:text-red-400'
    case 'running': return 'text-amber-700 dark:text-amber-400'
    case 'scheduled': return 'text-kipper-700 dark:text-kipper-400'
    default: return 'text-slate-500'
  }
}

function statusDot(status: string): string {
  switch (status) {
    case 'completed': return 'bg-emerald-500'
    case 'failed': return 'bg-red-500'
    case 'running': return 'bg-amber-500'
    case 'scheduled': return 'bg-kipper-500'
    default: return 'bg-slate-400'
  }
}
</script>

<template>
  <div class="animate-fade-in">
    <!-- Header -->
    <div class="mb-8 flex items-center justify-between">
      <div>
        <h1 class="text-2xl font-semibold tracking-tight text-slate-900 dark:text-slate-50">Jobs</h1>
        <p class="mt-1 text-sm text-slate-500 dark:text-slate-400">Scheduled tasks and one-off jobs</p>
      </div>
      <div class="flex items-center gap-2">
        <button
          @click="refreshList"
          :disabled="refreshing"
          class="rounded-lg border border-slate-300 p-2.5 text-slate-600 transition-colors hover:bg-slate-100 dark:border-slate-600 dark:text-slate-400 dark:hover:bg-slate-800"
          title="Refresh"
        >
          <RefreshCw class="h-4 w-4" :class="refreshing ? 'animate-spin' : ''" :stroke-width="1.75" />
        </button>
        <button
          v-if="canCreateSomewhere"
          @click="showCreate = !showCreate"
          class="inline-flex items-center gap-2 rounded-lg bg-kipper-600 px-4 py-2.5 text-sm font-semibold text-white shadow-sm transition-colors hover:bg-kipper-700 dark:bg-kipper-500 dark:hover:bg-kipper-600"
        >
          <Plus class="h-4 w-4" :stroke-width="2" />
          New job
        </button>
      </div>
    </div>

    <!-- Create form -->
    <div v-if="showCreate" class="mb-6 animate-slide-up rounded-xl border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
      <h3 class="mb-4 text-sm font-semibold text-slate-900 dark:text-slate-50">Create a job</h3>
      <form @submit.prevent="handleCreate" class="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div>
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Job name</label>
          <input
            v-model="newName"
            type="text"
            placeholder="cleanup-expired"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
          />
        </div>
        <div>
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Image</label>
          <input
            v-model="newImage"
            type="text"
            placeholder="ghcr.io/acme/cleanup:latest"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
          />
        </div>
        <div>
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Command</label>
          <input
            v-model="newCommand"
            type="text"
            placeholder="python cleanup.py"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
          />
        </div>
        <div>
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Schedule (cron)</label>
          <input
            v-model="newSchedule"
            type="text"
            placeholder="0 3 * * *"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
          />
          <p class="mt-1 text-xs text-slate-500 dark:text-slate-400">Leave empty for a one-off job</p>
        </div>
        <div>
          <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Namespace</label>
          <select
            v-model="newNamespace"
            class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50"
          >
            <option v-for="ns in namespaceOptions" :key="ns.value" :value="ns.value">{{ ns.label }}</option>
          </select>
        </div>
        <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <div>
            <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">Memory</label>
            <input
              v-model="newMemory"
              type="text"
              placeholder="128Mi"
              class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
            />
          </div>
          <div>
            <label class="mb-1.5 block text-sm font-medium text-slate-700 dark:text-slate-300">CPU</label>
            <input
              v-model="newCPU"
              type="text"
              placeholder="100m"
              class="block w-full rounded-lg border border-slate-300 bg-white px-3.5 py-2.5 text-sm text-slate-900 placeholder-slate-400 shadow-sm focus:border-kipper-500 focus:outline-none focus:ring-2 focus:ring-kipper-500/20 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-50 dark:placeholder-slate-500"
            />
          </div>
        </div>
        <div class="sm:col-span-2">
          <button
            type="submit"
            :disabled="creating || !newName || !newImage"
            class="rounded-lg bg-kipper-600 px-4 py-2.5 text-sm font-semibold text-white shadow-sm transition-colors hover:bg-kipper-700 disabled:opacity-50 dark:bg-kipper-500 dark:hover:bg-kipper-600"
          >
            {{ creating ? 'Creating...' : newSchedule ? 'Create scheduled job' : 'Run one-off job' }}
          </button>
        </div>
      </form>
    </div>

    <!-- Loading -->
    <div v-if="loading" class="py-12 text-center text-sm text-slate-500 dark:text-slate-400">Loading...</div>

    <!-- Job list -->
    <div v-else-if="jobs.length" class="space-y-3">
      <div
        v-for="job in jobs"
        :key="job.name + job.type + job.last"
        class="group flex items-center justify-between rounded-xl border border-slate-200 bg-white p-5 cursor-pointer transition-colors hover:border-kipper-300 dark:border-slate-800 dark:bg-slate-900 dark:hover:border-kipper-700"
        @click="selectedJob = job"
      >
        <div class="flex min-w-0 items-center gap-4">
          <span class="inline-flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-slate-100 dark:bg-slate-800">
            <Timer v-if="job.type === 'cronjob'" class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
            <Play v-else class="h-5 w-5 text-kipper-500" :stroke-width="1.75" />
          </span>
          <div class="min-w-0">
            <div class="flex flex-wrap items-center gap-2">
              <span class="text-sm font-semibold text-slate-900 dark:text-slate-50">{{ job.name }}</span>
              <span class="rounded-full bg-slate-100 px-2 py-0.5 text-xs font-medium text-slate-600 dark:bg-slate-800 dark:text-slate-400">
                {{ job.type }}
              </span>
            </div>
            <div class="mt-0.5 flex items-center gap-3 text-xs text-slate-500 dark:text-slate-400">
              <span v-if="job.schedule" class="flex shrink-0 items-center gap-1">
                <Clock class="h-3 w-3" />
                {{ job.schedule }}
              </span>
              <span v-if="job.image" class="truncate font-mono">{{ job.image }}</span>
            </div>
          </div>
        </div>

        <div class="flex items-center gap-4">
          <span class="text-xs text-slate-500 dark:text-slate-400">{{ job.last }}</span>
          <span class="inline-flex items-center gap-1.5 text-xs" :class="statusColor(job.status)">
            <span class="inline-block h-2 w-2 rounded-full" :class="statusDot(job.status)" />
            {{ job.status }}
          </span>
        </div>
      </div>
    </div>

    <!-- Empty state -->
    <div v-else class="rounded-xl border border-dashed border-slate-300 py-16 text-center dark:border-slate-700">
      <Timer class="mx-auto mb-3 h-10 w-10 text-slate-400 dark:text-slate-500" :stroke-width="1.5" />
      <p class="text-sm font-medium text-slate-900 dark:text-slate-50">No jobs configured</p>
      <p class="mt-1 max-w-sm mx-auto text-sm text-slate-500 dark:text-slate-400">
        Click <strong>New job</strong> to create a scheduled or one-off job.
      </p>
    </div>

    <!-- Job detail panel -->
    <JobDetail
      v-if="selectedJob"
      :job-name="selectedJob.name"
      :namespace="selectedJob.namespace"
      :job-type="selectedJob.type"
      :schedule="selectedJob.schedule"
      @close="selectedJob = null"
      @refresh="loadJobs"
    />
  </div>
</template>
