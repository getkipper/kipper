<script setup lang="ts">
import { computed, ref } from 'vue'
import { ChevronDown, ChevronRight, KeyRound } from 'lucide-vue-next'

import type { EnvPreviewName, EnvPreviewSnippet } from '@/api/apps'

interface Props {
  available: EnvPreviewName[]
  snippets?: EnvPreviewSnippet[]
}
const props = defineProps<Props>()

const emit = defineEmits<{
  /** A name to drop into the value being edited, as a ${NAME} reference. */
  (e: 'insert', reference: string): void
  /** A whole starter variable: key and value together. */
  (e: 'use-snippet', snippet: EnvPreviewSnippet): void
}>()

const open = ref(false)

/**
 * Grouped by where a variable comes from, because that is what an operator is
 * looking for: the five names a binding injects sit together under the service
 * that injects them, rather than scattered through one alphabetical list.
 */
const groups = computed(() => {
  const bySource = new Map<string, { label: string; names: EnvPreviewName[] }>()
  for (const entry of props.available) {
    const label = entry.source ? `${entry.origin} · ${entry.source}` : entry.origin
    const group = bySource.get(label) ?? { label, names: [] }
    group.names.push(entry)
    bySource.set(label, group)
  }
  return [...bySource.values()].sort((a, b) => a.label.localeCompare(b.label))
})
</script>

<template>
  <div class="rounded-lg border border-slate-200 dark:border-slate-700">
    <button
      @click="open = !open"
      class="flex w-full items-center gap-1.5 px-3 py-2 text-left text-xs font-medium text-slate-600 hover:bg-slate-50 dark:text-slate-400 dark:hover:bg-slate-800"
    >
      <ChevronDown v-if="open" class="h-3.5 w-3.5" />
      <ChevronRight v-else class="h-3.5 w-3.5" />
      Variables you can reference
      <span class="text-slate-400">({{ available.length }})</span>
    </button>

    <div v-if="open" class="space-y-3 border-t border-slate-200 px-3 py-2.5 dark:border-slate-700">
      <p class="text-xs text-slate-500 dark:text-slate-400">
        Write <code class="font-mono">${{ '{' }}NAME{{ '}' }}</code> in a value to reference one of
        these. Kipper resolves it when it renders your app's environment, so the credential stays
        out of the variable you type. Add <code class="font-mono">:urlencode</code> when the value
        goes inside a URL.
      </p>

      <div v-for="snippet in snippets" :key="snippet.service" class="rounded-md bg-slate-50 p-2 dark:bg-slate-800">
        <div class="mb-1 flex items-center justify-between gap-2">
          <span class="text-xs font-medium text-slate-600 dark:text-slate-400">
            {{ snippet.type }} · {{ snippet.service }}
          </span>
          <button
            @click="emit('use-snippet', snippet)"
            class="shrink-0 rounded-md bg-kipper-600 px-2 py-0.5 text-[11px] text-white hover:bg-kipper-700"
          >
            Use this
          </button>
        </div>
        <div class="truncate font-mono text-[11px] text-slate-500 dark:text-slate-400">
          {{ snippet.key }}={{ snippet.value }}
        </div>
      </div>

      <div v-for="group in groups" :key="group.label">
        <div class="mb-1 text-[11px] font-medium uppercase tracking-wide text-slate-400">
          {{ group.label }}
        </div>
        <div class="flex flex-wrap gap-1">
          <button
            v-for="entry in group.names"
            :key="entry.name"
            @click="emit('insert', `\${${entry.name}}`)"
            :title="entry.secret ? `${entry.name} is a credential. Referencing it keeps the value out of this variable.` : `Insert a reference to ${entry.name}`"
            class="flex items-center gap-1 rounded-md border border-slate-200 bg-white px-1.5 py-0.5 font-mono text-[11px] text-slate-600 hover:border-kipper-400 hover:text-kipper-700 dark:border-slate-600 dark:bg-slate-900 dark:text-slate-400 dark:hover:text-kipper-400"
          >
            <KeyRound v-if="entry.secret" class="h-3 w-3 text-amber-500" />
            {{ entry.name }}
          </button>
        </div>
      </div>
    </div>
  </div>
</template>
