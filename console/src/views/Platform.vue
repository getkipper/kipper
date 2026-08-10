<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { AlertTriangle, CheckCircle2, Loader2 } from 'lucide-vue-next'
import NoticeCallout from '@/components/NoticeCallout.vue'
import PlatformComponentCard from '@/components/PlatformComponentCard.vue'
import { usePlatformStore } from '@/stores/platform'
import { getAIBundleStatus, type BundleStatusReport } from '@/api/ai-settings'

const store = usePlatformStore()

const aiBundle = ref<BundleStatusReport | null>(null)
const aiBundleError = ref<string | null>(null)

async function loadAIBundleStatus() {
  try {
    aiBundle.value = await getAIBundleStatus()
  } catch (err) {
    aiBundleError.value = err instanceof Error ? err.message : 'failed to load AI bundle status'
  }
}

function bundleLabel(key: 'phase1' | 'rag'): string {
  return key === 'phase1' ? 'Phase 1 (LibreChat + Ollama)' : 'RAG (AnythingLLM + Qdrant)'
}

onMounted(() => {
  store.loadComponents()
  loadAIBundleStatus()
})

function profileBlurb(profile: string): string {
  switch (profile) {
    case 'nano':
      return 'Sub-4 GB box. Metrics off. Good for demos and dev.'
    case 'small':
      return '4-8 GB box. Functional but tight; apps share the room.'
    case 'medium':
      return '8-16 GB box. Real workloads, sensible defaults.'
    case 'large':
      return '16-32 GB box. Comfortable headroom for app teams.'
    case 'xlarge':
      return 'Over 32 GB. Production with many services.'
    default:
      return ''
  }
}
</script>

<template>
  <div class="mx-auto max-w-5xl space-y-6 p-6">
    <header>
      <h1 class="text-2xl font-semibold tracking-tight text-slate-900 dark:text-slate-50">Platform</h1>
      <p class="mt-1 text-sm text-slate-600 dark:text-slate-400">
        Memory and lifecycle for the cluster's system components. The platform reconciler picks up changes here and re-rolls the underlying Helm releases.
      </p>
    </header>

    <section class="rounded-lg border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
      <div class="flex items-start justify-between gap-4">
        <div>
          <div class="text-xs font-medium uppercase tracking-wide text-slate-500 dark:text-slate-400">Active profile</div>
          <div class="mt-1 text-xl font-semibold capitalize text-slate-900 dark:text-slate-50">
            {{ store.profile || 'unknown' }}
          </div>
          <p class="mt-1 text-xs text-slate-500 dark:text-slate-400">{{ profileBlurb(store.profile) }}</p>
        </div>
        <div class="text-xs text-slate-500 dark:text-slate-400">
          Profile is set at install time. Edit the <code class="rounded bg-slate-100 px-1.5 py-0.5 text-[11px] font-mono text-slate-700 dark:bg-slate-800 dark:text-slate-300">PlatformConfig</code> CR or run <code class="rounded bg-slate-100 px-1.5 py-0.5 text-[11px] font-mono text-slate-700 dark:bg-slate-800 dark:text-slate-300">kip platform profile set &lt;name&gt;</code> to change it.
        </div>
      </div>
    </section>

    <div v-if="store.loading" class="flex items-center justify-center py-12 text-slate-500 dark:text-slate-400">
      <Loader2 class="mr-2 h-5 w-5 animate-spin" /> Loading…
    </div>

    <NoticeCallout v-else-if="store.error" tone="danger" class="p-4 text-sm text-rose-700 dark:text-slate-300">
      {{ store.error }}
    </NoticeCallout>

    <div v-else class="grid grid-cols-1 gap-4 md:grid-cols-2">
      <PlatformComponentCard
        v-for="component in store.components"
        :key="component.name"
        :component="component"
      />
    </div>

    <p v-if="!store.loading && !store.components.length" class="text-sm text-slate-500 dark:text-slate-400">
      No platform components reported. If this cluster was installed before the platform sizing feature shipped, run <code>kip upgrade</code> to register the PlatformConfig CR.
    </p>

    <section v-if="aiBundle" class="rounded-lg border border-slate-200 bg-white p-5 dark:border-slate-800 dark:bg-slate-900">
      <header class="mb-3 flex items-baseline justify-between gap-3">
        <h2 class="text-base font-semibold text-slate-900 dark:text-slate-50">AI bundle health</h2>
        <span class="text-xs text-slate-500 dark:text-slate-400">
          Driven by the <code class="rounded bg-slate-100 px-1.5 py-0.5 text-[11px] font-mono text-slate-700 dark:bg-slate-800 dark:text-slate-300">kipper-ai-bundle-state</code> / <code class="rounded bg-slate-100 px-1.5 py-0.5 text-[11px] font-mono text-slate-700 dark:bg-slate-800 dark:text-slate-300">kipper-rag-bundle-state</code> ConfigMaps.
        </span>
      </header>

      <div class="space-y-3">
        <div
          v-for="key in (['phase1', 'rag'] as const)"
          :key="key"
          class="flex items-start gap-3 rounded-md border p-3 dark:rounded-l-none"
          :class="aiBundle[key].installed && aiBundle[key].missing && aiBundle[key].missing.length
            ? 'border-rose-200 bg-rose-50 dark:border-slate-800 dark:bg-slate-900 dark:shadow-[inset_3px_0_0_theme(colors.rose.400)]'
            : 'border-slate-200 bg-slate-50 dark:border-slate-800 dark:bg-slate-900/50'"
        >
          <div class="pt-0.5">
            <AlertTriangle
              v-if="aiBundle[key].installed && aiBundle[key].missing && aiBundle[key].missing.length"
              class="h-5 w-5 text-rose-600 dark:text-rose-400"
            />
            <CheckCircle2 v-else class="h-5 w-5 text-emerald-600 dark:text-emerald-400" />
          </div>
          <div class="flex-1">
            <div class="text-sm font-medium text-slate-900 dark:text-slate-50">{{ bundleLabel(key) }}</div>
            <div v-if="!aiBundle[key].installed" class="mt-0.5 text-xs text-slate-500 dark:text-slate-400">
              Not installed. Run <code class="rounded bg-slate-100 px-1.5 py-0.5 font-mono text-slate-700 dark:bg-slate-800 dark:text-slate-300">kip ai {{ key === 'rag' ? 'rag ' : '' }}install</code> to enable.
            </div>
            <div v-else-if="aiBundle[key].missing && aiBundle[key].missing.length" class="mt-1">
              <div class="text-xs font-medium text-rose-700 dark:text-rose-300">{{ aiBundle[key].missing!.length }} expected workload(s) missing:</div>
              <ul class="mt-1 list-disc pl-5 text-xs text-rose-700 dark:text-slate-400">
                <li v-for="r in aiBundle[key].missing!" :key="`${r.kind}/${r.name}`">
                  <code>{{ r.kind }}/{{ r.name }}</code> in <code>{{ r.namespace }}</code>
                </li>
              </ul>
              <div class="mt-2 text-xs text-rose-700 dark:text-slate-400">
                Re-run <code class="rounded bg-rose-100 px-1.5 py-0.5 font-mono dark:bg-slate-800 dark:text-slate-300">kip ai {{ key === 'rag' ? 'rag ' : '' }}install</code> to reconcile.
              </div>
            </div>
            <div v-else class="mt-0.5 text-xs text-emerald-700 dark:text-emerald-300">
              Installed, all workloads present.
            </div>
          </div>
        </div>
      </div>
    </section>

    <NoticeCallout v-else-if="aiBundleError" tone="warning" class="p-3 text-xs text-amber-800 dark:text-slate-300">
      Could not load AI bundle status: {{ aiBundleError }}
    </NoticeCallout>
  </div>
</template>
