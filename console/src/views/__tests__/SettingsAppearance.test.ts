// @vitest-environment happy-dom
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, shallowMount } from '@vue/test-utils'
import Settings from '../Settings.vue'
import * as appearanceApi from '@/api/appearance'
import * as favicon from '@/utils/favicon'

const toastSpies = { success: vi.fn(), error: vi.fn(), info: vi.fn(), dismiss: vi.fn(), toasts: { value: [] } }
vi.mock('@/composables/useToast', () => ({ useToast: () => toastSpies }))

vi.mock('@/api/appearance', () => ({
  getAppearance: vi.fn(),
  updateAppearance: vi.fn(),
}))

// The palette and its tints stay real, so the swatches under test are the ones
// the console actually offers. Only the DOM side effect is stubbed.
vi.mock('@/utils/favicon', async importOriginal => ({
  ...(await importOriginal<typeof favicon>()),
  applyFaviconColour: vi.fn().mockResolvedValue(true),
}))

vi.mock('@/api/ai-settings', () => ({
  getAISettings: vi.fn().mockResolvedValue({ provider: '', api_key: '', model: '', ollama_url: '' }),
  updateAISettings: vi.fn(),
}))
vi.mock('@/api/mode', () => ({
  getMode: vi.fn().mockResolvedValue({ mode: 'auto' }),
  updateMode: vi.fn(),
  getResourceLog: vi.fn().mockResolvedValue([]),
}))
vi.mock('@/api/slack', () => ({
  getSlackSettings: vi.fn().mockResolvedValue({ webhook_url: '' }),
  updateSlackSettings: vi.fn(),
}))
vi.mock('@/api/alertDelivery', () => ({ getAlertDelivery: vi.fn().mockResolvedValue({}) }))
vi.mock('@/api/smtp', () => ({
  getSmtpSettings: vi.fn().mockResolvedValue({}),
  updateSmtpSettings: vi.fn(),
  testSmtpSettings: vi.fn(),
}))
vi.mock('@/api/registry', () => ({
  fetchRegistries: vi.fn().mockResolvedValue([]),
  fetchRegistryHealth: vi.fn().mockResolvedValue({}),
  addRegistry: vi.fn(),
  removeRegistry: vi.fn(),
}))
vi.mock('@/api/git-credentials', () => ({
  fetchGitCredentials: vi.fn().mockResolvedValue([]),
  fetchGitCredentialHealth: vi.fn().mockResolvedValue({}),
  addGitCredential: vi.fn(),
  removeGitCredential: vi.fn(),
}))
vi.mock('@/api/client', () => ({
  default: { get: vi.fn().mockResolvedValue({ data: {} }), put: vi.fn(), post: vi.fn(), delete: vi.fn() },
}))

const getAppearance = vi.mocked(appearanceApi.getAppearance)
const updateAppearance = vi.mocked(appearanceApi.updateAppearance)
const applyFaviconColour = vi.mocked(favicon.applyFaviconColour)

async function mountSettings(current = 'blue') {
  getAppearance.mockResolvedValue({ faviconColour: current })
  const wrapper = shallowMount(Settings)
  await flushPromises()
  return wrapper
}

function swatch(wrapper: ReturnType<typeof shallowMount>, colour: string) {
  const button = wrapper.find(`button[aria-label="${colour}"]`)
  expect(button.exists(), `no swatch for ${colour}`).toBe(true)
  return button
}

describe('Settings, the appearance section', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    applyFaviconColour.mockResolvedValue(true)
  })

  it('offers a swatch for every colour the console can draw', async () => {
    const wrapper = await mountSettings()
    for (const colour of favicon.FAVICON_COLOURS) swatch(wrapper, colour)
    expect(wrapper.findAll('button[aria-pressed]')).toHaveLength(favicon.FAVICON_COLOURS.length)
  })

  it('marks the colour the cluster already has', async () => {
    const wrapper = await mountSettings('red')
    expect(swatch(wrapper, 'red').attributes('aria-pressed')).toBe('true')
    expect(swatch(wrapper, 'blue').attributes('aria-pressed')).toBe('false')
  })

  it('saves a chosen colour and paints the tab with it', async () => {
    updateAppearance.mockResolvedValue({ faviconColour: 'green' })
    const wrapper = await mountSettings()

    await swatch(wrapper, 'green').trigger('click')
    await flushPromises()

    expect(updateAppearance).toHaveBeenCalledWith('green')
    expect(applyFaviconColour).toHaveBeenCalledWith('green')
    expect(swatch(wrapper, 'green').attributes('aria-pressed')).toBe('true')
  })

  it('puts the old colour back when the cluster refuses the write', async () => {
    updateAppearance.mockRejectedValue(new Error('nope'))
    const wrapper = await mountSettings('purple')

    await swatch(wrapper, 'orange').trigger('click')
    await flushPromises()

    expect(swatch(wrapper, 'purple').attributes('aria-pressed')).toBe('true')
    expect(swatch(wrapper, 'orange').attributes('aria-pressed')).toBe('false')
    expect(applyFaviconColour).not.toHaveBeenCalled()
  })

  it('says the tab was updated only when the icon actually moved', async () => {
    updateAppearance.mockResolvedValue({ faviconColour: 'green' })
    applyFaviconColour.mockResolvedValue(false)
    const wrapper = await mountSettings()

    await swatch(wrapper, 'green').trigger('click')
    await flushPromises()

    expect(toastSpies.success).not.toHaveBeenCalled()
    expect(toastSpies.info).toHaveBeenCalled()
    expect(swatch(wrapper, 'green').attributes('aria-pressed')).toBe('true')
  })

  it('does not write again when the colour already showing is clicked', async () => {
    const wrapper = await mountSettings('pink')

    await swatch(wrapper, 'pink').trigger('click')
    await flushPromises()

    expect(updateAppearance).not.toHaveBeenCalled()
  })

  it('falls back to blue when the cluster cannot say what colour it is', async () => {
    getAppearance.mockRejectedValue(new Error('offline'))
    const wrapper = shallowMount(Settings)
    await flushPromises()

    expect(swatch(wrapper, 'blue').attributes('aria-pressed')).toBe('true')
  })
})
