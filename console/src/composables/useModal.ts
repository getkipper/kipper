import { ref, markRaw, type Component } from 'vue'

const visible = ref(false)
const component = ref<Component | null>(null)
const props = ref<Record<string, unknown>>({})

export function useModal() {
  function open(comp: Component, compProps: Record<string, unknown> = {}) {
    component.value = markRaw(comp)
    props.value = compProps
    visible.value = true
  }

  function close() {
    visible.value = false
    component.value = null
    props.value = {}
  }

  return { visible, component, props, open, close }
}
