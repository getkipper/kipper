import { ref, watchEffect } from 'vue'

const isDark = ref(localStorage.getItem('kipper_theme') === 'dark')

watchEffect(() => {
  document.documentElement.classList.toggle('dark', isDark.value)
  localStorage.setItem('kipper_theme', isDark.value ? 'dark' : 'light')
})

export function useDarkMode() {
  function toggle() {
    isDark.value = !isDark.value
  }

  return { isDark, toggle }
}
