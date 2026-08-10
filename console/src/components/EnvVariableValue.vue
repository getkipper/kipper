<script setup lang="ts">
import { computed } from 'vue'
import { AlertTriangle, CornerDownRight, Info } from 'lucide-vue-next'

import type { EnvPreviewReference, EnvPreviewVariable } from '@/api/apps'
import { parseTemplate, shellStyleRefs } from '@/utils/envTemplate'

interface Props {
  /** The value as written on the CR, references and all. */
  template: string
  /**
   * What the server made of it. Absent for a viewer, who is not served the
   * preview, and while it is still loading — the value itself still renders,
   * so the tab never waits on it.
   */
  preview?: EnvPreviewVariable
}
const props = defineProps<Props>()

const segments = computed(() => parseTemplate(props.template))

const referencesByName = computed(() => {
  const byName = new Map<string, EnvPreviewReference>()
  for (const ref of props.preview?.references ?? []) byName.set(ref.name, ref)
  return byName
})

/** A reference nothing defines reaches the process as written, so it is shown as a problem. */
function isUnresolved(name: string): boolean {
  const ref = referencesByName.value.get(name)
  return ref !== undefined && !ref.resolved
}

function referenceTitle(name: string): string {
  const ref = referencesByName.value.get(name)
  if (!ref) return name
  if (!ref.resolved) return `Nothing in this app's environment defines ${name}`
  const from = ref.source ? `${ref.origin} ${ref.source}` : ref.origin
  return ref.secret ? `${name} comes from ${from} and is masked here` : `${name} comes from ${from}`
}

const unresolved = computed(() =>
  (props.preview?.references ?? []).filter(r => !r.resolved).map(r => r.name),
)

const transitive = computed(() =>
  (props.preview?.references ?? []).filter(r => r.transitive).map(r => r.name),
)

/**
 * Falls back to parsing the value here when the preview has not arrived, so the
 * hint about $(NAME) shows for a viewer too. It costs nothing to work out and
 * it is the same grammar either way.
 */
const shellStyle = computed(() => props.preview?.shellStyle ?? shellStyleRefs(props.template))

const list = (names: string[]) => names.join(', ')
</script>

<template>
  <div class="min-w-0 flex-1">
    <!-- The value as written. A reference is marked so it reads as one thing
         rather than as punctuation the operator has to parse themselves. -->
    <div class="truncate font-mono text-xs text-slate-600 dark:text-slate-400">
      <template v-for="(segment, i) in segments" :key="i">
        <span v-if="segment.kind === 'literal'">{{ segment.text }}</span>
        <span
          v-else
          :title="referenceTitle(segment.name)"
          class="rounded px-0.5"
          :class="isUnresolved(segment.name)
            ? 'bg-amber-100 text-amber-800 dark:bg-amber-500/20 dark:text-amber-300'
            : 'bg-kipper-100 text-kipper-800 dark:bg-kipper-500/20 dark:text-kipper-300'"
        >{{ segment.text }}</span>
      </template>
    </div>

    <!-- What this resolves to. Deliberately not described as what the pod is
         running: a source can change before the reconciler republishes, and the
         restart banner does not close that window because it compares the last
         published environment against the pods rather than against these
         sources.

         Keyed on isTemplate rather than on the string having content, so a
         template resolving to nothing shows that it resolved to nothing.
         Secret-derived parts arrive masked from the server. -->
    <div
      v-if="preview?.isTemplate"
      class="mt-1 flex items-start gap-1 text-xs text-slate-500 dark:text-slate-400"
    >
      <CornerDownRight class="mt-0.5 h-3 w-3 shrink-0 text-slate-400" />
      <span class="shrink-0 text-slate-400">resolves to</span>
      <span v-if="preview.resolved" class="truncate font-mono">{{ preview.resolved }}</span>
      <span v-else class="italic text-slate-400">an empty value</span>
    </div>

    <p v-if="unresolved.length" class="mt-1 flex items-start gap-1 text-xs text-amber-600 dark:text-amber-400">
      <AlertTriangle class="mt-0.5 h-3 w-3 shrink-0" />
      <span>
        Nothing in this app's environment defines {{ list(unresolved) }}, so the reference reaches
        your app as written.
      </span>
    </p>

    <p v-if="preview?.shadowedBy" class="mt-1 flex items-start gap-1 text-xs text-amber-600 dark:text-amber-400">
      <AlertTriangle class="mt-0.5 h-3 w-3 shrink-0" />
      <span>
        {{ preview.shadowedBy }} sets this variable too and wins, so your app never sees this value.
      </span>
    </p>

    <p v-if="transitive.length" class="mt-1 flex items-start gap-1 text-xs text-amber-600 dark:text-amber-400">
      <AlertTriangle class="mt-0.5 h-3 w-3 shrink-0" />
      <span>
        {{ list(transitive) }} is itself a template. Values resolve in a single pass, so the
        reference inside it stays as written.
      </span>
    </p>

    <p v-if="shellStyle.length" class="mt-1 flex items-start gap-1 text-xs text-slate-500 dark:text-slate-400">
      <Info class="mt-0.5 h-3 w-3 shrink-0" />
      <span>
        Kipper resolves <code class="font-mono">${{ '{' }}{{ shellStyle[0] }}{{ '}' }}</code>, so
        <code class="font-mono">$({{ shellStyle[0] }})</code> reaches your app unchanged.
      </span>
    </p>
  </div>
</template>
