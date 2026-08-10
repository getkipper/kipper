// @vitest-environment happy-dom
import { describe, it, expect, beforeEach } from 'vitest'
import { nextTick } from 'vue'
import { useDarkMode } from '../useDarkMode'

describe('useDarkMode', () => {
  beforeEach(() => {
    localStorage.clear()
    document.documentElement.classList.remove('dark')
  })

  it('defaults to light mode', () => {
    const { isDark } = useDarkMode()
    expect(isDark.value).toBe(false)
  })

  it('toggles between light and dark', async () => {
    const { isDark, toggle } = useDarkMode()
    expect(isDark.value).toBe(false)

    toggle()
    await nextTick()
    expect(isDark.value).toBe(true)
    expect(localStorage.getItem('kipper_theme')).toBe('dark')

    toggle()
    await nextTick()
    expect(isDark.value).toBe(false)
    expect(localStorage.getItem('kipper_theme')).toBe('light')
  })

  it('persists preference to localStorage', async () => {
    const { toggle } = useDarkMode()
    toggle()
    await nextTick()
    expect(localStorage.getItem('kipper_theme')).toBe('dark')
    expect(document.documentElement.classList.contains('dark')).toBe(true)
  })
})
