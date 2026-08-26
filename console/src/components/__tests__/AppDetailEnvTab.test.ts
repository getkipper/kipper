// @vitest-environment happy-dom
import { capabilitiesForRole } from '@/utils/testCapabilities'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import AppDetail from '../AppDetail.vue'
import * as appsApi from '@/api/apps'
import * as servicesApi from '@/api/services'
import * as databaseApi from '@/api/database'
import * as projectsApi from '@/api/projects'
import { useAuthStore } from '@/stores/auth'
import { useProjectsStore } from '@/stores/projects'
import type { Capability, Project, ProjectRole } from '@/api/projects'

// Nothing in this test may reach the network. AppDetail loads build status and
// logs on mount as well as the env tab's own data, and the point here is which
// controls render for which role, not what any of that returns.
vi.mock('@/api/client', () => ({
  default: {
    get: vi.fn().mockResolvedValue({ data: {} }),
    put: vi.fn().mockResolvedValue({ data: {} }),
    post: vi.fn().mockResolvedValue({ data: {} }),
    delete: vi.fn().mockResolvedValue({ data: {} }),
  },
}))
vi.mock('@/api/apps')
vi.mock('@/api/services')
vi.mock('@/api/database')
vi.mock('@/api/files')
vi.mock('@/api/mode')
vi.mock('@/api/projects', async importOriginal => ({
  ...(await importOriginal<typeof projectsApi>()),
  fetchProjects: vi.fn(),
}))

// AppDetail loads Loki logs on mount, and the Logs tab renders them. The module
// is auto-mocked, so without this fetchLogs resolves undefined, the computed
// that joins them throws, and the run fails on an unhandled rejection rather
// than on any assertion — which is exactly how it reached CI unnoticed.
beforeEach(() => {
  vi.mocked(appsApi.fetchLogs).mockResolvedValue([])
})

// Markers that actually render. An earlier version of this test looked for
// handler names like "handleBind", which Vue compiles into bound listeners and
// never emits into the DOM — so it passed with every guard removed.
//
// Each of these needs its data seeded in mountEnvTab, or it does not render for
// a deployer either and the assertion proves nothing about the role. Five of
// them were in exactly that state: the fixtures had no services, links,
// injected variables or conflicts, so bind, link, unbind, unlink and the
// conflict fix were absent for a viewer whatever the guards said.
//
// They are grouped by the capability the route behind them takes, because the
// three do not travel together: a role can carry env.write without kipper.write
// and the console then has to offer one set and withhold the other.
const ENV_WRITE_MARKERS = [
  'placeholder="LOG_LEVEL"',  // the add-variable form
  'title="Edit"',
  'title="Delete"',
  'Remove direct entries',    // the conflicts fix
  'Variables you can reference', // the available-variables panel
]
const KIPPER_WRITE_MARKERS = [
  'Bind a service...',        // the service binding form
  'Link an app...',           // the app linking form
  'title="Unbind service"',
  'title="Unlink"',
  'title="AI Diagnose"',      // header toolbar
  'title="Update image"',
]
const RESTART_MARKERS = [
  'Restart to apply',         // the restart banner's button
  'title="Rolling restart"',  // header toolbar
]
const WRITE_MARKERS = [...ENV_WRITE_MARKERS, ...KIPPER_WRITE_MARKERS, ...RESTART_MARKERS]

function projectWithRole(role: ProjectRole, capabilities?: Capability[]): Project {
  return {
    name: 'shop', role, capabilities: capabilities ?? capabilitiesForRole(role), env_limit: 3,
    environments: [
      { name: 'prod', namespace: 'shop-prod', apps: [], status: 'active', order: '0', owned: true },
      { name: 'test', namespace: 'shop-test', apps: [], status: 'active', order: '1', owned: true },
    ],
  }
}

async function mountEnvTab(role: ProjectRole, clusterRole: string, capabilities?: Capability[]) {
  setActivePinia(createPinia())
  useAuthStore().role = clusterRole
  useProjectsStore().projects = [projectWithRole(role, capabilities)]

  // Everything a write control needs in order to render at all. Without these
  // the absence of a control says nothing about the role that asked for it.
  vi.mocked(appsApi.fetchEnv).mockResolvedValue({ LOG_LEVEL: 'debug', DATABASE_URL: 'postgres://…' })
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue(['DATABASE_URL'])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(true)
  vi.mocked(appsApi.fetchApps).mockResolvedValue([
    { name: 'api', status: 'running', image: 'nginx', replicas: 1, ready: 1 },
    { name: 'worker', status: 'running', image: 'nginx', replicas: 1, ready: 1 },
  ])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([
    { app: 'worker', namespace: 'shop-prod', envVar: 'WORKER_URL', url: 'http://worker', open: true, injected: true },
  ])
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([
    { name: 'DATABASE_URL', service: 'db', secret: true },
  ])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([
    { name: 'cache', namespace: 'shop-prod', type: 'redis', status: 'Running', ready: '1/1', storage: '1Gi' },
  ])
  // The preview route is deployer-gated, so a viewer is genuinely refused it.
  if (role === 'viewer' && clusterRole === 'viewer') {
    vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  } else {
    vi.mocked(appsApi.fetchEnvPreview).mockResolvedValue({
      variables: [],
      available: [
        { name: 'DATABASE_URL', origin: 'service', source: 'db', secret: true },
        { name: 'CACHE_HOST', origin: 'service', source: 'cache', secret: false },
      ],
      snippets: [],
    })
  }

  const wrapper = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  return wrapper
}

// Enter the tab the way the TabBar does: setActiveTab assigns activeTab, and a
// watcher on it runs the loaders. Two flushes because those loaders await each
// other — one leaves the links and bound services unresolved, so the controls
// that need them are absent for a reason that has nothing to do with the role.
async function openEnvTab(w: Awaited<ReturnType<typeof mountEnvTab>>) {
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  await flushPromises()
}

// The panel teleports to body, so wrapper.html() is only the placeholder.
// Reading that instead of the document is how the first version of the
// viewer test passed while asserting nothing at all.
function rendered(): string {
  return document.body.innerHTML
}

describe('the Env tab and the project role', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    // Each mount attaches to the document, and a teleported panel is not
    // removed by the next mount. Without this the second test reads the first
    // test's DOM, which is how a deployer appeared to be shown a viewer's
    // read-only note.
    document.body.innerHTML = ''
  })

  it('offers the tab to a project viewer, who could always read it through the API', async () => {
    const w = await mountEnvTab('viewer', 'viewer')
    const tabs = (w.vm as unknown as { visibleTabs: { key: string }[] }).visibleTabs
    expect(tabs.map(t => t.key)).toContain('env')
  })

  it('shows a project viewer no control that writes', async () => {
    const w = await mountEnvTab('viewer', 'viewer')
    await openEnvTab(w)
    const html = rendered()
    for (const marker of WRITE_MARKERS) {
      expect(html, `a viewer must not be offered: ${marker}`).not.toContain(marker)
    }
    expect(html, 'and is told why the controls are absent').toContain('which your role in this project does not carry')
  })

  // The preview route is deployer-gated, so a viewer is normally refused it and
  // the panel would be absent whatever the template said. Serve one anyway: the
  // insert buttons write into the add-variable form, so the client has to hide
  // them on its own rather than relying on the endpoint to stay gated.
  it('hides the reference panel from a viewer even when a preview arrives', async () => {
    const w = await mountEnvTab('viewer', 'viewer')
    vi.mocked(appsApi.fetchEnvPreview).mockResolvedValue({
      variables: [],
      available: [{ name: 'CACHE_HOST', origin: 'service', source: 'cache', secret: false }],
      snippets: [],
    })
    await openEnvTab(w)
    expect(rendered()).not.toContain('Variables you can reference')
  })

  // The server hands the cluster role an override only to an admin, so a
  // cluster deployer who is a viewer of this project gets 403 from every one of
  // these controls.
  it('shows a cluster deployer no write control in a project they only view', async () => {
    const w = await mountEnvTab('viewer', 'deployer')
    await openEnvTab(w)
    const html = rendered()
    for (const marker of WRITE_MARKERS) {
      expect(html, `a cluster deployer who is a project viewer must not be offered: ${marker}`).not.toContain(marker)
    }
  })

  // An admin is resolved as project owner by the server, so the controls are real.
  it('leaves a cluster admin the controls whatever the project says', async () => {
    const w = await mountEnvTab('viewer', 'admin')
    await openEnvTab(w)
    expect(rendered()).toContain('placeholder="LOG_LEVEL"')
  })

  // The server resolves a non-admin through project membership alone, so with
  // no membership to read there is nothing to offer — and a cluster deployer
  // was being shown the whole tab for the moment before the store loaded.
  it('offers a cluster deployer nothing while the project role is unknown', async () => {
    setActivePinia(createPinia())
    useAuthStore().role = 'deployer'
    useProjectsStore().projects = []
    vi.mocked(appsApi.fetchEnv).mockResolvedValue({})
    vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
    vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
    vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
    vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
    vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
    vi.mocked(appsApi.fetchApps).mockResolvedValue([])
    vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
    const w = mount(AppDetail, {
      props: { appName: 'api', namespace: 'shop-prod' },
      attachTo: document.body,
      global: { stubs: { RouterLink: true } },
    })
    await flushPromises()
    const tabs = (w.vm as unknown as { visibleTabs: { key: string }[] }).visibleTabs
    expect(tabs.map(t => t.key), 'no project role means no Env tab').not.toContain('env')
  })

  // An admin is resolved as project owner whatever the membership says.
  it('offers a cluster admin the tab with no project role at all', async () => {
    setActivePinia(createPinia())
    useAuthStore().role = 'admin'
    useProjectsStore().projects = []
    vi.mocked(appsApi.fetchEnv).mockResolvedValue({})
    vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
    vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
    vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
    vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
    vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
    vi.mocked(appsApi.fetchApps).mockResolvedValue([])
    vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
    const w = mount(AppDetail, {
      props: { appName: 'api', namespace: 'shop-prod' },
      attachTo: document.body,
      global: { stubs: { RouterLink: true } },
    })
    await flushPromises()
    const tabs = (w.vm as unknown as { visibleTabs: { key: string }[] }).visibleTabs
    expect(tabs.map(t => t.key)).toContain('env')
  })

  it('leaves a project deployer with the controls', async () => {
    const w = await mountEnvTab('deployer', 'viewer')
    await openEnvTab(w)
    // The same markers, present this time: the guard has to be permissive as
    // well as restrictive or it is just a broken tab.
    const html = rendered()
    for (const marker of WRITE_MARKERS) {
      expect(html, `a deployer must still get: ${marker}`).toContain(marker)
    }
    expect(html).not.toContain('which your role in this project does not carry')
  })
})

describe('links belong to the app the panel is showing', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  // Entering the Env tab and switching apps both start an unawaited load, so
  // the responses can land in either order. The older one must not win.
  it('discards a link response that arrives after the panel moved on', async () => {
    setActivePinia(createPinia())
    useAuthStore().role = 'deployer'
    useProjectsStore().projects = [projectWithRole('deployer')]

    vi.mocked(appsApi.fetchEnv).mockResolvedValue({})
    vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
    vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
    vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
    vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
    vi.mocked(appsApi.fetchApps).mockResolvedValue([])
    vi.mocked(servicesApi.fetchServices).mockResolvedValue([])

    let releaseSlowRequest: (v: appsApi.AppLink[]) => void = () => {}
    vi.mocked(appsApi.fetchLinks).mockImplementation((_p: string, app: string) => {
      if (app === 'api') {
        return new Promise<appsApi.AppLink[]>(resolve => { releaseSlowRequest = resolve })
      }
      return Promise.resolve([
        { app: 'queue', namespace: 'shop-prod', envVar: 'QUEUE_URL', url: 'http://queue', open: true, injected: true },
      ])
    })

    const w = mount(AppDetail, {
      props: { appName: 'api', namespace: 'shop-prod' },
      attachTo: document.body,
      global: { stubs: { RouterLink: true } },
    })
    await flushPromises()
    ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
    await flushPromises()

    // The panel moves to another app while api's links are still in flight.
    await w.setProps({ appName: 'worker' })
    await flushPromises()

    // api's answer lands last, and describes an app the panel is no longer on.
    releaseSlowRequest([
      { app: 'legacy', namespace: 'shop-prod', envVar: 'LEGACY_URL', url: 'http://legacy', open: true, injected: true },
    ])
    await flushPromises()

    const links = (w.vm as unknown as { links: appsApi.AppLink[] }).links
    expect(links.map(l => l.app), "a stale response must not replace the current app's links").toEqual(['queue'])
  })
})

describe('a load publishes only while it still describes the panel', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  function quietLoaders() {
    vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
    vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
    vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
    vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
    vi.mocked(appsApi.fetchApps).mockResolvedValue([])
    vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
    vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
  }

  async function openPanel(props: { appName: string; namespace: string }) {
    setActivePinia(createPinia())
    useAuthStore().role = 'deployer'
    useProjectsStore().projects = [
      { name: 'shop', role: 'deployer', capabilities: capabilitiesForRole('deployer'), env_limit: 3, environments: [
        { name: 'test', namespace: 'shop-test', apps: [], status: 'active', order: '0', owned: true },
        { name: 'prod', namespace: 'shop-prod', apps: [], status: 'active', order: '1', owned: true },
      ] } as Project,
    ]
    const w = mount(AppDetail, { props, attachTo: document.body, global: { stubs: { RouterLink: true } } })
    await flushPromises()
    ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
    await flushPromises()
    return w
  }

  // A -> B -> A. The captured app name is A again by the time the first request
  // lands, so a name comparison alone lets the oldest answer win.
  it('keeps the newest response when the panel returns to the app it left', async () => {
    quietLoaders()
    let releaseFirst: (v: appsApi.AppLink[]) => void = () => {}
    let call = 0
    vi.mocked(appsApi.fetchLinks).mockImplementation((_p: string, app: string) => {
      if (app !== 'api') return Promise.resolve([])
      call++
      if (call === 1) return new Promise<appsApi.AppLink[]>(resolve => { releaseFirst = resolve })
      return Promise.resolve([
        { app: 'newest', namespace: 'shop-test', envVar: 'NEW_URL', open: true, injected: true },
      ])
    })

    const w = await openPanel({ appName: 'api', namespace: 'shop-test' })
    await w.setProps({ appName: 'worker' })
    await flushPromises()
    await w.setProps({ appName: 'api' })
    await flushPromises()

    releaseFirst([{ app: 'oldest', namespace: 'shop-test', envVar: 'OLD_URL', open: true, injected: true }])
    await flushPromises()

    const links = (w.vm as unknown as { links: appsApi.AppLink[] }).links
    expect(links.map(l => l.app), 'the first request must not outrank the one that replaced it').toEqual(['newest'])
  })

  // App names repeat across environments, so the app name alone does not say
  // which app a response describes.
  it('discards a response from the namespace the panel has left', async () => {
    quietLoaders()
    let releaseTest: (v: Record<string, string>) => void = () => {}
    vi.mocked(appsApi.fetchEnv).mockImplementation((project: string) => {
      if (project === 'shop-test') {
        return new Promise<Record<string, string>>(resolve => { releaseTest = resolve })
      }
      return Promise.resolve({ ENVIRONMENT: 'prod' })
    })

    const w = await openPanel({ appName: 'api', namespace: 'shop-test' })
    // Same app name, other environment.
    await w.setProps({ namespace: 'shop-prod' })
    await flushPromises()

    releaseTest({ ENVIRONMENT: 'test', DATABASE_URL: 'postgres://test' })
    await flushPromises()

    const envVars = (w.vm as unknown as { envVars: Record<string, string> }).envVars
    expect(envVars, "test's variables must not be shown under the prod selection").toEqual({ ENVIRONMENT: 'prod' })
  })

  // The generation only advances when a new load starts, and the watchers start
  // the Env loaders only while the Env tab is the active one. Leave that tab
  // with a load in flight and switch app, and nothing supersedes it — the
  // captured app and namespace are all that stand between the answer and the
  // panel it no longer describes.
  it('discards a load left in flight when the tab was not reloaded', async () => {
    quietLoaders()
    let releaseFirst: (v: Record<string, string>) => void = () => {}
    let call = 0
    vi.mocked(appsApi.fetchEnv).mockImplementation(() => {
      call++
      if (call === 1) return new Promise<Record<string, string>>(resolve => { releaseFirst = resolve })
      return Promise.resolve({})
    })

    const w = await openPanel({ appName: 'api', namespace: 'shop-test' })
    // Leave the Env tab, so no later load starts and no generation advances.
    ;(w.vm as unknown as { activeTab: string }).activeTab = 'logs'
    await flushPromises()
    await w.setProps({ appName: 'worker', namespace: 'shop-prod' })
    await flushPromises()

    releaseFirst({ ONLY_ON_API: 'yes' })
    await flushPromises()

    expect((w.vm as unknown as { envVars: Record<string, string> }).envVars,
      'nothing superseded this load, so its own identity has to stop it').toEqual({})
  })

  // Reads and writes both publish the whole variable map, so they need one
  // order between them: a read that started before a save must not put the
  // pre-save map back, because the next edit sends that map as the new truth.
  it('does not let a read that started first undo a save', async () => {
    quietLoaders()
    let releaseRead: (v: Record<string, string>) => void = () => {}
    let reads = 0
    vi.mocked(appsApi.fetchEnv).mockImplementation(() => {
      reads++
      if (reads === 2) return new Promise<Record<string, string>>(resolve => { releaseRead = resolve })
      return Promise.resolve({ LOG_LEVEL: 'debug' })
    })
    vi.mocked(appsApi.updateEnv).mockResolvedValue({ LOG_LEVEL: 'debug', ADDED: 'yes' })

    const w = await openPanel({ appName: 'api', namespace: 'shop-test' })
    const vm = w.vm as unknown as {
      loadEnv: () => Promise<void>
      newEnvKey: string
      newEnvValue: string
      addEnvVar: () => Promise<void>
      envVars: Record<string, string>
    }

    // A refresh is in flight when the save goes out.
    const reading = vm.loadEnv()
    vm.newEnvKey = 'ADDED'
    vm.newEnvValue = 'yes'
    await vm.addEnvVar()
    expect(vm.envVars).toEqual({ LOG_LEVEL: 'debug', ADDED: 'yes' })

    releaseRead({ LOG_LEVEL: 'debug' })
    await reading
    await flushPromises()

    expect((w.vm as unknown as { envVars: Record<string, string> }).envVars,
      'the older read must not put the pre-save map back').toEqual({ LOG_LEVEL: 'debug', ADDED: 'yes' })
  })

  // The other interleaving: a read overlaps the save and is answered from
  // before it committed, then lands last. Starting later does not make it
  // newer, so ordering by what started cannot decide this one.
  it('does not let a read issued during a save resurrect what the save removed', async () => {
    quietLoaders()
    let releaseRead: (v: Record<string, string>) => void = () => {}
    let reads = 0
    vi.mocked(appsApi.fetchEnv).mockImplementation(() => {
      reads++
      // The first read fills the panel; the second is the one that overlaps.
      if (reads === 1) return Promise.resolve({ LOG_LEVEL: 'debug', DOOMED: 'x' })
      return new Promise<Record<string, string>>(resolve => { releaseRead = resolve })
    })

    let releaseWrite: (v: Record<string, string>) => void = () => {}
    vi.mocked(appsApi.updateEnv).mockImplementation(() =>
      new Promise<Record<string, string>>(resolve => { releaseWrite = resolve }))

    const w = await openPanel({ appName: 'api', namespace: 'shop-test' })
    const vm = w.vm as unknown as {
      deleteEnvVar: (k: string) => Promise<void>
      loadEnv: () => Promise<void>
      envVars: Record<string, string>
    }
    expect(vm.envVars).toEqual({ LOG_LEVEL: 'debug', DOOMED: 'x' })

    const deleting = vm.deleteEnvVar('DOOMED')
    // The operator leaves the tab and comes back while the PUT is in flight.
    const reading = vm.loadEnv()
    await flushPromises()

    // The PUT commits and publishes the stored map.
    releaseWrite({ LOG_LEVEL: 'debug' })
    await deleting
    await flushPromises()
    expect(vm.envVars).toEqual({ LOG_LEVEL: 'debug' })

    // The GET, answered before the commit, lands afterwards.
    releaseRead({ LOG_LEVEL: 'debug', DOOMED: 'x' })
    await reading
    await flushPromises()

    expect((w.vm as unknown as { envVars: Record<string, string> }).envVars,
      'a read that overlapped the save cannot bring the removed value back').toEqual({ LOG_LEVEL: 'debug' })
  })

  // A write publishes its response too. updateEnv returns the whole map, so a
  // save that lands after the panel moved would put the app it saved onto the
  // app now shown, and hand the next edit that map to send back.
  it('does not publish a write that resolved after the panel moved', async () => {
    quietLoaders()
    vi.mocked(appsApi.fetchEnv).mockImplementation((_p: string, app: string) => {
      const vars: Record<string, string> = app === 'api' ? { ONLY_ON_API: 'yes' } : { WORKER_QUEUE: 'jobs' }
      return Promise.resolve(vars)
    })

    let releaseWrite: (v: Record<string, string>) => void = () => {}
    vi.mocked(appsApi.updateEnv).mockImplementation(() =>
      new Promise<Record<string, string>>(resolve => { releaseWrite = resolve }))

    const w = await openPanel({ appName: 'api', namespace: 'shop-test' })
    const vm = w.vm as unknown as {
      newEnvKey: string
      newEnvValue: string
      addEnvVar: () => Promise<void>
      envVars: Record<string, string>
    }
    vm.newEnvKey = 'NEW_ONE'
    vm.newEnvValue = 'x'
    const writing = vm.addEnvVar()

    await w.setProps({ appName: 'worker' })
    await flushPromises()

    releaseWrite({ ONLY_ON_API: 'yes', NEW_ONE: 'x' })
    await writing
    await flushPromises()

    expect((w.vm as unknown as { envVars: Record<string, string> }).envVars,
      "api's saved map must not land on worker").toEqual({ WORKER_QUEUE: 'jobs' })
  })

  // updateEnv sends the whole map, so a stale map plus one edit replaces the
  // current app's configuration with another app's.
  it('keeps one app’s env out of another’s', async () => {
    quietLoaders()
    let releaseFirst: (v: Record<string, string>) => void = () => {}
    vi.mocked(appsApi.fetchEnv).mockImplementation((_p: string, app: string) => {
      if (app === 'api') return new Promise<Record<string, string>>(resolve => { releaseFirst = resolve })
      return Promise.resolve({ WORKER_QUEUE: 'jobs' })
    })

    const w = await openPanel({ appName: 'api', namespace: 'shop-test' })
    await w.setProps({ appName: 'worker' })
    await flushPromises()

    releaseFirst({ API_KEY_SOURCE: 'vault', ONLY_ON_API: 'yes' })
    await flushPromises()

    const envVars = (w.vm as unknown as { envVars: Record<string, string> }).envVars
    expect(envVars, "api's variables must not become worker's").toEqual({ WORKER_QUEUE: 'jobs' })
  })
})

describe('an operation in flight does not strand the panel it left', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  // The result of a restart belongs to the panel that asked for it, so it will
  // not clear the flag once the panel has moved. Something else has to, or the
  // button stays disabled for every app this panel goes on to show.
  it('re-enables restart after switching away mid-restart', async () => {
    setActivePinia(createPinia())
    useAuthStore().role = 'deployer'
    useProjectsStore().projects = [projectWithRole('deployer')]
    vi.mocked(appsApi.fetchEnv).mockResolvedValue({})
    vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
    vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
    vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
    vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
    vi.mocked(appsApi.fetchApps).mockResolvedValue([])
    vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
    vi.mocked(servicesApi.fetchServices).mockResolvedValue([])

    let releaseRestart: () => void = () => {}
    vi.mocked(appsApi.restartApp).mockImplementation(() =>
      new Promise<void>(resolve => { releaseRestart = () => resolve() }))

    const w = mount(AppDetail, {
      props: { appName: 'api', namespace: 'shop-prod' },
      attachTo: document.body,
      global: { stubs: { RouterLink: true } },
    })
    await flushPromises()
    const vm = w.vm as unknown as { handleRestart: () => Promise<void>; restarting: boolean }
    const restarting = vm.handleRestart()
    expect(vm.restarting).toBe(true)

    await w.setProps({ appName: 'worker' })
    await flushPromises()

    releaseRestart()
    await restarting
    await flushPromises()

    expect((w.vm as unknown as { restarting: boolean }).restarting,
      'the new app must not inherit a disabled restart button').toBe(false)
  })
})

// A handler that reloads after its own write advances the generation it took at
// the start, so the flag cannot be tied to that. It belongs to the panel.
it('re-enables the bind form after a successful bind', async () => {
  // Top level, so no describe resets the mock counts or the teleported DOM.
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'deployer'
  useProjectsStore().projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnv).mockResolvedValue({})
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([
    { name: 'cache', namespace: 'shop-prod', type: 'redis', status: 'Running', ready: '1/1', storage: '1Gi' },
  ])
  vi.mocked(servicesApi.bindService).mockResolvedValue(undefined as never)

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  const vm = w.vm as unknown as { bindingService: string; handleBind: () => Promise<void>; binding: boolean }
  vm.bindingService = 'cache'
  await flushPromises()
  await vm.handleBind()
  await flushPromises()

  expect((w.vm as unknown as { binding: boolean }).binding,
    'the bind button must come back after the bind it completed').toBe(false)
})

// An unrelated variable edit must not strand the bind button, which is what
// testing the shared env-write order here would do.
it('re-enables the bind form even when another env write is in flight', async () => {
  // Top level, so no describe resets the mock counts or the teleported DOM.
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'deployer'
  useProjectsStore().projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnv).mockResolvedValue({})
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([
    { name: 'cache', namespace: 'shop-prod', type: 'redis', status: 'Running', ready: '1/1', storage: '1Gi' },
  ])
  let releaseBind: () => void = () => {}
  vi.mocked(servicesApi.bindService).mockImplementation(() =>
    new Promise<void>(resolve => { releaseBind = () => resolve() }) as never)
  vi.mocked(appsApi.updateEnv).mockImplementation(() => new Promise(() => {}))

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  const vm = w.vm as unknown as {
    bindingService: string
    handleBind: () => Promise<void>
    binding: boolean
    newEnvKey: string
    newEnvValue: string
    addEnvVar: () => Promise<void>
  }
  vm.bindingService = 'cache'
  await flushPromises()
  const binding = vm.handleBind()
  // A variable save started while the bind is still running, so it is the newer
  // env write of the two.
  vm.newEnvKey = 'SLOW'
  vm.newEnvValue = 'x'
  void vm.addEnvVar()
  await flushPromises()

  releaseBind()
  await binding
  await flushPromises()

  expect((w.vm as unknown as { binding: boolean }).binding,
    'an unrelated save must not leave the bind button disabled').toBe(false)
})

// Same sequence, same trap: handleLink reloads through the guard it holds.
it('re-enables the link form after a successful link', async () => {
  // Top level, so no describe resets the mock counts or the teleported DOM.
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'deployer'
  useProjectsStore().projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnv).mockResolvedValue({})
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([
    { name: 'api', status: 'running', image: 'nginx', replicas: 1, ready: 1 },
    { name: 'worker', status: 'running', image: 'nginx', replicas: 1, ready: 1 },
  ])
  vi.mocked(appsApi.linkApp).mockResolvedValue({ target: 'worker', envVar: 'WORKER_URL' } as Awaited<ReturnType<typeof appsApi.linkApp>>)

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  const vm = w.vm as unknown as { linkingTarget: string; handleLink: () => Promise<void>; linking: boolean }
  vm.linkingTarget = 'worker'
  await vm.handleLink()
  await flushPromises()

  expect((w.vm as unknown as { linking: boolean }).linking,
    'the link button must come back after the link it completed').toBe(false)
})

// Deselecting the service invalidates the request in flight for the old one, so
// that request will not clear the spinner. The selection that invalidated it has
// to.
it('clears the database spinner when the service is deselected', async () => {
  // Top level, so no describe resets the mock counts or the teleported DOM.
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'deployer'
  useProjectsStore().projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnv).mockResolvedValue({})
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([
    { name: 'db', namespace: 'shop-prod', type: 'postgres', status: 'Running', ready: '1/1', storage: '5Gi' },
  ])
  // Never resolves: the request for db is still in flight when it is abandoned.
  vi.mocked(databaseApi.fetchDBSchema).mockImplementation(() => new Promise(() => {}))
  vi.mocked(servicesApi.fetchServiceInfo).mockImplementation(() => new Promise(() => {}))

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  // The service list loads on entering the tab, and the picker reads its type
  // from there.
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  const vm = w.vm as unknown as { bindingService: string; bindingDatabasesLoading: boolean }
  vm.bindingService = 'db'
  await flushPromises()
  expect(vm.bindingDatabasesLoading).toBe(true)

  vm.bindingService = ''
  await flushPromises()

  expect((w.vm as unknown as { bindingDatabasesLoading: boolean }).bindingDatabasesLoading,
    'the spinner must not outlive the selection it belonged to').toBe(false)
})

describe('whole-map writes are serialised', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  async function envPanel(props: { appName: string; namespace: string }) {
    setActivePinia(createPinia())
    useAuthStore().role = 'deployer'
    useProjectsStore().projects = [projectWithRole('deployer')]
    vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
    vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
    vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
    vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
    vi.mocked(appsApi.fetchApps).mockResolvedValue([])
    vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
    vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
    const w = mount(AppDetail, { props, attachTo: document.body, global: { stubs: { RouterLink: true } } })
    await flushPromises()
    ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
    await flushPromises()
    return w
  }

  // The handler retries on conflict, so of two overlapping PUTs the one sent
  // first can be the one committed last, carrying the older body. Only one is
  // in flight at a time, and the second builds on what the first returned.
  it('sends one map at a time, each built on the last answer', async () => {
    vi.mocked(appsApi.fetchEnv).mockResolvedValue({ BASE: '1' })
    const sent: Record<string, string>[] = []
    const release: Array<(v: Record<string, string>) => void> = []
    vi.mocked(appsApi.updateEnv).mockImplementation((_p: string, _a: string, vars: Record<string, string>) => {
      sent.push({ ...vars })
      return new Promise<Record<string, string>>(resolve => { release.push(resolve) })
    })

    const w = await envPanel({ appName: 'api', namespace: 'shop-test' })
    const vm = w.vm as unknown as {
      newEnvKey: string; newEnvValue: string
      addEnvVar: () => Promise<void>
      envVars: Record<string, string>
    }

    vm.newEnvKey = 'FIRST'; vm.newEnvValue = 'a'
    const first = vm.addEnvVar()
    await flushPromises()
    vm.newEnvKey = 'SECOND'; vm.newEnvValue = 'b'
    const second = vm.addEnvVar()
    await flushPromises()

    expect(sent.length, 'the second must wait its turn').toBe(1)
    expect(sent[0]).toEqual({ BASE: '1', FIRST: 'a' })

    release[0]({ BASE: '1', FIRST: 'a' })
    await first
    await flushPromises()

    expect(sent.length).toBe(2)
    expect(sent[1], 'the second builds on what the first returned').toEqual({ BASE: '1', FIRST: 'a', SECOND: 'b' })

    release[1]({ BASE: '1', FIRST: 'a', SECOND: 'b' })
    await second
    await flushPromises()
    expect(vm.envVars).toEqual({ BASE: '1', FIRST: 'a', SECOND: 'b' })
  })

  // A write abandoned on the way out returns during a later visit to the same
  // app, where comparing app and namespace cannot tell it apart from a write
  // this visit issued.
  it('survives a write abandoned by a panel that comes back', async () => {
    vi.mocked(appsApi.fetchEnv).mockImplementation((_p: string, app: string) => {
      const vars: Record<string, string> = app === 'api' ? { BASE: '1' } : {}
      return Promise.resolve(vars)
    })
    let releaseAbandoned: (v: Record<string, string>) => void = () => {}
    vi.mocked(appsApi.updateEnv).mockImplementation(() =>
      new Promise<Record<string, string>>(resolve => { releaseAbandoned = resolve }))

    const w = await envPanel({ appName: 'api', namespace: 'shop-test' })
    const vm = w.vm as unknown as {
      newEnvKey: string; newEnvValue: string
      addEnvVar: () => Promise<void>
      loadEnv: () => Promise<void>
      envVars: Record<string, string>
      envLoading: boolean
    }
    vm.newEnvKey = 'GONE'; vm.newEnvValue = 'x'
    const abandoned = vm.addEnvVar()
    await flushPromises()

    await w.setProps({ appName: 'worker' })
    await flushPromises()
    await w.setProps({ appName: 'api' })
    await flushPromises()

    releaseAbandoned({ BASE: '1', GONE: 'x' })
    await abandoned
    await flushPromises()

    // The counters must still be sane, so a fresh read publishes and settles.
    vi.mocked(appsApi.fetchEnv).mockResolvedValue({ BASE: '2' })
    await vm.loadEnv()
    await flushPromises()
    expect(vm.envVars, 'a later read must still be able to publish').toEqual({ BASE: '2' })
    expect(vm.envLoading, 'and must still clear the spinner').toBe(false)
  })

  // Standing aside for a write decides what a read may publish, not who owns
  // the spinner it turned on.
  it('clears the spinner for a read that stood aside', async () => {
    vi.mocked(appsApi.fetchEnv).mockResolvedValue({ BASE: '1' })
    vi.mocked(appsApi.updateEnv).mockImplementation(() => new Promise(() => {}))

    const w = await envPanel({ appName: 'api', namespace: 'shop-test' })
    const vm = w.vm as unknown as {
      newEnvKey: string; newEnvValue: string
      addEnvVar: () => Promise<void>
      loadEnv: () => Promise<void>
      envLoading: boolean
    }
    vm.newEnvKey = 'PENDING'; vm.newEnvValue = 'x'
    void vm.addEnvVar()
    await flushPromises()

    await vm.loadEnv()
    await flushPromises()

    expect((w.vm as unknown as { envLoading: boolean }).envLoading,
      'the tab must not read Loading... for good').toBe(false)
  })
})

describe('the write queue holds under the awkward cases', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  async function envPanel(props: { appName: string; namespace: string }) {
    setActivePinia(createPinia())
    useAuthStore().role = 'deployer'
    useProjectsStore().projects = [
      { name: 'shop', role: 'deployer', capabilities: capabilitiesForRole('deployer'), env_limit: 3, environments: [
        { name: 'test', namespace: 'shop-test', apps: [], status: 'active', order: '0', owned: true },
        { name: 'prod', namespace: 'shop-prod', apps: [], status: 'active', order: '1', owned: true },
      ] } as Project,
    ]
    vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
    vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
    vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
    vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
    vi.mocked(appsApi.fetchApps).mockResolvedValue([])
    vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
    vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
    const w = mount(AppDetail, { props, attachTo: document.body, global: { stubs: { RouterLink: true } } })
    await flushPromises()
    ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
    await flushPromises()
    return w
  }

  // A place in the queue has to be taken when the operation is created. Taken
  // after the first await, two mutations started in one turn — a double-clicked
  // button, a save and a delete — capture the same predecessor and both run.
  it('keeps two mutations started in the same turn from overlapping', async () => {
    vi.mocked(appsApi.fetchEnv).mockResolvedValue({ BASE: '1' })
    const sent: Record<string, string>[] = []
    const release: Array<(v: Record<string, string>) => void> = []
    vi.mocked(appsApi.updateEnv).mockImplementation((_p: string, _a: string, vars: Record<string, string>) => {
      sent.push({ ...vars })
      return new Promise<Record<string, string>>(resolve => { release.push(resolve) })
    })

    const w = await envPanel({ appName: 'api', namespace: 'shop-test' })
    const vm = w.vm as unknown as {
      newEnvKey: string; newEnvValue: string
      addEnvVar: () => Promise<void>
      deleteEnvVar: (k: string) => Promise<void>
    }

    // Both started before either can reach its continuation.
    vm.newEnvKey = 'ADDED'; vm.newEnvValue = 'a'
    const adding = vm.addEnvVar()
    const deleting = vm.deleteEnvVar('BASE')
    await flushPromises()

    expect(sent.length, 'only one whole-map write may be in flight').toBe(1)

    release[0]({ BASE: '1', ADDED: 'a' })
    await adding
    await flushPromises()
    expect(sent.length).toBe(2)
    release[1]({ ADDED: 'a' })
    await deleting
    await flushPromises()
  })

  // The queue is per panel. A request that never settles on the app being left
  // must not hold up the app arriving.
  it('lets the new panel write while the old panel’s request hangs', async () => {
    // A fresh object per call: the component mutates the map it is given, so a
    // shared fixture would hand the abandoned panel's value to the new one and
    // this test would pass while proving the opposite.
    vi.mocked(appsApi.fetchEnv).mockImplementation(() => Promise.resolve({ BASE: '1' }))
    const sent: string[] = []
    const bodies: Record<string, string>[] = []
    vi.mocked(appsApi.updateEnv).mockImplementation((project: string, _a: string, vars: Record<string, string>) => {
      sent.push(project)
      bodies.push({ ...vars })
      if (project === 'shop-test') return new Promise(() => {})
      return Promise.resolve({ BASE: '1', ON_PROD: 'y' })
    })

    const w = await envPanel({ appName: 'api', namespace: 'shop-test' })
    const vm = w.vm as unknown as {
      newEnvKey: string; newEnvValue: string
      addEnvVar: () => Promise<void>
      envVars: Record<string, string>
    }
    vm.newEnvKey = 'HANGS'; vm.newEnvValue = 'x'
    void vm.addEnvVar()
    await flushPromises()
    expect(sent).toEqual(['shop-test'])

    await w.setProps({ namespace: 'shop-prod' })
    await flushPromises()

    vm.newEnvKey = 'ON_PROD'; vm.newEnvValue = 'y'
    await vm.addEnvVar()
    await flushPromises()

    expect(sent, "the hung request must not block the app now shown").toEqual(['shop-test', 'shop-prod'])
    expect(bodies[1], "and must not carry the abandoned panel's value").toEqual({ BASE: '1', ON_PROD: 'y' })
    expect(vm.envVars).toEqual({ BASE: '1', ON_PROD: 'y' })
  })

  // The optimistic value is on screen before it is stored, and the next queued
  // write builds its body from what is on screen.
  it('does not let the next write store a value the server rejected', async () => {
    // A fresh object per call: the component mutates the map it is given
    // optimistically, so one shared fixture would carry a rejected value into
    // the next read and make this test pass or fail for the wrong reason.
    vi.mocked(appsApi.fetchEnv).mockImplementation(() => Promise.resolve({ BASE: '1' }))
    const sent: Record<string, string>[] = []
    vi.mocked(appsApi.updateEnv).mockImplementation((_p: string, _a: string, vars: Record<string, string>) => {
      sent.push({ ...vars })
      if ('REJECTED' in vars) return Promise.reject(new Error('422'))
      return Promise.resolve({ ...vars })
    })

    const w = await envPanel({ appName: 'api', namespace: 'shop-test' })
    const vm = w.vm as unknown as {
      newEnvKey: string; newEnvValue: string
      addEnvVar: () => Promise<void>
      envVars: Record<string, string>
    }

    vm.newEnvKey = 'REJECTED'; vm.newEnvValue = 'no'
    // The handler reports the failure itself. Rethrowing would only reach a
    // @click listener, where Vue logs it as an unhandled component error.
    await vm.addEnvVar()
    await flushPromises()
    expect(vm.envVars, 'a rejected save must not stay on screen').toEqual({ BASE: '1' })

    vm.newEnvKey = 'GOOD'; vm.newEnvValue = 'yes'
    await vm.addEnvVar()
    await flushPromises()

    expect(sent[1], 'the next write must not carry the rejected value').toEqual({ BASE: '1', GOOD: 'yes' })
  })
})

// A bind books a slot in the same queue the map writers use, so it has to wait
// its turn: sending while a whole-map write is in flight means its follow-up
// read cannot publish, and nothing else refreshes the injected variables it
// just created.
it('makes a bind wait for a whole-map write in flight', async () => {
  // Top level, so no describe resets the mock counts or the teleported DOM.
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'deployer'
  useProjectsStore().projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnv).mockResolvedValue({ BASE: '1' })
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([
    { name: 'cache', namespace: 'shop-prod', type: 'redis', status: 'Running', ready: '1/1', storage: '1Gi' },
  ])

  let releaseWrite: (v: Record<string, string>) => void = () => {}
  vi.mocked(appsApi.updateEnv).mockImplementation(() =>
    new Promise<Record<string, string>>(resolve => { releaseWrite = resolve }))
  const bindCalls: string[] = []
  vi.mocked(servicesApi.bindService).mockImplementation(({ service }) => {
    bindCalls.push(service)
    return Promise.resolve(undefined as never)
  })

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  const vm = w.vm as unknown as {
    newEnvKey: string; newEnvValue: string
    addEnvVar: () => Promise<void>
    bindingService: string
    handleBind: () => Promise<void>
  }

  vm.newEnvKey = 'SLOW'; vm.newEnvValue = 'x'
  const writing = vm.addEnvVar()
  await flushPromises()
  vm.bindingService = 'cache'
  await flushPromises()
  const binding = vm.handleBind()
  await flushPromises()

  expect(bindCalls, 'the bind must not go out while the map write is in flight').toEqual([])

  releaseWrite({ BASE: '1', SLOW: 'x' })
  await writing
  await binding
  await flushPromises()

  expect(bindCalls, 'and must go out once its turn comes').toEqual(['cache'])
})

// The form stays usable while a bind waits its turn, so what is sent has to be
// what was on screen at the click.
it('binds what was selected when the button was pressed', async () => {
  // Top level, so no describe resets the mock counts or the teleported DOM.
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  useProjectsStore().projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnv).mockResolvedValue({ BASE: '1' })
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([
    { name: 'cache', namespace: 'shop-prod', type: 'redis', status: 'Running', ready: '1/1', storage: '1Gi' },
    { name: 'queue', namespace: 'shop-prod', type: 'redis', status: 'Running', ready: '1/1', storage: '1Gi' },
  ])
  let releaseWrite: (v: Record<string, string>) => void = () => {}
  vi.mocked(appsApi.updateEnv).mockImplementation(() =>
    new Promise<Record<string, string>>(resolve => { releaseWrite = resolve }))
  const bound: string[] = []
  vi.mocked(servicesApi.bindService).mockImplementation(({ service }) => {
    bound.push(service)
    return Promise.resolve(undefined as never)
  })

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  const vm = w.vm as unknown as {
    newEnvKey: string; newEnvValue: string
    addEnvVar: () => Promise<void>
    bindingService: string
    handleBind: () => Promise<void>
  }

  // A write in flight, so the bind has to wait.
  vm.newEnvKey = 'SLOW'; vm.newEnvValue = 'x'
  const writing = vm.addEnvVar()
  await flushPromises()

  vm.bindingService = 'cache'
  await flushPromises()
  const binding = vm.handleBind()
  await flushPromises()

  // The operator changes their mind while it says "Binding...".
  vm.bindingService = 'queue'
  await flushPromises()

  releaseWrite({ BASE: '1', SLOW: 'x' })
  await writing
  await binding
  await flushPromises()

  expect(bound, 'the click chose cache, so cache is what is bound').toEqual(['cache'])
})

describe('a mutation that changes the env publishes it before releasing the queue', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  // A public link is a plain env value, so the panel has to adopt the map the
  // link produced before the next queued write builds its body from what is
  // shown — otherwise that write posts a map without it and deletes it.
  it('does not let a queued write delete the variable a link just created', async () => {
    setActivePinia(createPinia())
    useAuthStore().role = 'viewer'
    useProjectsStore().projects = [projectWithRole('deployer')]
    vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
    vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
    vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
    vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
    vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
    vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
    vi.mocked(appsApi.fetchApps).mockResolvedValue([
      { name: 'api', status: 'running', image: 'nginx', replicas: 1, ready: 1 },
      { name: 'worker', status: 'running', image: 'nginx', replicas: 1, ready: 1 },
    ])

    // Before the link there is one variable; after it, the link's URL as well.
    let linked = false
    vi.mocked(appsApi.fetchEnv).mockImplementation(() => {
      const vars: Record<string, string> = linked
        ? { BASE: '1', WORKER_URL: 'http://worker' }
        : { BASE: '1' }
      return Promise.resolve(vars)
    })
    let releaseLink: (v: { target: string; envVar: string }) => void = () => {}
    vi.mocked(appsApi.linkApp).mockImplementation(() =>
      new Promise<{ target: string; envVar: string }>(resolve => {
        releaseLink = v => { linked = true; resolve(v) }
      }) as ReturnType<typeof appsApi.linkApp>)

    const sent: Record<string, string>[] = []
    vi.mocked(appsApi.updateEnv).mockImplementation((_p: string, _a: string, vars: Record<string, string>) => {
      sent.push({ ...vars })
      return Promise.resolve({ ...vars })
    })

    const w = mount(AppDetail, {
      props: { appName: 'api', namespace: 'shop-prod' },
      attachTo: document.body,
      global: { stubs: { RouterLink: true } },
    })
    await flushPromises()
    ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
    await flushPromises()
    const vm = w.vm as unknown as {
      linkingTarget: string
      handleLink: () => Promise<void>
      newEnvKey: string; newEnvValue: string
      addEnvVar: () => Promise<void>
    }

    vm.linkingTarget = 'worker'
    const linking = vm.handleLink()
    await flushPromises()

    // An unrelated edit queues behind the link while it is still running.
    vm.newEnvKey = 'EXTRA'; vm.newEnvValue = 'y'
    const adding = vm.addEnvVar()
    await flushPromises()

    releaseLink({ target: 'worker', envVar: 'WORKER_URL' })
    await linking
    await adding
    await flushPromises()

    expect(sent.length).toBe(1)
    expect(sent[0], "the queued write must carry the link's variable").toEqual({
      BASE: '1', WORKER_URL: 'http://worker', EXTRA: 'y',
    })
  })
})

describe('a failed hand-off does not let the next write post an old map', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  // The refresh after a link is the hand-off the next writer builds on. If it
  // fails, the displayed map no longer matches the server, and posting it back
  // would delete what the link just added.
  it('re-reads before a queued write when the hand-off failed', async () => {
    setActivePinia(createPinia())
    useAuthStore().role = 'viewer'
    useProjectsStore().projects = [projectWithRole('deployer')]
    vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
    vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
    vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
    vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
    vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
    vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
    vi.mocked(appsApi.fetchApps).mockResolvedValue([
      { name: 'api', status: 'running', image: 'nginx', replicas: 1, ready: 1 },
      { name: 'worker', status: 'running', image: 'nginx', replicas: 1, ready: 1 },
    ])

    let reads = 0
    let linked = false
    vi.mocked(appsApi.fetchEnv).mockImplementation(() => {
      reads++
      // The first read fills the panel. The hand-off read after the link fails.
      // The retry before the queued write succeeds.
      if (reads === 2) return Promise.reject(new Error('timeout'))
      const vars: Record<string, string> = linked
        ? { BASE: '1', WORKER_URL: 'http://worker' }
        : { BASE: '1' }
      return Promise.resolve(vars)
    })
    let releaseLink: (v: { target: string; envVar: string }) => void = () => {}
    vi.mocked(appsApi.linkApp).mockImplementation(() =>
      new Promise<{ target: string; envVar: string }>(resolve => {
        releaseLink = v => { linked = true; resolve(v) }
      }) as ReturnType<typeof appsApi.linkApp>)

    const sent: Record<string, string>[] = []
    vi.mocked(appsApi.updateEnv).mockImplementation((_p: string, _a: string, vars: Record<string, string>) => {
      sent.push({ ...vars })
      return Promise.resolve({ ...vars })
    })

    const w = mount(AppDetail, {
      props: { appName: 'api', namespace: 'shop-prod' },
      attachTo: document.body,
      global: { stubs: { RouterLink: true } },
    })
    await flushPromises()
    ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
    await flushPromises()
    const vm = w.vm as unknown as {
      linkingTarget: string
      handleLink: () => Promise<void>
      newEnvKey: string; newEnvValue: string
      addEnvVar: () => Promise<void>
    }

    vm.linkingTarget = 'worker'
    const linking = vm.handleLink()
    await flushPromises()
    vm.newEnvKey = 'EXTRA'; vm.newEnvValue = 'y'
    const adding = vm.addEnvVar()
    await flushPromises()

    releaseLink({ target: 'worker', envVar: 'WORKER_URL' })
    await linking
    await adding
    await flushPromises()

    expect(sent.length).toBe(1)
    expect(sent[0], 'the queued write must re-read rather than post the map it was shown').toEqual({
      BASE: '1', WORKER_URL: 'http://worker', EXTRA: 'y',
    })
  })

  // If the re-read fails too, there is nothing safe to send.
  it('refuses the write when it cannot read the current variables', async () => {
    setActivePinia(createPinia())
    useAuthStore().role = 'viewer'
    useProjectsStore().projects = [projectWithRole('deployer')]
    vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
    vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
    vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
    vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
    vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
    vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
    vi.mocked(appsApi.fetchApps).mockResolvedValue([
      { name: 'api', status: 'running', image: 'nginx', replicas: 1, ready: 1 },
      { name: 'worker', status: 'running', image: 'nginx', replicas: 1, ready: 1 },
    ])

    let reads = 0
    vi.mocked(appsApi.fetchEnv).mockImplementation(() => {
      reads++
      if (reads === 1) return Promise.resolve({ BASE: '1' })
      return Promise.reject(new Error('timeout'))
    })
    vi.mocked(appsApi.linkApp).mockResolvedValue({ target: 'worker', envVar: 'WORKER_URL' } as Awaited<ReturnType<typeof appsApi.linkApp>>)
    const sent: Record<string, string>[] = []
    vi.mocked(appsApi.updateEnv).mockImplementation((_p: string, _a: string, vars: Record<string, string>) => {
      sent.push({ ...vars })
      return Promise.resolve({ ...vars })
    })

    const w = mount(AppDetail, {
      props: { appName: 'api', namespace: 'shop-prod' },
      attachTo: document.body,
      global: { stubs: { RouterLink: true } },
    })
    await flushPromises()
    ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
    await flushPromises()
    const vm = w.vm as unknown as {
      linkingTarget: string
      handleLink: () => Promise<void>
      newEnvKey: string; newEnvValue: string
      addEnvVar: () => Promise<void>
    }

    vm.linkingTarget = 'worker'
    await vm.handleLink()
    await flushPromises()

    vm.newEnvKey = 'EXTRA'; vm.newEnvValue = 'y'
    await vm.addEnvVar()
    await flushPromises()

    expect(sent, 'no map may be posted when none is known to be current').toEqual([])
  })
})

// The panel moving mid-refresh is not the map being current. Reading it as
// "safe to send" posted the app that was left to the app that arrived.
it('sends nothing when the panel moves during the stale-map re-read', async () => {
  // Top level, so no describe resets the mock counts or the teleported DOM.
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  useProjectsStore().projects = [
    { name: 'shop', role: 'deployer', capabilities: capabilitiesForRole('deployer'), env_limit: 3, environments: [
      { name: 'test', namespace: 'shop-test', apps: [], status: 'active', order: '0', owned: true },
      { name: 'prod', namespace: 'shop-prod', apps: [], status: 'active', order: '1', owned: true },
    ] } as Project,
  ]
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([
    { name: 'api', status: 'running', image: 'nginx', replicas: 1, ready: 1 },
    { name: 'worker', status: 'running', image: 'nginx', replicas: 1, ready: 1 },
  ])

  let reads = 0
  let releaseRetry: (v: Record<string, string>) => void = () => {}
  vi.mocked(appsApi.fetchEnv).mockImplementation(() => {
    reads++
    if (reads === 1) return Promise.resolve({ BASE: '1' })
    if (reads === 2) return Promise.reject(new Error('timeout'))  // the hand-off
    if (reads === 3) return new Promise<Record<string, string>>(resolve => { releaseRetry = resolve })
    return Promise.resolve({ ON_PROD: 'y' })
  })
  vi.mocked(appsApi.linkApp).mockResolvedValue({ target: 'worker', envVar: 'WORKER_URL' } as Awaited<ReturnType<typeof appsApi.linkApp>>)
  const sent: string[] = []
  vi.mocked(appsApi.updateEnv).mockImplementation((project: string) => {
    sent.push(project)
    return Promise.resolve({})
  })

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-test' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  const vm = w.vm as unknown as {
    linkingTarget: string
    handleLink: () => Promise<void>
    newEnvKey: string; newEnvValue: string
    addEnvVar: () => Promise<void>
  }

  // A link whose hand-off read fails, leaving the map stale.
  vm.linkingTarget = 'worker'
  await vm.handleLink()
  await flushPromises()

  // An edit then re-reads before sending; the panel moves while it does.
  vm.newEnvKey = 'EXTRA'; vm.newEnvValue = 'y'
  const adding = vm.addEnvVar()
  await flushPromises()
  await w.setProps({ namespace: 'shop-prod' })
  await flushPromises()
  releaseRetry({ BASE: '1', WORKER_URL: 'http://worker' })
  await adding
  await flushPromises()

  expect(sent, 'the edit belonged to the app the panel left').toEqual([])
})

// An empty map is what a failed read has, not what the app has. updateEnv
// replaces the whole map, so an edit made on top of one would delete every
// variable the app already had.
it('does not treat a failed first read as an app with no variables', async () => {
  // Top level, so no describe resets the mock counts or the teleported DOM.
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  useProjectsStore().projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])

  let reads = 0
  vi.mocked(appsApi.fetchEnv).mockImplementation(() => {
    reads++
    if (reads === 1) return Promise.reject(new Error('timeout'))
    return Promise.resolve({ DATABASE_HOST: 'db', LOG_LEVEL: 'info' })
  })
  const sent: Record<string, string>[] = []
  vi.mocked(appsApi.updateEnv).mockImplementation((_p: string, _a: string, vars: Record<string, string>) => {
    sent.push({ ...vars })
    return Promise.resolve({ ...vars })
  })

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  const vm = w.vm as unknown as {
    newEnvKey: string; newEnvValue: string
    addEnvVar: () => Promise<void>
  }

  vm.newEnvKey = 'FEATURE_FLAG'; vm.newEnvValue = 'yes'
  await vm.addEnvVar()
  await flushPromises()

  expect(sent[0], 'the edit must be built on what the app actually has').toEqual({
    DATABASE_HOST: 'db', LOG_LEVEL: 'info', FEATURE_FLAG: 'yes',
  })
})

// A write that has queued behind this one must not stop this one publishing:
// it is the only write in flight, so its answer is the newest there is, and the
// queued write builds its body from what this one publishes.
it('publishes a write even when another has queued behind it', async () => {
  // Top level, so no describe resets the mock counts or the teleported DOM.
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  useProjectsStore().projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnv).mockResolvedValue({ BASE: '1' })

  const sent: Record<string, string>[] = []
  const release: Array<(v: Record<string, string>) => void> = []
  vi.mocked(appsApi.updateEnv).mockImplementation((_p: string, _a: string, vars: Record<string, string>) => {
    sent.push({ ...vars })
    return new Promise<Record<string, string>>(resolve => { release.push(resolve) })
  })

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  const vm = w.vm as unknown as {
    newEnvKey: string; newEnvValue: string
    addEnvVar: () => Promise<void>
    envVars: Record<string, string>
  }

  vm.newEnvKey = 'FIRST'; vm.newEnvValue = 'a'
  const first = vm.addEnvVar()
  await flushPromises()
  vm.newEnvKey = 'SECOND'; vm.newEnvValue = 'b'
  const second = vm.addEnvVar()
  await flushPromises()

  // The server normalises the value it stored; the panel has to adopt that.
  release[0]({ BASE: '1', FIRST: 'A-NORMALISED' })
  await first
  await flushPromises()
  expect(vm.envVars.FIRST, "the first write's answer must not be refused").toBe('A-NORMALISED')
  expect(sent[1], 'and the queued write builds on it').toEqual({ BASE: '1', FIRST: 'A-NORMALISED', SECOND: 'b' })

  release[1]({ BASE: '1', FIRST: 'A-NORMALISED', SECOND: 'b' })
  await second
  await flushPromises()
})

// The panel is reused, so a viewer can arrive at one a deployer left with an
// editor open. A fresh mount never sees that state, which is why the other role
// tests cannot catch it.
it('closes an open editor when the role stops allowing writes', async () => {
  // A top-level test gets no describe's beforeEach, and both the mock counts and
  // the teleported DOM carry over from whatever ran before it.
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  const projects = useProjectsStore()
  projects.projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnv).mockResolvedValue({ LOG_LEVEL: 'debug' })
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  const vm = w.vm as unknown as {
    startEditEnv: (k: string) => void
    editingEnv: string | null
    saveEditEnv: (k: string) => Promise<void>
  }
  vm.startEditEnv('LOG_LEVEL')
  await flushPromises()
  expect(vm.editingEnv).toBe('LOG_LEVEL')
  expect(document.body.innerHTML, 'the editor is open').not.toContain('which your role in this project does not carry')

  // The store refreshes and the caller is now only a viewer here.
  projects.projects = [projectWithRole('viewer')]
  await flushPromises()

  expect((w.vm as unknown as { editingEnv: string | null }).editingEnv,
    'the editor must not outlive the permission that opened it').toBeNull()
  expect(document.body.innerHTML, 'and the tab says why it is read-only now').toContain('which your role in this project does not carry')

  // And the handler refuses on its own account, not just in the template.
  await vm.saveEditEnv('LOG_LEVEL')
  expect(vi.mocked(appsApi.updateEnv)).not.toHaveBeenCalled()
})

// Both apps are writable, so no permission change closes the editor. The draft
// still belongs to the app it was typed for.
it('does not carry an edit in progress to the next app', async () => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  useProjectsStore().projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnv).mockImplementation((_p: string, app: string) => {
    const vars: Record<string, string> = app === 'api' ? { SHARED: 'from-api' } : { SHARED: 'from-worker' }
    return Promise.resolve(vars)
  })
  vi.mocked(appsApi.updateEnv).mockResolvedValue({})

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  const vm = w.vm as unknown as {
    startEditEnv: (k: string) => void
    editEnvValue: string
    editingEnv: string | null
    newEnvKey: string
  }
  vm.startEditEnv('SHARED')
  vm.editEnvValue = 'half-typed'
  vm.newEnvKey = 'ALSO_HALF_TYPED'
  await flushPromises()

  await w.setProps({ appName: 'worker' })
  await flushPromises()

  const after = w.vm as unknown as { editingEnv: string | null; editEnvValue: string; newEnvKey: string }
  expect(after.editingEnv, 'the editor belonged to the app that was left').toBeNull()
  expect(after.editEnvValue).toBe('')
  expect(after.newEnvKey).toBe('')
})

// A write that failed may still have been applied, so the map put back in its
// place is a guess. The next write has to read rather than post the guess.
it('re-reads after a failed write rather than trusting what it put back', async () => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  useProjectsStore().projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])

  let reads = 0
  vi.mocked(appsApi.fetchEnv).mockImplementation(() => {
    reads++
    // The timed-out write did land, so the server has it.
    const vars: Record<string, string> = reads === 1 ? { BASE: '1' } : { BASE: '1', TIMED_OUT: 'a' }
    return Promise.resolve(vars)
  })
  const sent: Record<string, string>[] = []
  vi.mocked(appsApi.updateEnv).mockImplementation((_p: string, _a: string, vars: Record<string, string>) => {
    sent.push({ ...vars })
    if ('TIMED_OUT' in vars && !('LATER' in vars)) return Promise.reject(new Error('timeout'))
    return Promise.resolve({ ...vars })
  })

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  const vm = w.vm as unknown as {
    newEnvKey: string; newEnvValue: string
    addEnvVar: () => Promise<void>
  }

  vm.newEnvKey = 'TIMED_OUT'; vm.newEnvValue = 'a'
  await vm.addEnvVar()
  await flushPromises()

  vm.newEnvKey = 'LATER'; vm.newEnvValue = 'b'
  await vm.addEnvVar()
  await flushPromises()

  expect(sent[1], 'the next write must be built on what the server actually holds').toEqual({
    BASE: '1', TIMED_OUT: 'a', LATER: 'b',
  })
})

// The bind and link forms are drafts too, and both apps here are writable.
it('does not carry a bind or link selection to the next app', async () => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  useProjectsStore().projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnv).mockResolvedValue({})
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([
    { name: 'api', status: 'running', image: 'nginx', replicas: 1, ready: 1 },
    { name: 'worker', status: 'running', image: 'nginx', replicas: 1, ready: 1 },
  ])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([
    { name: 'cache', namespace: 'shop-prod', type: 'redis', status: 'Running', ready: '1/1', storage: '1Gi' },
  ])

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  const vm = w.vm as unknown as {
    bindingService: string; bindingPrefix: string
    linkingTarget: string; linkingPublic: boolean
  }
  vm.bindingService = 'cache'
  vm.bindingPrefix = 'CACHE_'
  vm.linkingTarget = 'worker'
  vm.linkingPublic = true
  await flushPromises()

  await w.setProps({ appName: 'worker' })
  await flushPromises()

  const after = w.vm as unknown as {
    bindingService: string; bindingPrefix: string
    linkingTarget: string; linkingPublic: boolean
  }
  expect(after.bindingService, 'the service was chosen for the app that was left').toBe('')
  expect(after.bindingPrefix).toBe('')
  expect(after.linkingTarget).toBe('')
  expect(after.linkingPublic).toBe(false)
})

// A guard stops a late response publishing; it does not remove what already
// published. If the new app's read fails, the old app's bindings, conflicts and
// restart banner stay on screen and stay clickable, and their handlers act on
// the app now shown.
it('does not leave one app’s Env state on screen for another', async () => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  useProjectsStore().projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue(['DATABASE_URL'])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(true)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([
    { name: 'DATABASE_URL', service: 'db', secret: true },
  ])
  // The app arrived at cannot be read at all, which is the case where nothing
  // replaces what is on screen.
  vi.mocked(appsApi.fetchLinks).mockImplementation((_p: string, app: string) => {
    if (app === 'api') {
      return Promise.resolve([
        { app: 'queue', namespace: 'shop-prod', envVar: 'QUEUE_URL', url: 'http://queue', open: true, injected: true },
      ])
    }
    return Promise.reject(new Error('timeout'))
  })
  vi.mocked(appsApi.fetchEnv).mockImplementation((_p: string, app: string) => {
    if (app === 'api') return Promise.resolve({ DATABASE_URL: 'x' })
    return Promise.reject(new Error('timeout'))
  })

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  const before = w.vm as unknown as {
    injectedVars: unknown[]; envConflicts: string[]; links: unknown[]; envRestartPending: boolean
  }
  expect(before.injectedVars.length).toBe(1)
  expect(before.envConflicts).toEqual(['DATABASE_URL'])
  expect(before.links.length).toBe(1)
  expect(before.envRestartPending).toBe(true)

  await w.setProps({ appName: 'worker' })
  await flushPromises()

  const after = w.vm as unknown as {
    injectedVars: unknown[]; envConflicts: string[]; links: unknown[]
    envRestartPending: boolean; envVars: Record<string, string>
  }
  expect(after.envVars, "the app that was left took its variables with it").toEqual({})
  expect(after.injectedVars, 'and its bindings').toEqual([])
  expect(after.envConflicts, 'and its conflicts').toEqual([])
  expect(after.links, 'and its links').toEqual([])
  expect(after.envRestartPending, 'and its restart banner').toBe(false)
})

// A timed-out delete may well have committed, so the map put back in its place
// is a guess like any other. Only addEnvVar was entering that recovery.
it('re-reads after a failed delete rather than trusting what it put back', async () => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  useProjectsStore().projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])

  let reads = 0
  vi.mocked(appsApi.fetchEnv).mockImplementation(() => {
    reads++
    // The delete did commit, whatever the client saw.
    const vars: Record<string, string> = reads === 1 ? { A: '1', B: '2' } : { A: '1' }
    return Promise.resolve(vars)
  })
  const sent: Record<string, string>[] = []
  vi.mocked(appsApi.updateEnv).mockImplementation((_p: string, _a: string, vars: Record<string, string>) => {
    sent.push({ ...vars })
    if (!('LATER' in vars)) return Promise.reject(new Error('timeout'))
    return Promise.resolve({ ...vars })
  })

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  const vm = w.vm as unknown as {
    deleteEnvVar: (k: string) => Promise<void>
    newEnvKey: string; newEnvValue: string
    addEnvVar: () => Promise<void>
  }

  await vm.deleteEnvVar('B')
  await flushPromises()

  vm.newEnvKey = 'LATER'; vm.newEnvValue = 'c'
  await vm.addEnvVar()
  await flushPromises()

  expect(sent[1], 'B was deleted on the server, so the next write must not put it back').toEqual({
    A: '1', LATER: 'c',
  })
})

// Removing the tab button does not remove what is already on screen. A panel
// held open through a membership change kept every value it had loaded.
it('clears the Env tab when read access goes away', async () => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  const projects = useProjectsStore()
  projects.projects = [projectWithRole('viewer')]
  vi.mocked(appsApi.fetchEnv).mockResolvedValue({ DATABASE_HOST: 'db.internal' })
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  expect(document.body.innerHTML).toContain('db.internal')

  // The membership is removed and the store refreshes.
  projects.projects = []
  await flushPromises()

  const after = w.vm as unknown as { envVars: Record<string, string> }
  expect(after.envVars, 'the values must not stay on screen').toEqual({})
  expect(w.emitted('close'), 'and the panel must close rather than move to another tab it cannot serve').toBeTruthy()
  expect(document.body.innerHTML).not.toContain('db.internal')
})

// A cluster deployer is not exempt: the server gives project-independent access
// to an admin alone, so losing the membership loses the namespace.
it('closes for a cluster deployer whose membership is removed', async () => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'deployer'
  const projects = useProjectsStore()
  projects.projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnv).mockResolvedValue({ DATABASE_HOST: 'db.internal' })
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  expect(document.body.innerHTML).toContain('db.internal')

  projects.projects = []
  await flushPromises()

  expect((w.vm as unknown as { envVars: Record<string, string> }).envVars).toEqual({})
  expect(w.emitted('close'), 'the cluster role is not a claim on this namespace').toBeTruthy()
  expect(document.body.innerHTML).not.toContain('db.internal')
})

// An admin keeps it, because the server resolves them as owner of every project.
it('keeps the panel for a cluster admin with no membership', async () => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'admin'
  const projects = useProjectsStore()
  projects.projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnv).mockResolvedValue({ DATABASE_HOST: 'db.internal' })
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()

  projects.projects = []
  await flushPromises()

  expect(w.emitted('close'), 'an admin is resolved as owner of every project').toBeFalsy()
})

// A refresh whose ownership lookup failed leaves every declaration optimistically
// owned, so one claimant becomes two and the role reads as unknown. That is not
// a membership going away, and throwing the caller out of the panel over it is a
// worse answer than hiding the tab.
it('hides the tab but keeps the panel when ownership is contested', async () => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  const projects = useProjectsStore()
  projects.projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnv).mockResolvedValue({ DATABASE_HOST: 'db.internal' })
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  expect(document.body.innerHTML).toContain('db.internal')

  // A second project now also claims shop-prod, which is what a failed
  // ownership lookup looks like from here.
  projects.projects = [
    projectWithRole('deployer'),
    { name: 'shop-prod', role: 'deployer', capabilities: capabilitiesForRole('deployer'), env_limit: 3, environments: [
      { name: 'default', namespace: 'shop-prod', apps: [], status: 'active', order: '0', owned: true },
    ] } as Project,
  ]
  await flushPromises()

  expect(w.emitted('close'), 'a contested refresh is not a revocation').toBeFalsy()
  expect((w.vm as unknown as { envVars: Record<string, string> }).envVars,
    'the values still go, because the role can no longer be established').toEqual({})
  expect(document.body.innerHTML).not.toContain('db.internal')
})

// Clearing what is on screen achieves nothing if a read issued under the old
// role publishes a moment later.
it('does not republish an Env read that was in flight when access went', async () => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  const projects = useProjectsStore()
  projects.projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])

  let releaseRead: (v: Record<string, string>) => void = () => {}
  let reads = 0
  vi.mocked(appsApi.fetchEnv).mockImplementation(() => {
    reads++
    if (reads === 1) return new Promise<Record<string, string>>(resolve => { releaseRead = resolve })
    return Promise.resolve({})
  })

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()

  // Membership goes while that first read is still out.
  projects.projects = []
  await flushPromises()

  releaseRead({ DATABASE_HOST: 'db.internal' })
  await flushPromises()

  expect((w.vm as unknown as { envVars: Record<string, string> }).envVars,
    'a read from before the revocation must not land after it').toEqual({})
  expect(document.body.innerHTML).not.toContain('db.internal')
})

// The tab a contested refresh emptied fills again once ownership settles.
it('reloads the Env tab when a later refresh restores the role', async () => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  const projects = useProjectsStore()
  projects.projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnv).mockImplementation(() => Promise.resolve({ DATABASE_HOST: 'db.internal' }))

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  expect(document.body.innerHTML).toContain('db.internal')

  // A refresh that cannot settle ownership empties the tab.
  projects.projects = [
    projectWithRole('deployer'),
    { name: 'shop-prod', role: 'deployer', capabilities: capabilitiesForRole('deployer'), env_limit: 3, environments: [
      { name: 'default', namespace: 'shop-prod', apps: [], status: 'active', order: '0', owned: true },
    ] } as Project,
  ]
  await flushPromises()
  expect((w.vm as unknown as { envVars: Record<string, string> }).envVars).toEqual({})

  // The next one settles it.
  projects.projects = [projectWithRole('deployer')]
  await flushPromises()

  expect((w.vm as unknown as { envVars: Record<string, string> }).envVars,
    'the tab must fill again rather than stay blank').toEqual({ DATABASE_HOST: 'db.internal' })
})

// Retiring a write by moving the epoch past it means its done() will not
// decrement the count it incremented. A count stuck above zero stops every
// later read publishing for as long as the panel lives.
it('can still read after a write was retired by losing access', async () => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  const projects = useProjectsStore()
  projects.projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnv).mockImplementation(() => Promise.resolve({ BASE: '1' }))

  let releaseWrite: (v: Record<string, string>) => void = () => {}
  vi.mocked(appsApi.updateEnv).mockImplementation(() =>
    new Promise<Record<string, string>>(resolve => { releaseWrite = resolve }))

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  const vm = w.vm as unknown as {
    newEnvKey: string; newEnvValue: string
    addEnvVar: () => Promise<void>
    loadEnv: () => Promise<void>
    envVars: Record<string, string>
  }

  vm.newEnvKey = 'PENDING'; vm.newEnvValue = 'x'
  const writing = vm.addEnvVar()
  await flushPromises()

  // A contested refresh retires that write along with everything else.
  projects.projects = [
    projectWithRole('deployer'),
    { name: 'shop-prod', role: 'deployer', capabilities: capabilitiesForRole('deployer'), env_limit: 3, environments: [
      { name: 'default', namespace: 'shop-prod', apps: [], status: 'active', order: '0', owned: true },
    ] } as Project,
  ]
  await flushPromises()

  releaseWrite({ BASE: '1', PENDING: 'x' })
  await writing
  await flushPromises()

  // Ownership settles again, and the tab has to be able to read.
  projects.projects = [projectWithRole('deployer')]
  await flushPromises()

  expect((w.vm as unknown as { envVars: Record<string, string> }).envVars,
    'a retired write must not leave the read gate shut').toEqual({ BASE: '1' })
})

// A role that goes and comes back on the same app is not a reason to release the
// next writer: the write it would run alongside is still in flight, and two
// whole-map replacements overlapping is what the queue exists to prevent.
it('keeps the write queue across a momentary loss of the role', async () => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  const projects = useProjectsStore()
  projects.projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnv).mockImplementation(() => Promise.resolve({ BASE: '1' }))

  const inFlight: Array<(v: Record<string, string>) => void> = []
  const sent: Record<string, string>[] = []
  vi.mocked(appsApi.updateEnv).mockImplementation((_p: string, _a: string, vars: Record<string, string>) => {
    sent.push({ ...vars })
    return new Promise<Record<string, string>>(resolve => { inFlight.push(resolve) })
  })

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  const vm = w.vm as unknown as {
    newEnvKey: string; newEnvValue: string
    addEnvVar: () => Promise<void>
  }

  vm.newEnvKey = 'FIRST'; vm.newEnvValue = 'a'
  void vm.addEnvVar()
  await flushPromises()
  expect(sent.length).toBe(1)

  const contested = [
    projectWithRole('deployer'),
    { name: 'shop-prod', role: 'deployer', capabilities: capabilitiesForRole('deployer'), env_limit: 3, environments: [
      { name: 'default', namespace: 'shop-prod', apps: [], status: 'active', order: '0', owned: true },
    ] } as Project,
  ]
  projects.projects = contested
  await flushPromises()
  projects.projects = [projectWithRole('deployer')]
  await flushPromises()

  // A second edit while the first PUT is still out must still wait for it.
  vm.newEnvKey = 'SECOND'; vm.newEnvValue = 'b'
  void vm.addEnvVar()
  await flushPromises()

  expect(sent.length, 'the first write is still in flight, so the second waits').toBe(1)

  inFlight[0]({ BASE: '1', FIRST: 'a' })
  await flushPromises()
  expect(sent.length).toBe(2)
})

// A mutation publishes what it reads back, so losing the role has to stop it
// too. Otherwise the tab is emptied and a write settling a moment later refills
// it with the app's variables for someone who may no longer read them.
it('does not let a write refill the tab after access is lost', async () => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  const projects = useProjectsStore()
  projects.projects = [projectWithRole('deployer')]
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnv).mockImplementation(() => Promise.resolve({ BASE: '1' }))

  let releaseWrite: (v: Record<string, string>) => void = () => {}
  vi.mocked(appsApi.updateEnv).mockImplementation(() =>
    new Promise<Record<string, string>>(resolve => { releaseWrite = resolve }))

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  const vm = w.vm as unknown as {
    newEnvKey: string; newEnvValue: string
    addEnvVar: () => Promise<void>
  }
  vm.newEnvKey = 'ADDED'; vm.newEnvValue = 'x'
  const writing = vm.addEnvVar()
  await flushPromises()

  // The membership goes while that PUT is out.
  projects.projects = []
  await flushPromises()

  releaseWrite({ BASE: '1', ADDED: 'x' })
  await writing
  await flushPromises()

  expect((w.vm as unknown as { envVars: Record<string, string> }).envVars,
    'a write settling after the role went must not refill the tab').toEqual({})
})

// A write retired by a role that went cannot publish and cannot run its own
// hand-off, so without this the tab stays empty until something else reloads it
// — and a viewer has no write to trigger that with.
it('reads again once a retired write releases the last slot', async () => {
  vi.clearAllMocks()
  document.body.innerHTML = ''
  setActivePinia(createPinia())
  useAuthStore().role = 'viewer'
  const projects = useProjectsStore()
  const owned = projectWithRole('deployer')
  projects.projects = [owned]
  vi.mocked(appsApi.fetchEnvPreview).mockRejectedValue(new Error('403'))
  vi.mocked(appsApi.fetchEnvConflicts).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnvRestartPending).mockResolvedValue(false)
  vi.mocked(appsApi.fetchInjectedEnv).mockResolvedValue([])
  vi.mocked(appsApi.fetchLinks).mockResolvedValue([])
  vi.mocked(appsApi.fetchApps).mockResolvedValue([])
  vi.mocked(servicesApi.fetchServices).mockResolvedValue([])
  vi.mocked(appsApi.fetchEnv).mockImplementation(() => Promise.resolve({ BASE: '1' }))

  let releaseWrite: (v: Record<string, string>) => void = () => {}
  vi.mocked(appsApi.updateEnv).mockImplementation(() =>
    new Promise<Record<string, string>>(resolve => { releaseWrite = resolve }))

  const w = mount(AppDetail, {
    props: { appName: 'api', namespace: 'shop-prod' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
  ;(w.vm as unknown as { activeTab: string }).activeTab = 'env'
  await flushPromises()
  const vm = w.vm as unknown as {
    newEnvKey: string; newEnvValue: string
    addEnvVar: () => Promise<void>
  }
  vm.newEnvKey = 'PENDING'; vm.newEnvValue = 'x'
  const writing = vm.addEnvVar()
  await flushPromises()

  // Ownership goes contested and comes back while that PUT is still out.
  projects.projects = [
    owned,
    { name: 'shop-prod', role: 'deployer', capabilities: capabilitiesForRole('deployer'), env_limit: 3, environments: [
      { name: 'default', namespace: 'shop-prod', apps: [], status: 'active', order: '0', owned: true },
    ] } as Project,
  ]
  await flushPromises()
  projects.projects = [owned]
  await flushPromises()
  expect((w.vm as unknown as { envVars: Record<string, string> }).envVars,
    'the recovery read cannot publish while that write is pending').toEqual({})

  releaseWrite({ BASE: '1', PENDING: 'x' })
  await writing
  await flushPromises()

  expect((w.vm as unknown as { envVars: Record<string, string> }).envVars,
    'so releasing the slot has to read again').toEqual({ BASE: '1' })
})

// Every built-in role holds env.write, kipper.write and workloads.restart
// together or holds none of them, so a fixture built from a role cannot tell
// whether a control asks for the capability its own route takes. A cluster is
// free to grant a role that splits them, and these two name the capabilities
// outright to cover that.
describe('the Env tab and a capability set no built-in role produces', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  it('offers env editing but not binding or restarting to a set holding only env.write', async () => {
    const w = await mountEnvTab('deployer', 'viewer', ['kipper.read', 'env.read', 'env.write', 'workloads.read'])
    await openEnvTab(w)
    const html = rendered()

    for (const marker of ENV_WRITE_MARKERS) {
      expect(html, `env.write carries: ${marker}`).toContain(marker)
    }
    for (const marker of [...KIPPER_WRITE_MARKERS, ...RESTART_MARKERS]) {
      expect(html, `env.write does not carry: ${marker}`).not.toContain(marker)
    }
  })

  it('offers binding but not env editing to a set holding only kipper.write', async () => {
    const w = await mountEnvTab('deployer', 'viewer', ['kipper.read', 'kipper.write', 'env.read', 'workloads.read'])
    await openEnvTab(w)
    const html = rendered()

    for (const marker of KIPPER_WRITE_MARKERS) {
      expect(html, `kipper.write carries: ${marker}`).toContain(marker)
    }
    for (const marker of [...ENV_WRITE_MARKERS, ...RESTART_MARKERS]) {
      expect(html, `kipper.write does not carry: ${marker}`).not.toContain(marker)
    }
  })
})

// Eight of the nine tabs used to be gated on the cluster role. That denied a
// project member the tabs the API would have served them, and showed a cluster
// deployer with no role in this project tabs whose every read comes back 403.
describe('which tabs the panel offers', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  function tabsOf(w: Awaited<ReturnType<typeof mountEnvTab>>): string[] {
    return (w.vm as unknown as { visibleTabs: { key: string }[] }).visibleTabs.map(t => t.key)
  }

  it('offers a project owner every tab even though their cluster role is viewer', async () => {
    const w = await mountEnvTab('owner', 'viewer')
    expect(tabsOf(w)).toEqual(
      expect.arrayContaining(['logs', 'deploys', 'scale', 'resources', 'env', 'files', 'connect', 'secrets', 'settings']),
    )
  })

  it('offers a cluster deployer with no role in this project nothing', async () => {
    setActivePinia(createPinia())
    useAuthStore().role = 'deployer'
    // No project claims this namespace, so the caller holds nothing in it.
    useProjectsStore().projects = []
    const w = mount(AppDetail, {
      props: { appName: 'api', namespace: 'someone-elses-ns' },
      attachTo: document.body,
      global: { stubs: { RouterLink: true } },
    })
    await flushPromises()
    expect(tabsOf(w)).toEqual([])
  })

  it('withholds the env and secrets tabs from a set that cannot read env', async () => {
    const w = await mountEnvTab('deployer', 'viewer', ['kipper.read', 'workloads.read', 'pods.logs.read'])
    const tabs = tabsOf(w)
    expect(tabs).toContain('logs')
    expect(tabs).not.toContain('env')
    expect(tabs).not.toContain('secrets')
  })

  it('withholds the files tab from a set that cannot read files', async () => {
    const w = await mountEnvTab('deployer', 'viewer', ['kipper.read', 'env.read'])
    expect(tabsOf(w)).not.toContain('files')
  })

  it('withholds the connect tab from a set that cannot open a terminal', async () => {
    const w = await mountEnvTab('deployer', 'viewer', ['kipper.read', 'env.read', 'files.read'])
    expect(tabsOf(w)).not.toContain('connect')
  })
})

// The Secrets and Files tabs carried no gate at all: every control rendered for
// anyone who reached the tab, and the server refused each one.
describe('the Secrets and Files tabs', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
    vi.mocked(appsApi.fetchSecretKeys).mockResolvedValue([{ key: 'DATABASE_URL', has_previous: false }])
  })

  async function openTab(w: Awaited<ReturnType<typeof mountEnvTab>>, tab: string) {
    ;(w.vm as unknown as { activeTab: string }).activeTab = tab
    await flushPromises()
    await flushPromises()
  }

  const SECRET_WRITE_MARKERS = ['title="Edit"', 'title="Delete"', 'placeholder="DATABASE_URL"']

  it('offers a set that can read but not write secrets no control that writes one', async () => {
    const w = await mountEnvTab('deployer', 'viewer', ['kipper.read', 'env.read'])
    await openTab(w, 'secrets')
    const html = rendered()
    for (const marker of SECRET_WRITE_MARKERS) {
      expect(html, `env.read alone must not be offered: ${marker}`).not.toContain(marker)
    }
    expect(html, 'and cannot reveal one either').not.toContain('title="Reveal"')
  })

  it('offers those controls to a set that can write them', async () => {
    const w = await mountEnvTab('deployer', 'viewer', ['kipper.read', 'env.read', 'env.write', 'env.reveal'])
    await openTab(w, 'secrets')
    const html = rendered()
    for (const marker of SECRET_WRITE_MARKERS) {
      expect(html, `env.write carries: ${marker}`).toContain(marker)
    }
    expect(html, 'and env.reveal carries the reveal').toContain('title="Reveal"')
  })

  it('withholds upload from a set that can read files but not write them', async () => {
    const w = await mountEnvTab('deployer', 'viewer', ['kipper.read', 'files.read'])
    await openTab(w, 'files')
    expect(rendered(), 'files.read alone must not be offered an upload').not.toContain('Upload')
  })

  it('offers upload to a set that can write files', async () => {
    const w = await mountEnvTab('deployer', 'viewer', ['kipper.read', 'files.read', 'files.write'])
    await openTab(w, 'files')
    expect(rendered(), 'files.write carries the upload').toContain('Upload')
  })
})

// Opening the deploys, scale, resources and settings tabs on a read capability
// made write controls reachable that the cluster-role gate used to hide with the
// whole tab. A project viewer holds every read these tabs take and none of the
// writes inside them.
describe('the write controls inside the read-gated tabs', () => {
  // Each of these tabs needs its own data before any control in it renders, or
  // the control is absent for a deployer too and the viewer assertion proves
  // nothing about the gate.
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
    vi.mocked(appsApi.fetchResources).mockResolvedValue({
      memory_limit: '512Mi', memory_request: '256Mi', cpu_limit: '500m', cpu_request: '100m',
    })
    vi.mocked(appsApi.fetchAutoscale).mockResolvedValue({
      enabled: true, min_replicas: 1, max_replicas: 4, cpu_target: 70, memory_target: 80,
      current_replicas: 2, current_cpu: '20m', current_memory: '100Mi',
    })
    vi.mocked(appsApi.fetchRoute).mockResolvedValue({
      host: 'api.example.com', path: '/', redirect_from: [], url: 'https://api.example.com',
      enabled: true, health: { ingress_ready: true, tls_ready: true, message: '' },
    })
  })

  // The resources tab is not covered here: its save sits behind an advanced
  // toggle this test could not open, so an assertion on it would pass whether
  // or not the gate existed. It reads the same canWriteApp these two prove.
  const WRITES_BY_TAB: Record<string, string[]> = {
    settings: ['Save settings', 'Save route'],
    scale: ['Save autoscaling', 'Optimise'],
  }

  async function openTab(w: Awaited<ReturnType<typeof mountEnvTab>>, tab: string) {
    ;(w.vm as unknown as { activeTab: string }).activeTab = tab
    await flushPromises()
    await flushPromises()
  }

  for (const [tab, markers] of Object.entries(WRITES_BY_TAB)) {
    it(`offers a project viewer no write control on the ${tab} tab`, async () => {
      const w = await mountEnvTab('viewer', 'viewer')
      await openTab(w, tab)
      const html = rendered()
      for (const marker of markers) {
        expect(html, `a viewer must not be offered on ${tab}: ${marker}`).not.toContain(marker)
      }
    })

    it(`offers a project deployer those controls on the ${tab} tab`, async () => {
      const w = await mountEnvTab('deployer', 'viewer')
      await openTab(w, tab)
      const html = rendered()
      for (const marker of markers) {
        expect(html, `a deployer holds kipper.write, so ${tab} carries: ${marker}`).toContain(marker)
      }
    })
  }
})
