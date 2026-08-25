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

// The route group carries its own namespace, so the editor should read from
// that namespace instead of the top-level selector. Otherwise the operator
// has to change the top selector before every edit — and if they have no
// permission to list "All projects" contents, the empty dropdown looks like
// their edit rights were revoked.
describe('editing a route while All projects is selected', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  it('lists the group\'s apps, not an empty list', async () => {
    setActivePinia(createPinia())
    useProjectsStore().globalNamespace = ''

    vi.mocked(routesApi.fetchRoutes).mockResolvedValue([ROUTE_GROUP] as never)
    vi.mocked(appsApi.fetchApps).mockImplementation(async (ns: string) => {
      if (ns === 'shop-test') return [{ name: 'webapp' }, { name: 'email-service' }] as never
      return [] as never
    })

    const wrapper = mount(Routes, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
    await flushPromises()
    await openEditor(wrapper)

    expect(vi.mocked(appsApi.fetchApps)).toHaveBeenCalledWith('shop-test')
    expect(optionNames()).toContain('email-service')
    expect(document.body.innerHTML).not.toContain('routes-no-project')
  })

  it('saves the update against the group\'s namespace', async () => {
    setActivePinia(createPinia())
    useProjectsStore().globalNamespace = ''

    vi.mocked(routesApi.fetchRoutes).mockResolvedValue([ROUTE_GROUP] as never)
    vi.mocked(appsApi.fetchApps).mockResolvedValue([{ name: 'webapp' }, { name: 'order-service' }] as never)
    vi.mocked(routesApi.updateRouteGroup).mockResolvedValue({
      host: ROUTE_GROUP.host, url: `https://${ROUTE_GROUP.host}`, mappings: [],
    } as never)

    const wrapper = mount(Routes, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
    await flushPromises()
    await openEditor(wrapper)
    await (wrapper.vm as unknown as { saveForm: () => Promise<void> }).saveForm()

    expect(vi.mocked(routesApi.updateRouteGroup)).toHaveBeenCalledWith(
      'shop-test',
      expect.objectContaining({ host: ROUTE_GROUP.host }),
    )
  })

  it('deletes against the group\'s namespace', async () => {
    setActivePinia(createPinia())
    useProjectsStore().globalNamespace = ''

    vi.mocked(routesApi.fetchRoutes).mockResolvedValue([ROUTE_GROUP] as never)
    vi.mocked(appsApi.fetchApps).mockResolvedValue([] as never)
    vi.mocked(routesApi.deleteRouteGroup).mockResolvedValue(undefined as never)

    const wrapper = mount(Routes, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
    await flushPromises()
    await (wrapper.vm as unknown as { handleDelete: (g: unknown) => Promise<void> }).handleDelete(ROUTE_GROUP)

    expect(vi.mocked(routesApi.deleteRouteGroup)).toHaveBeenCalledWith('shop-test', ROUTE_GROUP.host)
  })
})

// Clicking the pencil on a route way down the page used to open the editor at
// the top with no visual cue — an operator would click Edit and think nothing
// had happened. The editor scrolls itself into view when it opens.
describe('the route editor opening', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  it('scrolls the form into view so the click has a visible effect', async () => {
    const scrollSpy = vi.fn()
    // happy-dom doesn't implement scrollIntoView, so we polyfill and spy in
    // one step. Every Element the code touches lands here.
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
      configurable: true,
      value: scrollSpy,
    })

    const wrapper = await mountRoutes('shop-test', [{ name: 'webapp' }])
    await openEditor(wrapper)

    expect(scrollSpy).toHaveBeenCalled()
  })

  // The scroll is a UI action. If it is sequenced behind the app-list request
  // then a slow or hung backend brings back bug 3 — the click still looks
  // like it did nothing.
  it('scrolls even while the app-list request is still in flight', async () => {
    const scrollSpy = vi.fn()
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
      configurable: true,
      value: scrollSpy,
    })

    setActivePinia(createPinia())
    // Start with no top-level namespace so the initial load doesn't call
    // fetchApps; the only call comes from openEdit, which is the one we want
    // to hang.
    useProjectsStore().globalNamespace = ''

    vi.mocked(routesApi.fetchRoutes).mockResolvedValue([ROUTE_GROUP] as never)
    let resolveApps: (apps: { name: string }[]) => void = () => {}
    vi.mocked(appsApi.fetchApps).mockImplementation(() =>
      new Promise<{ name: string }[]>(res => { resolveApps = res }) as never,
    )

    const wrapper = mount(Routes, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
    await flushPromises()
    scrollSpy.mockClear()

    // openEdit only awaits the reveal, not the app-list request, so this
    // returns even though fetchApps is still pending.
    await (wrapper.vm as unknown as { openEdit: (g: unknown) => Promise<void> }).openEdit(ROUTE_GROUP)

    expect(scrollSpy).toHaveBeenCalled()

    // Release the pending promise so the test does not leak it.
    resolveApps([])
    await flushPromises()
  })
})

// Overlapping openEdit calls: the first request has not yet returned when a
// second one is fired. If the slow one wins the race back, it would overwrite
// the newer namespace's apps with the older one's — and the dropdown would
// silently misrepresent the project being edited.
describe('overlapping loadApps calls', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  it('drops the response from a superseded request', async () => {
    setActivePinia(createPinia())
    useProjectsStore().globalNamespace = ''

    const groupA = { ...ROUTE_GROUP, host: 'a.example.com', namespace: 'project-a' }
    const groupB = { ...ROUTE_GROUP, host: 'b.example.com', namespace: 'project-b' }

    vi.mocked(routesApi.fetchRoutes).mockResolvedValue([groupA, groupB] as never)

    let resolveA: (apps: { name: string }[]) => void = () => {}
    let resolveB: (apps: { name: string }[]) => void = () => {}
    vi.mocked(appsApi.fetchApps).mockImplementation((ns: string) => {
      if (ns === 'project-a') return new Promise<{ name: string }[]>(r => { resolveA = r }) as never
      if (ns === 'project-b') return new Promise<{ name: string }[]>(r => { resolveB = r }) as never
      return Promise.resolve([] as { name: string }[]) as never
    })

    const wrapper = mount(Routes, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
    await flushPromises()

    const vm = wrapper.vm as unknown as { openEdit: (g: unknown) => Promise<void> }
    // Open A, then immediately open B without letting A's fetchApps settle.
    void vm.openEdit(groupA)
    await flushPromises()
    void vm.openEdit(groupB)
    await flushPromises()

    // B's response arrives first and populates the dropdown.
    resolveB([{ name: 'b-app' }])
    await flushPromises()

    // A's late response arrives after the request was superseded. It must not
    // overwrite B's apps.
    resolveA([{ name: 'a-app' }])
    await flushPromises()

    expect(optionNames()).toContain('b-app')
    expect(optionNames()).not.toContain('a-app')
  })
})

// The top project selector governs the routes list. While the editor is open
// its own `formNamespace` is what the dropdown and Save/Delete should read
// from, so the selector switching mid-edit must not overwrite the apps list
// the operator is choosing from.
describe('the route editor while the top selector changes', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  it('does not refetch apps when the top selector moves under an open editor', async () => {
    setActivePinia(createPinia())
    const projects = useProjectsStore()
    projects.globalNamespace = 'shop-test'

    vi.mocked(routesApi.fetchRoutes).mockResolvedValue([ROUTE_GROUP] as never)
    vi.mocked(appsApi.fetchApps).mockResolvedValue([{ name: 'webapp' }] as never)

    const wrapper = mount(Routes, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
    await flushPromises()
    await openEditor(wrapper)

    const before = vi.mocked(appsApi.fetchApps).mock.calls.length
    projects.setGlobalNamespace('other-project')
    await flushPromises()

    expect(vi.mocked(appsApi.fetchApps).mock.calls.length).toBe(before)
  })

  // Refresh reloads routes and, when the form is closed, also refreshes apps
  // for the top-level namespace. With the form open the same call would
  // replace the edited group's dropdown, so the invariant belongs on this
  // path too.
  it('does not refresh apps from the top selector when Refresh runs with the editor open', async () => {
    setActivePinia(createPinia())
    useProjectsStore().globalNamespace = 'top-project'

    vi.mocked(routesApi.fetchRoutes).mockResolvedValue([ROUTE_GROUP] as never)
    vi.mocked(appsApi.fetchApps).mockImplementation((ns: string) => {
      if (ns === 'shop-test') return Promise.resolve([{ name: 'group-app' }]) as never
      return Promise.resolve([{ name: 'top-app' }]) as never
    })

    const wrapper = mount(Routes, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
    await flushPromises()
    await openEditor(wrapper)
    await flushPromises()

    await (wrapper.vm as unknown as { loadRoutes: () => Promise<void> }).loadRoutes()
    await flushPromises()

    expect(optionNames()).toContain('group-app')
    expect(optionNames()).not.toContain('top-app')
  })
})

// A pending fetch is not the same as an empty project. Rendering "This
// project has no apps yet" during that interval tells the operator something
// the code cannot yet know, and it stays wrong for the whole wait.
describe('while the apps fetch is pending', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  it('shows a loading state, not the empty-project message', async () => {
    setActivePinia(createPinia())
    useProjectsStore().globalNamespace = 'shop-test'

    vi.mocked(routesApi.fetchRoutes).mockResolvedValue([ROUTE_GROUP] as never)
    let resolveApps: (apps: { name: string }[]) => void = () => {}
    vi.mocked(appsApi.fetchApps).mockImplementation(() =>
      new Promise<{ name: string }[]>(r => { resolveApps = r }) as never,
    )

    const wrapper = mount(Routes, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
    await flushPromises()
    await openEditor(wrapper)
    await flushPromises()

    expect(document.body.innerHTML).toContain('routes-apps-loading')
    expect(document.body.innerHTML).not.toContain('routes-no-apps')

    resolveApps([])
    await flushPromises()

    expect(document.body.innerHTML).not.toContain('routes-apps-loading')
    expect(document.body.innerHTML).toContain('routes-no-apps')
  })
})

// A new namespace fetch begins but has not yet returned. The previous
// namespace's apps must not remain selectable during that window — an
// operator picking one would save an app that does not exist in the
// namespace the update writes to.
describe('while a new namespace fetch is in flight', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  it('clears the previous apps as soon as the new fetch starts', async () => {
    setActivePinia(createPinia())
    useProjectsStore().globalNamespace = ''

    const groupA = { ...ROUTE_GROUP, host: 'a.example.com', namespace: 'project-a' }
    const groupB = { ...ROUTE_GROUP, host: 'b.example.com', namespace: 'project-b' }

    vi.mocked(routesApi.fetchRoutes).mockResolvedValue([groupA, groupB] as never)

    let resolveB: (apps: { name: string }[]) => void = () => {}
    vi.mocked(appsApi.fetchApps).mockImplementation((ns: string) => {
      if (ns === 'project-a') return Promise.resolve([{ name: 'a-app' }]) as never
      if (ns === 'project-b') return new Promise<{ name: string }[]>(r => { resolveB = r }) as never
      return Promise.resolve([]) as never
    })

    const wrapper = mount(Routes, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
    await flushPromises()

    const vm = wrapper.vm as unknown as { openEdit: (g: unknown) => Promise<void> }
    await vm.openEdit(groupA)
    await flushPromises()
    expect(optionNames()).toContain('a-app')

    await vm.openEdit(groupB)
    await flushPromises()
    // groupB's fetch is still pending. a-app must not still be selectable.
    expect(optionNames()).not.toContain('a-app')

    resolveB([{ name: 'b-app' }])
    await flushPromises()
    expect(optionNames()).toContain('b-app')
  })
})
