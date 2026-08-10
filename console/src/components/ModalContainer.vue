<script setup lang="ts">
import { ref, computed, watch, nextTick, onMounted, onBeforeUnmount } from 'vue'
import { useModal } from '@/composables/useModal'

const { visible, component, props: modalProps, close } = useModal()

// Name the dialog for screen readers. Modals opened through useModal carry a
// `title` prop (see ConfirmDialog); fall back to a generic label otherwise.
const dialogLabel = computed(() => {
  const title = modalProps.value.title
  return typeof title === 'string' ? title : 'Dialog'
})

const overlayRef = ref<HTMLElement | null>(null)
const mouseDownTarget = ref<EventTarget | null>(null)
// The element focused before the modal opened, so focus can return there.
let previouslyFocused: HTMLElement | null = null

function onMousedown(e: MouseEvent) {
  mouseDownTarget.value = e.target
}

function onMouseup(e: MouseEvent) {
  if (mouseDownTarget.value === e.currentTarget && e.target === e.currentTarget) {
    close()
  }
  mouseDownTarget.value = null
}

function focusableElements(): HTMLElement[] {
  if (!overlayRef.value) return []
  const selector =
    'a[href], button:not([disabled]), textarea:not([disabled]), input:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])'
  return Array.from(overlayRef.value.querySelectorAll<HTMLElement>(selector))
}

// Keep Tab within the modal so keyboard focus can't wander to the page behind it.
function trapTab(e: KeyboardEvent) {
  const focusable = focusableElements()
  if (focusable.length === 0) {
    e.preventDefault()
    return
  }
  const first = focusable[0]
  const last = focusable[focusable.length - 1]
  const active = document.activeElement
  if (e.shiftKey && (active === first || active === overlayRef.value)) {
    e.preventDefault()
    last.focus()
  } else if (!e.shiftKey && active === last) {
    e.preventDefault()
    first.focus()
  }
}

// Capture at the document level so this fires before SidePanel's listener.
function onDocKeydown(e: KeyboardEvent) {
  if (!visible.value) return
  if (e.key === 'Escape') {
    e.stopImmediatePropagation()
    close()
  } else if (e.key === 'Tab') {
    trapTab(e)
  }
}

watch(visible, (isOpen) => {
  if (isOpen) {
    previouslyFocused = document.activeElement as HTMLElement | null
    nextTick(() => {
      const focusable = focusableElements()
      ;(focusable[0] ?? overlayRef.value)?.focus()
    })
  } else if (previouslyFocused) {
    previouslyFocused.focus()
    previouslyFocused = null
  }
})

onMounted(() => {
  document.addEventListener('keydown', onDocKeydown, true)
})

onBeforeUnmount(() => {
  document.removeEventListener('keydown', onDocKeydown, true)
})
</script>

<template>
  <Transition name="modal-fade">
    <div
      v-if="visible && component"
      ref="overlayRef"
      role="dialog"
      aria-modal="true"
      :aria-label="dialogLabel"
      tabindex="-1"
      class="fixed inset-0 z-[100] flex items-center justify-center bg-black/40 p-8 focus:outline-none"
      @mousedown="onMousedown"
      @mouseup="onMouseup"
    >
      <component :is="component" v-bind="modalProps" @close="close" />
    </div>
  </Transition>
</template>

<style scoped>
.modal-fade-enter-active,
.modal-fade-leave-active {
  transition: opacity 0.15s ease;
}
.modal-fade-enter-from,
.modal-fade-leave-to {
  opacity: 0;
}
</style>
