<script setup lang="ts">
import { ref, computed, nextTick, onMounted, onBeforeUnmount, watch } from 'vue'
import { X, Maximize2, Minimize2 } from 'lucide-vue-next'
import { useModal } from '@/composables/useModal'

interface Props {
  open: boolean
  // Accessible dialog name announced when focus moves into the panel
  label?: string
  defaultWidth?: number
  minWidth?: number
  maxWidth?: number
  fullscreenToggle?: boolean
}

const props = withDefaults(defineProps<Props>(), {
  defaultWidth: 672,
  minWidth: 400,
  maxWidth: 1600,
  fullscreenToggle: false,
})

const emit = defineEmits<{
  close: []
}>()

const panelWidth = ref(props.defaultWidth)
const isFullscreen = ref(false)
const isDragging = ref(false)
const panelRef = ref<HTMLElement | null>(null)
const modal = useModal()

const viewportWidth = ref(typeof window !== 'undefined' ? window.innerWidth : props.defaultWidth)
function onResize() {
  viewportWidth.value = window.innerWidth
}

// Cap the panel at the viewport so it fills a narrow screen instead of
// overflowing off the right edge on a phone.
const effectiveWidth = computed(() => Math.min(panelWidth.value, viewportWidth.value))

function onKeydown(e: KeyboardEvent) {
  if (modal.visible.value) return
  if (e.key === 'Escape' && props.open) {
    emit('close')
  }
}

function onClickOutside(e: MouseEvent) {
  if (modal.visible.value) return
  if (!props.open || isDragging.value) return
  if (panelRef.value && !panelRef.value.contains(e.target as Node)) {
    emit('close')
  }
}

function startDrag(e: MouseEvent) {
  e.preventDefault()
  isDragging.value = true
  document.body.style.userSelect = 'none'
  const overlay = document.createElement('div')
  overlay.style.cssText = 'position:fixed;inset:0;z-index:9999;cursor:col-resize;'
  document.body.appendChild(overlay)

  const onMove = (ev: MouseEvent) => {
    const newWidth = window.innerWidth - ev.clientX
    // Clamp to the viewport so the panel can't be dragged wider than the
    // screen or below a min that would itself overflow a narrow screen.
    const maxW = Math.min(props.maxWidth, window.innerWidth)
    const minW = Math.min(props.minWidth, window.innerWidth)
    panelWidth.value = Math.min(maxW, Math.max(minW, newWidth))
  }

  const onUp = () => {
    isDragging.value = false
    document.body.style.userSelect = ''
    overlay.remove()
    document.removeEventListener('mousemove', onMove)
    document.removeEventListener('mouseup', onUp)
  }

  document.addEventListener('mousemove', onMove)
  document.addEventListener('mouseup', onUp)
}

function toggleFullscreen() {
  isFullscreen.value = !isFullscreen.value
}

watch(() => props.open, (val) => {
  if (val) {
    isFullscreen.value = false
    nextTick(() => panelRef.value?.focus())
  }
})

onMounted(() => {
  document.addEventListener('keydown', onKeydown)
  document.addEventListener('mousedown', onClickOutside)
  window.addEventListener('resize', onResize)
  // Move focus into the dialog so Esc and screen readers land in the panel
  // rather than the page behind it.
  panelRef.value?.focus()
})

onBeforeUnmount(() => {
  document.removeEventListener('keydown', onKeydown)
  document.removeEventListener('mousedown', onClickOutside)
  window.removeEventListener('resize', onResize)
})
</script>

<template>
  <Teleport to="body">
    <!-- Backdrop -->
    <Transition name="fade">
      <div v-if="open" class="fixed inset-0 z-40 bg-black/20" />
    </Transition>

    <!-- Panel -->
    <Transition name="slide">
      <!-- Non-modal dialog: the page behind stays interactive and any click
           outside dismisses the panel, so aria-modal would overpromise. -->
      <div
        v-if="open"
        ref="panelRef"
        role="dialog"
        :aria-label="label"
        tabindex="-1"
        class="fixed inset-y-0 right-0 z-50 flex flex-col border-l border-slate-200 bg-white shadow-xl outline-none dark:border-slate-800 dark:bg-slate-900"
        :style="{ width: isFullscreen ? '100vw' : `${effectiveWidth}px` }"
      >
        <!-- Resize handle -->
        <div
          v-if="!isFullscreen"
          class="absolute inset-y-0 left-0 z-20 flex w-2 cursor-col-resize items-center hover:bg-kipper-500/10"
          @mousedown="startDrag"
        >
          <div class="mx-auto h-12 w-0.5 rounded-full bg-slate-300 dark:bg-slate-600" />
        </div>

        <!-- Header -->
        <div class="flex items-center justify-between border-b border-slate-200 px-5 py-4 dark:border-slate-800">
          <div class="flex-1 min-w-0">
            <slot name="header" />
          </div>
          <div class="flex items-center gap-1 ml-2">
            <slot name="actions" />
            <button
              v-if="fullscreenToggle"
              @click="toggleFullscreen"
              class="rounded-lg p-1.5 text-slate-400 hover:bg-slate-100 hover:text-slate-700 dark:hover:bg-slate-800"
              :title="isFullscreen ? 'Exit fullscreen' : 'Fullscreen'"
            >
              <Minimize2 v-if="isFullscreen" class="h-4 w-4" />
              <Maximize2 v-else class="h-4 w-4" />
            </button>
            <button
              @click="emit('close')"
              class="rounded-lg p-1.5 text-slate-400 hover:bg-slate-100 hover:text-slate-700 dark:hover:bg-slate-800"
              title="Close (Esc)"
            >
              <X class="h-5 w-5" />
            </button>
          </div>
        </div>

        <!-- Content -->
        <div class="flex flex-1 flex-col min-h-0 overflow-hidden">
          <slot />
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.fade-enter-active,
.fade-leave-active {
  transition: opacity 0.15s ease;
}
.fade-enter-from,
.fade-leave-to {
  opacity: 0;
}
.slide-enter-active,
.slide-leave-active {
  transition: transform 0.2s ease;
}
.slide-enter-from,
.slide-leave-to {
  transform: translateX(100%);
}
</style>
