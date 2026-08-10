<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ChevronDown } from 'lucide-vue-next'

export interface Tab {
  key: string
  label: string
}

interface Props {
  tabs: Tab[]
  modelValue: string
  density?: 'compact' | 'comfortable'
  ariaLabel?: string
}

const props = withDefaults(defineProps<Props>(), {
  density: 'comfortable',
  ariaLabel: 'Tabs',
})

const emit = defineEmits<{
  'update:modelValue': [key: string]
}>()

const container = ref<HTMLDivElement | null>(null)
const tabRefs = ref<HTMLButtonElement[]>([])
const chevronWrap = ref<HTMLDivElement | null>(null)
const popover = ref<HTMLDivElement | null>(null)

const hiddenKeys = ref<Set<string>>(new Set())
const popoverOpen = ref(false)

const tabClass = computed(() =>
  props.density === 'compact'
    ? 'shrink-0 whitespace-nowrap px-4 py-2.5 text-xs font-medium capitalize transition-colors'
    : 'shrink-0 whitespace-nowrap px-5 py-2.5 text-sm font-medium capitalize transition-colors',
)

const chevronWidth = computed(() => (props.density === 'compact' ? 36 : 40))

const hiddenTabs = computed(() => props.tabs.filter((t) => hiddenKeys.value.has(t.key)))

const activeIsHidden = computed(() => hiddenKeys.value.has(props.modelValue))

function selectTab(key: string) {
  emit('update:modelValue', key)
  popoverOpen.value = false
}

// Measure visible tabs against the container width and mark the overflow.
// All tab buttons are always rendered; the hidden ones get display:none.
// We keep visual order so the user's mental map of tab positions stays stable.
function recompute() {
  const root = container.value
  if (!root) return
  const available = root.clientWidth
  if (available <= 0) return

  // First reset to all-visible so we can measure unrestricted widths.
  hiddenKeys.value = new Set()
  void nextTick().then(() => {
    if (!container.value) return
    const widths = tabRefs.value.map((el) => el?.offsetWidth ?? 0)
    const total = widths.reduce((a, b) => a + b, 0)
    if (total <= available) return

    // We will need the chevron. Reserve space for it.
    const budget = available - chevronWidth.value
    let running = 0
    const next = new Set<string>()
    for (let i = 0; i < props.tabs.length; i++) {
      const w = widths[i] ?? 0
      if (running + w <= budget) {
        running += w
      } else {
        next.add(props.tabs[i].key)
      }
    }
    hiddenKeys.value = next
  })
}

let ro: ResizeObserver | null = null

function startObserving() {
  if (!container.value) return
  if (typeof ResizeObserver !== 'undefined') {
    ro = new ResizeObserver(() => recompute())
    ro.observe(container.value)
  } else {
    window.addEventListener('resize', recompute)
  }
}

function stopObserving() {
  if (ro) {
    ro.disconnect()
    ro = null
  } else {
    window.removeEventListener('resize', recompute)
  }
}

function onDocClick(e: MouseEvent) {
  if (!popoverOpen.value) return
  const target = e.target as Node
  if (popover.value?.contains(target)) return
  if (chevronWrap.value?.contains(target)) return
  popoverOpen.value = false
}

function onDocKey(e: KeyboardEvent) {
  if (e.key === 'Escape' && popoverOpen.value) {
    popoverOpen.value = false
    ;(chevronWrap.value?.querySelector('button') as HTMLButtonElement | null)?.focus()
  }
}

onMounted(() => {
  recompute()
  startObserving()
  document.addEventListener('mousedown', onDocClick)
  document.addEventListener('keydown', onDocKey)
})

onBeforeUnmount(() => {
  stopObserving()
  document.removeEventListener('mousedown', onDocClick)
  document.removeEventListener('keydown', onDocKey)
})

watch(
  () => props.tabs.map((t) => t.key).join('|'),
  () => recompute(),
)
watch(() => props.density, () => recompute())
</script>

<template>
  <div ref="container" class="relative flex border-b border-slate-200 dark:border-slate-800" role="tablist" :aria-label="ariaLabel">
    <button
      v-for="(tab, i) in tabs"
      :ref="el => { if (el) tabRefs[i] = el as HTMLButtonElement }"
      :key="tab.key"
      :data-tab-key="tab.key"
      role="tab"
      :aria-selected="modelValue === tab.key"
      :tabindex="modelValue === tab.key ? 0 : -1"
      @click="selectTab(tab.key)"
      :class="[
        tabClass,
        modelValue === tab.key
          ? 'border-b-2 border-kipper-500 text-kipper-600 dark:text-kipper-400'
          : 'text-slate-500 hover:text-slate-700 dark:text-slate-400 dark:hover:text-slate-200',
        hiddenKeys.has(tab.key) ? 'hidden' : '',
      ]"
    >
      {{ tab.label }}
    </button>

    <div
      v-if="hiddenTabs.length > 0"
      ref="chevronWrap"
      class="ml-auto flex items-center"
      data-testid="tabbar-overflow"
    >
      <button
        type="button"
        :aria-label="`Show ${hiddenTabs.length} more ${hiddenTabs.length === 1 ? 'tab' : 'tabs'}`"
        :aria-expanded="popoverOpen"
        aria-haspopup="menu"
        @click="popoverOpen = !popoverOpen"
        class="flex h-full items-center px-2 transition-colors"
        :class="activeIsHidden
          ? 'border-b-2 border-kipper-500 text-kipper-600 dark:text-kipper-400'
          : 'text-slate-500 hover:text-slate-700 dark:text-slate-400 dark:hover:text-slate-200'"
      >
        <ChevronDown class="h-4 w-4" :stroke-width="2" />
      </button>

      <div
        v-if="popoverOpen"
        ref="popover"
        role="menu"
        data-testid="tabbar-overflow-menu"
        class="absolute right-0 top-full z-20 mt-1 min-w-[10rem] rounded-md border border-slate-200 bg-white py-1 shadow-lg dark:border-slate-700 dark:bg-slate-900"
      >
        <button
          v-for="tab in hiddenTabs"
          :key="tab.key"
          role="menuitem"
          @click="selectTab(tab.key)"
          class="block w-full px-3 py-2 text-left text-sm capitalize transition-colors"
          :class="modelValue === tab.key
            ? 'bg-kipper-50 font-medium text-kipper-700 dark:bg-kipper-950 dark:text-kipper-300'
            : 'text-slate-700 hover:bg-slate-50 dark:text-slate-200 dark:hover:bg-slate-800'"
        >
          {{ tab.label }}
        </button>
      </div>
    </div>
  </div>
</template>
