// @vitest-environment happy-dom
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import Routes from '../Routes.vue'
import * as appsApi from '@/api/apps'
import * as routesApi from '@/api/routes'
import { useProjectsStore } from '@/stores/projects'

vi.mock('@/api/client', () => ({
  default: {
    get: vi.fn().mockResolvedValue({ data: {} }),
    put: vi.fn().mockResolvedValue({ data: {} }),
    post: vi.fn().mockResolvedValue({ data: {} }),
    delete: vi.fn().mockResolvedValue({ data: {} }),
  },
}))
vi.mock('@/api/apps')
vi.mock('@/api/routes')

const HEALTH = { ingress_ready: true, tls_ready: true, message: '' }

const ROUTE_GROUP = {
  name: 'shop-webapp-test',
  namespace: 'shop-test',
  host: 'shop-webapp-test.example.com',
  tls: true,
  project: 'shop',
  environment: 'test',
  health: HEALTH,
  routes: [
    { path: '/', service: 'webapp', app: 'webapp', port: 3000, health: HEALTH },
    { path: '/cart-api/', service: 'order-service', app: 'order-service', port: 8080, health: HEALTH },
  ],
}

async function mountRoutes(namespace: string, apps: { name: string }[] | Error) {
  setActivePinia(createPinia())
  useProjectsStore().globalNamespace = namespace

  vi.mocked(routesApi.fetchRoutes).mockResolvedValue([ROUTE_GROUP] as never)
  if (apps instanceof Error) {
    vi.mocked(appsApi.fetchApps).mockRejectedValue(apps)
  } else {
    vi.mocked(appsApi.fetchApps).mockResolvedValue(apps as never)
  }

  const wrapper = mount(Routes, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
  await flushPromises()
  return wrapper
}

// The value each mapping's dropdown is actually showing. A select whose value
// has no matching option reports an empty string, which is the blank the
// operator saw.
function selectedApps(): string[] {
  return [...document.querySelectorAll<HTMLSelectElement>('select')].map(s => s.value)
}

function optionNames(): string[] {
  return [...document.querySelectorAll<HTMLOptionElement>('select option')].map(o => o.value).filter(Boolean)
}

// Open the editor the way the pencil does.
async function openEditor(wrapper: Awaited<ReturnType<typeof mountRoutes>>) {
  ;(wrapper.vm as unknown as { openEdit: (g: unknown) => void }).openEdit(ROUTE_GROUP)
  await flushPromises()
}

describe('the route editor with nothing to choose from', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  // What a new operator meets: localStorage carries no project, so the global
  // selector is "All projects", the app list is empty, and nothing said why.
  it('says a project has to be picked before apps can be listed', async () => {
    const wrapper = await mountRoutes('', [])
    await openEditor(wrapper)

    expect(document.body.innerHTML).toContain('routes-no-project')
    expect(document.body.textContent).toContain('Pick a project')
  })

  // A denied or failed list is not the same as an empty one, and rendering
  // both as a blank dropdown is what made this look like a permissions bug.
  it('separates a failure to list apps from there being none', async () => {
    const wrapper = await mountRoutes('shop-test', new Error('403 forbidden'))
    await openEditor(wrapper)

    expect(document.body.innerHTML).toContain('routes-apps-error')
    expect(document.body.innerHTML).not.toContain('routes-no-apps')
  })

  it('says so plainly when the project genuinely has no apps', async () => {
    const wrapper = await mountRoutes('shop-test', [])
    await openEditor(wrapper)

    expect(document.body.innerHTML).toContain('routes-no-apps')
  })

  // The alarming part: a select whose value has no matching option renders
  // blank, so an existing route group looked like it had lost its mappings.
  it('still shows what a mapping points at when the app list did not load', async () => {
    const wrapper = await mountRoutes('shop-test', new Error('403 forbidden'))
    await openEditor(wrapper)

    // Read the selects themselves. The names also appear in the route list
    // below the editor, so asserting on the page text passes even when every
    // dropdown is blank — which is the bug.
    expect(selectedApps()).toEqual(['webapp', 'order-service'])
  })

  it('offers the project\'s apps when the list did load', async () => {
    const wrapper = await mountRoutes('shop-test', [{ name: 'webapp' }, { name: 'email-service' }])
    await openEditor(wrapper)

    expect(optionNames()).toContain('email-service')
    expect(document.body.innerHTML).not.toContain('routes-no-apps')
  })
})
