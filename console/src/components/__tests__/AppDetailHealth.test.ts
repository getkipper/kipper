// @vitest-environment happy-dom
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import AppDetail from '../AppDetail.vue'
import * as appsApi from '@/api/apps'
import * as projectsApi from '@/api/projects'
import { useAuthStore } from '@/stores/auth'
import { useProjectsStore } from '@/stores/projects'
import type { ContainerHealth, PodHealth } from '@/api/apps'
import type { Project } from '@/api/projects'

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

beforeEach(() => {
  vi.mocked(appsApi.fetchLogs).mockResolvedValue([])
})

function pod(containers: ContainerHealth[], phase = 'Running'): PodHealth {
  return { name: 'checkout-5f7d', phase, init_containers: [], containers }
}

function running(name: string): ContainerHealth {
  return { name, ready: true, restarts: 0, state: 'running' }
}

async function mountWithHealth(health: PodHealth[] | Error) {
  setActivePinia(createPinia())
  useAuthStore().role = 'admin'
  useProjectsStore().projects = [{
    name: 'shop', role: 'owner', env_limit: 3,
    environments: [{ name: 'test', namespace: 'shop-test', apps: [], status: 'active', order: '0', owned: true }],
  } as Project]

  if (health instanceof Error) {
    vi.mocked(appsApi.fetchAppHealth).mockRejectedValue(health)
  } else {
    vi.mocked(appsApi.fetchAppHealth).mockResolvedValue(health)
  }

  mount(AppDetail, {
    props: { appName: 'checkout', namespace: 'shop-test' },
    attachTo: document.body,
    global: { stubs: { RouterLink: true } },
  })
  await flushPromises()
}

// The panel teleports to body, so the wrapper's own html is only a
// placeholder. Reading that instead of the document is how the first version
// of this test reported the banner missing when it was rendering fine.
function rendered(): string {
  return document.body.innerHTML
}

function renderedText(): string {
  return document.body.textContent || ''
}

describe('AppDetail health banner', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(appsApi.fetchLogs).mockResolvedValue([])
    // A teleported panel is not removed by the next mount, so without this a
    // test reads the previous test's DOM.
    document.body.innerHTML = ''
  })

  // The case from the live cluster: the pod phase reads Running because the
  // sidecar is up, while the app's own container is dead and looping. The
  // console showed such an app as healthy.
  it('reports a dead container in a pod whose phase says Running', async () => {
    await mountWithHealth([pod([
      {
        name: 'checkout', ready: false, restarts: 4, state: 'terminated', reason: 'Error', exit_code: 1,
        last_termination: { reason: 'Error', exit_code: 1, message: 'migration failed: relation already exists' },
      },
      running('kipper-instance-proxy'),
    ])])

    expect(rendered()).toContain('app-health-banner')
    expect(renderedText()).toContain('checkout')
    expect(renderedText()).toContain('restarted 4')
  })

  // A container between restarts carries only CrashLoopBackOff in its current
  // state. The exit code and the process's own message are in the previous
  // termination, and that is the part that names the cause.
  it('shows the previous termination detail, not just the backoff reason', async () => {
    await mountWithHealth([pod([{
      name: 'checkout', ready: false, restarts: 3, state: 'waiting', reason: 'CrashLoopBackOff',
      message: 'back-off 40s restarting failed container',
      last_termination: { reason: 'Error', exit_code: 1, message: 'migration failed: relation already exists' },
    }])])

    expect(renderedText()).toContain('CrashLoopBackOff')
    expect(renderedText()).toContain('exit 1')
    expect(renderedText()).toContain('migration failed: relation already exists')
  })

  // "Error, exit 1" names no cause on its own. The log is where the reason
  // actually is, and reading it needed kubectl during the live incident.
  it('shows the log of the container that died', async () => {
    await mountWithHealth([pod([{
      name: 'checkout', ready: false, restarts: 3, state: 'waiting', reason: 'CrashLoopBackOff',
      last_termination: { reason: 'Error', exit_code: 1 },
      log: 'Migration failed for changeset 016-mailbox-ownership-lookups.sql\n  Reason: relation "idx_email_entitlements_user_id" already exists',
    }])])

    expect(rendered()).toContain('app-health-log')
    expect(renderedText()).toContain('already exists')
  })

  // A build's clone step finishes with exit 0 and stays "terminated" forever.
  // Calling that a failure would put a red banner on every successful build.
  it('does not call a completed init container a failure', async () => {
    await mountWithHealth([{
      name: 'checkout-build', phase: 'Running',
      init_containers: [{
        name: 'clone', ready: false, restarts: 0, state: 'terminated', reason: 'Completed', exit_code: 0,
      }],
      containers: [running('checkout')],
    }])

    expect(rendered()).not.toContain('app-health-banner')
  })

  // The other half: a crash loop sampled in the moment its container is up but
  // not ready. Treating that as healthy hides exactly what this panel is for.
  it('reports a container that is running but has already been restarting', async () => {
    await mountWithHealth([pod([{
      name: 'checkout', ready: false, restarts: 4, state: 'running',
      last_termination: { reason: 'Error', exit_code: 1, message: 'migration failed' },
    }])])

    expect(rendered()).toContain('app-health-banner')
    expect(renderedText()).toContain('restarted 4')
  })

  it('stays out of the way when every container is running', async () => {
    await mountWithHealth([pod([running('checkout'), running('kipper-instance-proxy')])])

    expect(rendered()).not.toContain('app-health-banner')
  })

  // Rule 4 of the plan: a failure to read status must not render as "healthy".
  // Swallowing it into an empty list is what made the route editor's empty
  // dropdown indistinguishable from a permission error.
  it('says so when container status cannot be read at all', async () => {
    await mountWithHealth(new Error('403 forbidden'))

    expect(rendered()).not.toContain('app-health-banner')
    expect(renderedText()).toContain('Container status could not be read')
  })

  // A build's clone step fails in an init container, so a panel that only reads
  // the main containers is blank on exactly the pods that failed to start.
  it('reports a failed init container', async () => {
    await mountWithHealth([{
      name: 'checkout-build', phase: 'Pending',
      init_containers: [{
        name: 'clone', ready: false, restarts: 0, state: 'terminated', reason: 'Error', exit_code: 128,
        message: "could not read Username for 'https://git.example.com'",
      }],
      containers: [],
    }])

    expect(renderedText()).toContain('clone')
    expect(renderedText()).toContain("could not read Username for 'https://git.example.com'")
  })

  // The health endpoint's result was assigned straight through, so a body that
  // is not a list — a 204, a null, an endpoint the caller has stubbed out —
  // left the pod list holding something the banner then iterated. The component
  // update threw on every render rather than showing nothing.
  it('shows no banner when the health endpoint returns no list at all', async () => {
    setActivePinia(createPinia())
    useAuthStore().role = 'admin'
    useProjectsStore().projects = [{
      name: 'shop', role: 'owner', env_limit: 3,
      environments: [{ name: 'test', namespace: 'shop-test', apps: [], status: 'active', order: '0', owned: true }],
    } as Project]

    vi.mocked(appsApi.fetchAppHealth).mockResolvedValue(
      undefined as unknown as Awaited<ReturnType<typeof appsApi.fetchAppHealth>>,
    )

    mount(AppDetail, {
      props: { appName: 'checkout', namespace: 'shop-test' },
      attachTo: document.body,
      global: { stubs: { RouterLink: true } },
    })
    await flushPromises()

    expect(rendered()).not.toContain('app-health-banner')
  })
})

// The gap the operator reported: the CI webhook card has a remove button and
// the Git source card did not, so a source added by mistake could not be
// taken off in the console at all.
describe('the Git source card', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(appsApi.fetchLogs).mockResolvedValue([])
    document.body.innerHTML = ''
  })

  async function mountWithGitSource() {
    setActivePinia(createPinia())
    useAuthStore().role = 'admin'
    useProjectsStore().projects = [{
      name: 'shop', role: 'owner', env_limit: 3,
      environments: [{ name: 'test', namespace: 'shop-test', apps: [], status: 'active', order: '0', owned: true }],
    } as Project]
    vi.mocked(appsApi.fetchAppHealth).mockResolvedValue([])
    vi.mocked(appsApi.fetchDeployHistory).mockResolvedValue([])
    vi.mocked(appsApi.fetchWebhookConfig).mockResolvedValue({ enabled: false })
    vi.mocked(appsApi.fetchBuildStatus).mockResolvedValue({
      git_configured: true,
      git_url: 'https://git.example.com/shop/checkout.git',
      git_branch: 'main',
      phase: 'Failed',
    })

    const wrapper = mount(AppDetail, {
      props: { appName: 'checkout', namespace: 'shop-test' },
      attachTo: document.body,
      global: { stubs: { RouterLink: true } },
    })
    await flushPromises()
    const vm = wrapper.vm as unknown as { activeTab: string; deployMethodsOpen: boolean }
    vm.activeTab = 'deploys'
    await flushPromises()
    await flushPromises()
    // Deploy methods is collapsed by default, and the Git card lives inside it.
    vm.deployMethodsOpen = true
    await flushPromises()
    return wrapper
  }

  it('offers a way to stop building from git', async () => {
    await mountWithGitSource()

    expect(document.body.innerHTML).toContain('remove-git-source')
  })
})
