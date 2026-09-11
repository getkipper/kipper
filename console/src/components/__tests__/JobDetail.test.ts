// @vitest-environment happy-dom
import { capabilitiesForRole } from '@/utils/testCapabilities'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import JobDetail from '../JobDetail.vue'
import * as jobsApi from '@/api/jobs'
import { useAuthStore } from '@/stores/auth'
import { useProjectsStore } from '@/stores/projects'
import type { Project, ProjectRole } from '@/api/projects'

const toastSpies = { success: vi.fn(), error: vi.fn(), info: vi.fn() }
vi.mock('@/composables/useToast', () => ({ useToast: () => toastSpies }))

// The panel opens a log socket on mount. happy-dom has no server to talk to and
// the subject here is which job each request names, so the stream is a stub.
const logStream = {
  lines: { value: [] as string[] },
  connected: { value: false },
  connect: vi.fn(),
  disconnect: vi.fn(),
  clear: vi.fn(),
}
vi.mock('@/composables/useLogStream', () => ({ useLogStream: () => logStream }))

vi.mock('@/api/jobs')

// Two projects, each with a job called nightly-cleanup. Telling them apart is
// the whole point: a name alone reaches both.
const SHOP_NS = 'shop-prod'
const BLOG_NS = 'blog-prod'
const JOB = 'nightly-cleanup'

function project(name: string, namespace: string, role: ProjectRole): Project {
  return {
    name, role, capabilities: capabilitiesForRole(role), env_limit: 3,
    environments: [{ name: 'prod', namespace, apps: [], status: 'active', order: '0', owned: true }],
  }
}

async function mountDetail(namespace = SHOP_NS, role: ProjectRole = 'deployer', jobType = 'cronjob') {
  setActivePinia(createPinia())
  useAuthStore().role = 'member'
  useProjectsStore().projects = [project('shop', SHOP_NS, role), project('blog', BLOG_NS, role)]

  vi.mocked(jobsApi.fetchJobHistory).mockResolvedValue([])
  vi.mocked(jobsApi.fetchJobResources).mockResolvedValue({
    memory_limit: '', memory_request: '', cpu_limit: '', cpu_request: '',
  })
  vi.mocked(jobsApi.triggerJob).mockResolvedValue(undefined)
  vi.mocked(jobsApi.updateJobResources).mockResolvedValue(undefined)

  const wrapper = mount(JobDetail, {
    props: { jobName: JOB, jobType, schedule: jobType === 'cronjob' ? '0 3 * * *' : '', namespace },
    attachTo: document.body,
    global: { stubs: { RouterLink: true, SidePanel: false } },
  })
  await flushPromises()
  return wrapper
}

beforeEach(() => {
  vi.clearAllMocks()
  // SidePanel teleports into the body and the assertions below read it, so a
  // panel left behind by the previous case would answer for this one.
  document.body.innerHTML = ''
})

describe('JobDetail addresses one job', () => {
  it('names the namespace when it reads history', async () => {
    await mountDetail()
    expect(jobsApi.fetchJobHistory).toHaveBeenCalledWith(SHOP_NS, JOB)
  })

  it('reloads when the namespace changes and the name does not', async () => {
    // Two jobs share a name across projects, so switching rows changes only the
    // namespace. A panel that watches the name alone keeps the first job's
    // history, resources and logs under the second one's heading, and Save then
    // writes the stale values to the namespace now named.
    const wrapper = await mountDetail(SHOP_NS)
    vi.mocked(jobsApi.fetchJobHistory).mockClear()

    await wrapper.setProps({ namespace: BLOG_NS })
    await flushPromises()

    expect(jobsApi.fetchJobHistory).toHaveBeenCalledWith(BLOG_NS, JOB)
  })

  it('reloads the resources tab when the namespace changes', async () => {
    // The panel the red team described: open Resources on one job, select the
    // same-named job in another project, and Save writes the first job's limits
    // to the second job's namespace, which the server authorizes correctly.
    const wrapper = await mountDetail(SHOP_NS)
    ;(wrapper.vm as unknown as { activeTab: string }).activeTab = 'resources'
    await flushPromises()
    vi.mocked(jobsApi.fetchJobResources).mockClear()

    await wrapper.setProps({ namespace: BLOG_NS })
    await flushPromises()

    expect(jobsApi.fetchJobResources).toHaveBeenCalledWith(BLOG_NS, JOB)
  })

  it('names the namespace when it triggers', async () => {
    const wrapper = await mountDetail()
    await (wrapper.vm as unknown as { handleTrigger: () => Promise<void> }).handleTrigger()
    expect(jobsApi.triggerJob).toHaveBeenCalledWith(SHOP_NS, JOB)
  })

  it('names the namespace when it saves resources', async () => {
    const wrapper = await mountDetail()
    await (wrapper.vm as unknown as { saveJobResources: () => Promise<void> }).saveJobResources()
    expect(jobsApi.updateJobResources).toHaveBeenCalledWith(SHOP_NS, JOB, expect.anything())
  })

  it('gives log analysis the namespace the job runs in, not default', async () => {
    const wrapper = await mountDetail(BLOG_NS)
    const analysis = wrapper.findComponent({ name: 'LogAnalysis' })
    expect(analysis.exists()).toBe(true)
    expect(analysis.props('namespace')).toBe(BLOG_NS)
  })
})

describe('JobDetail does not turn a failed read into an empty one', () => {
  it('refuses to save limits it never managed to read', async () => {
    // The fields blank themselves when a read fails. Saving them writes that
    // blank over whatever was pinned, and the reconciler then falls back to its
    // own defaults, so a failed read silently resets the job.
    const wrapper = await mountDetail(SHOP_NS)
    vi.mocked(jobsApi.fetchJobResources).mockRejectedValue({ response: { data: { error: 'etcd is unreachable' } } })

    const vm = wrapper.vm as unknown as {
      activeTab: string
      saveJobResources: () => Promise<void>
    }
    vm.activeTab = 'resources'
    await flushPromises()

    await vm.saveJobResources()

    expect(jobsApi.updateJobResources).not.toHaveBeenCalled()
    expect(document.body.textContent).toContain('etcd is unreachable')
  })

  it('says the history could not be read rather than showing no runs', async () => {
    const wrapper = await mountDetail(SHOP_NS)
    vi.mocked(jobsApi.fetchJobHistory).mockRejectedValue({ response: { data: { error: 'etcd is unreachable' } } })

    const vm = wrapper.vm as unknown as { activeTab: string }
    vm.activeTab = 'history'
    await flushPromises()

    expect(document.body.textContent).toContain('etcd is unreachable')
  })
})

describe('JobDetail offers only what the server will accept', () => {
  it('does not offer a resources form for a job that runs once', async () => {
    // A one-off job's run is created from the CR and never patched, so the
    // server refuses the write. Rendering the form invites a 409 every time.
    const wrapper = await mountDetail(SHOP_NS, 'deployer', 'job')
    const vm = wrapper.vm as unknown as { activeTab: string }
    vm.activeTab = 'resources'
    await flushPromises()

    expect(document.body.textContent).toContain('runs once')
    expect(document.body.innerHTML).not.toContain('e.g. 256Mi, 1Gi')
  })

  it('names the job it triggered even when the row changed meanwhile', async () => {
    const wrapper = await mountDetail(SHOP_NS)
    let release: () => void = () => {}
    vi.mocked(jobsApi.triggerJob).mockReturnValue(new Promise<void>(resolve => { release = resolve }))

    const vm = wrapper.vm as unknown as { handleTrigger: () => Promise<void> }
    const triggered = vm.handleTrigger()
    await wrapper.setProps({ jobName: 'backup', namespace: BLOG_NS })
    release()
    await triggered
    await flushPromises()

    expect(toastSpies.success).toHaveBeenCalledWith(`${JOB} triggered, running now`)
  })
})

describe('JobDetail tells a missing route from a missing job', () => {
  it('says the API is behind when a 404 carries no reason', async () => {
    // Every 404 this API raises for itself goes through respondError and carries
    // {"error": ...}. A route that does not exist reaches no handler, so chi
    // answers a plain-text 404 with no error field. That is the whole signal:
    // an old console-api serves none of the namespaced job routes.
    const wrapper = await mountDetail(SHOP_NS)
    vi.mocked(jobsApi.fetchJobHistory).mockRejectedValue({ response: { status: 404, data: '404 page not found' } })

    const vm = wrapper.vm as unknown as { activeTab: string }
    vm.activeTab = 'history'
    await flushPromises()

    expect(document.body.textContent).toContain('kip upgrade')
  })

  it('repeats the server reason when a 404 carries one', async () => {
    const wrapper = await mountDetail(SHOP_NS)
    vi.mocked(jobsApi.fetchJobHistory).mockRejectedValue({
      response: { status: 404, data: { error: 'job "nightly-cleanup" not found' } },
    })

    const vm = wrapper.vm as unknown as { activeTab: string }
    vm.activeTab = 'history'
    await flushPromises()

    expect(document.body.textContent).toContain('job "nightly-cleanup" not found')
    expect(document.body.textContent).not.toContain('kip upgrade')
  })
})

describe('JobDetail ignores a response for the job it is no longer showing', () => {
  it('keeps the selected job\'s limits when the previous request lands late', async () => {
    // Open Resources on one job, select the same-named job in another project
    // before the first response arrives, then let it arrive. Writing it would
    // put shop-prod's numbers under blog-prod's heading, and Save would send
    // them to blog-prod, which the server authorizes correctly.
    const wrapper = await mountDetail(SHOP_NS)

    // mountDetail seeds the loaders, so the ordering is set up after it: the
    // first read hangs, every later one answers for the job now selected.
    let releaseShop: (v: jobsApi.JobResources) => void = () => {}
    const shopPending = new Promise<jobsApi.JobResources>(resolve => { releaseShop = resolve })
    vi.mocked(jobsApi.fetchJobResources)
      .mockReturnValueOnce(shopPending)
      .mockResolvedValue({ memory_limit: '256Mi', memory_request: '', cpu_limit: '250m', cpu_request: '' })

    ;(wrapper.vm as unknown as { activeTab: string }).activeTab = 'resources'
    await flushPromises()

    await wrapper.setProps({ namespace: BLOG_NS })
    await flushPromises()

    releaseShop({ memory_limit: '512Mi', memory_request: '', cpu_limit: '500m', cpu_request: '' })
    await flushPromises()

    const vm = wrapper.vm as unknown as {
      jobMemoryLimit: string
      saveJobResources: () => Promise<void>
    }
    expect(vm.jobMemoryLimit).toBe('256Mi')

    await vm.saveJobResources()
    expect(jobsApi.updateJobResources).toHaveBeenCalledWith(BLOG_NS, JOB, {
      memory_limit: '256Mi',
      cpu_limit: '250m',
    })
  })
})

describe('JobDetail says why a request was refused', () => {
  it('repeats the server reason when a trigger is refused', async () => {
    // The server answers 409 naming both namespaces when a bare name reaches
    // more than one job. Swallowing the body leaves the operator with "failed"
    // and no way to tell a collision from an outage.
    const wrapper = await mountDetail()
    const reason = `job "${JOB}" exists in ${BLOG_NS}, ${SHOP_NS}; name one of them`
    vi.mocked(jobsApi.triggerJob).mockRejectedValue({ response: { data: { error: reason } } })

    await (wrapper.vm as unknown as { handleTrigger: () => Promise<void> }).handleTrigger()

    expect(toastSpies.error).toHaveBeenCalledWith(reason)
  })

  it('repeats the server reason when a resource save is refused', async () => {
    // A job save has no quota preflight behind it, so the reasons it can meet
    // are the ambiguous name and a value the server will not parse.
    const wrapper = await mountDetail()
    const reason = 'memory_limit: quantities must match the regular expression'
    vi.mocked(jobsApi.updateJobResources).mockRejectedValue({ response: { data: { error: reason } } })

    await (wrapper.vm as unknown as { saveJobResources: () => Promise<void> }).saveJobResources()

    expect(toastSpies.error).toHaveBeenCalledWith(reason)
  })
})
