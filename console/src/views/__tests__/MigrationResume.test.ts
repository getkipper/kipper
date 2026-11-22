// @vitest-environment happy-dom
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import Migration from '../Migration.vue'
import * as migrationApi from '@/api/migration'
import { useProjectsStore } from '@/stores/projects'
import { useToast } from '@/composables/useToast'

vi.mock('@/api/client', () => ({
  default: {
    get: vi.fn().mockResolvedValue({ data: {} }),
    put: vi.fn().mockResolvedValue({ data: {} }),
    post: vi.fn().mockResolvedValue({ data: {} }),
    delete: vi.fn().mockResolvedValue({ data: {} }),
  },
}))
vi.mock('@/api/migration')

const session = (status: 'running' | 'verifying') => ({
  id: 'abc123',
  source_cluster: 'source.example.com',
  target_cluster: 'target.example.com',
  projects: ['shop'],
  steps: [{ name: 'Creating apps (shop-prod)', phase: 'structure', status: 'completed' }],
  status,
})

async function mountMigration() {
  setActivePinia(createPinia())
  useProjectsStore().projects = [{ name: 'shop', environments: [{ name: 'prod' }] }] as never
  const wrapper = mount(Migration, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
  await flushPromises()
  return wrapper
}

describe('the migration page after a reload', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
    vi.mocked(migrationApi.connectProgress).mockReturnValue({ close: vi.fn() } as never)
    vi.mocked(migrationApi.getVerification).mockResolvedValue({ apps: [], status: 'verifying' })
  })

  // The page kept the session only in memory, so a reload during verification
  // showed the start screen while the migration waited for a cutover nobody
  // could reach any more.
  it('returns to the verification of a migration awaiting cutover', async () => {
    vi.mocked(migrationApi.getActiveMigration).mockResolvedValue(session('verifying') as never)

    const wrapper = await mountMigration()

    expect(wrapper.text()).toContain('Verify your apps on the new cluster')
    expect(migrationApi.getVerification).toHaveBeenCalledWith('abc123')
  })

  it('follows the progress of a migration still running', async () => {
    vi.mocked(migrationApi.getActiveMigration).mockResolvedValue(session('running') as never)

    const wrapper = await mountMigration()

    expect(migrationApi.connectProgress).toHaveBeenCalledWith('abc123', expect.any(Function), expect.any(Function))
    expect(wrapper.text()).toContain('Creating apps (shop-prod)')
  })

  // Cutover records its warnings as session steps, and the cutover response
  // carries none, so a resumed page has to keep listening for them.
  it('shows the warnings cutover records after resuming verification', async () => {
    vi.mocked(migrationApi.getActiveMigration).mockResolvedValue(session('verifying') as never)
    vi.mocked(migrationApi.performCutover).mockResolvedValue({ domains: [] })

    const wrapper = await mountMigration()
    expect(migrationApi.connectProgress).toHaveBeenCalledWith('abc123', expect.any(Function), expect.any(Function))
    const onStep = vi.mocked(migrationApi.connectProgress).mock.calls[0][1]

    await wrapper.find('input[placeholder="6-digit code"]').setValue('123456')
    await wrapper.findAll('button').find(b => b.text().includes('apply custom domains'))!.trigger('click')
    onStep({ name: 'Clearing rollback copies on target', phase: 'cutover', status: 'skipped', detail: 'copies remain' })
    await flushPromises()

    expect(wrapper.text()).toContain('Migration complete, with skipped data')
    expect(wrapper.text()).toContain('Clearing rollback copies on target')
  })

  it('starts nothing when the page closes before the lookup answers', async () => {
    let answer: (s: unknown) => void = () => {}
    vi.mocked(migrationApi.getActiveMigration).mockReturnValue(new Promise(resolve => { answer = resolve }) as never)

    const wrapper = await mountMigration()
    wrapper.unmount()
    answer(session('verifying'))
    await flushPromises()

    expect(migrationApi.connectProgress).not.toHaveBeenCalled()
    expect(migrationApi.getVerification).not.toHaveBeenCalled()
  })

  it('shows the start screen when nothing is in progress', async () => {
    vi.mocked(migrationApi.getActiveMigration).mockResolvedValue(null)

    await mountMigration()

    expect(migrationApi.connectProgress).not.toHaveBeenCalled()
    expect(migrationApi.getVerification).not.toHaveBeenCalled()
  })
})

describe('the migration plan', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
    vi.mocked(migrationApi.getActiveMigration).mockResolvedValue(null)
  })

  // A redirect domain needs its DNS record changed like the host it redirects
  // to, so the plan names it.
  it('lists the redirect domains under the app they redirect to', async () => {
    vi.mocked(migrationApi.getPlan).mockResolvedValue({
      target_cluster: 'target.example.com',
      target_endpoint: 'https://api.target.example.com',
      blockers: [],
      warnings: [],
      will_migrate: [{
        kind: 'app', name: 'website', namespace: 'site-prod', status: 'ok',
        host: 'www.example.org', redirect_from: ['example.org'], domain_class: 'custom', disposition: 'move',
      }],
      will_skip: [],
      not_migrated: [],
      conflicts: [],
      receipt: 'r',
    } as never)
    const wrapper = await mountMigration()

    const start = wrapper.findAll('button').find(b => b.text().includes('Migrate to another cluster'))
    expect(start, 'no button leads to the migration form').toBeDefined()
    await start!.trigger('click')
    await wrapper.find('textarea, input[type="text"]').setValue('x'.repeat(40))
    await wrapper.find('input[type="checkbox"]').setValue(true)
    await wrapper.findAll('button').find(b => b.text().includes('Review migration plan'))!.trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="plan-redirect-hosts"]').text()).toContain('example.org')
  })

  // Only one outgoing migration runs at a time, and the server's refusal names
  // the one in the way, so the page shows that reason.
  it('shows why the server refused to start a second migration', async () => {
    vi.mocked(migrationApi.getPlan).mockResolvedValue({
      target_cluster: 'target.example.com',
      target_endpoint: 'https://api.target.example.com',
      blockers: [],
      warnings: [],
      will_migrate: [],
      will_skip: [],
      not_migrated: [],
      conflicts: [],
      receipt: 'r',
    } as never)
    const refusal = 'a migration to earlier.example.org is already in progress on this cluster'
    vi.mocked(migrationApi.startMigration).mockRejectedValue({ response: { status: 409, data: { error: refusal } } })
    const wrapper = await mountMigration()

    await wrapper.findAll('button').find(b => b.text().includes('Migrate to another cluster'))!.trigger('click')
    await wrapper.find('textarea, input[type="text"]').setValue('x'.repeat(40))
    await wrapper.find('input[type="checkbox"]').setValue(true)
    await wrapper.findAll('button').find(b => b.text().includes('Review migration plan'))!.trigger('click')
    await flushPromises()
    await wrapper.find('input[placeholder="6-digit code"]').setValue('123456')
    await wrapper.findAll('button').find(b => b.text().includes('Start migration'))!.trigger('click')
    await flushPromises()

    expect(migrationApi.startMigration).toHaveBeenCalled()
    expect(useToast().toasts.value.map(t => t.message)).toContain(refusal)
  })

  // After a failed run, Cancel is the only way back to the start screen,
  // even though the server preserves the status of a finished migration.
  it('returns to the start screen when cancel finds the migration already ended', async () => {
    vi.mocked(migrationApi.getActiveMigration).mockResolvedValue(session('running') as never)
    const ended = 'The migration has already ended with status failed.'
    vi.mocked(migrationApi.cancelMigration).mockRejectedValue({ response: { status: 409, data: { error: ended } } })
    const wrapper = await mountMigration()

    await wrapper.findAll('button').find(b => b.text().includes('Cancel migration'))!.trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('Migrate to another cluster')
    expect(useToast().toasts.value.map(t => t.message)).toContain(ended)
  })

  it('returns to the start screen when cancel finds the migration gone', async () => {
    vi.mocked(migrationApi.getActiveMigration).mockResolvedValue(session('running') as never)
    vi.mocked(migrationApi.cancelMigration).mockRejectedValue({ response: { status: 404, data: { error: 'session not found' } } })
    const wrapper = await mountMigration()

    await wrapper.findAll('button').find(b => b.text().includes('Cancel migration'))!.trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('Migrate to another cluster')
  })

  it('keeps following the migration when a cutover holds it', async () => {
    // A cutover refusal leaves the migration active, so the page must keep
    // showing its progress.
    vi.mocked(migrationApi.getActiveMigration).mockResolvedValue(session('running') as never)
    const busy = 'A cutover is moving the custom domains.'
    vi.mocked(migrationApi.cancelMigration).mockRejectedValue({ response: { status: 409, data: { error: busy, cutover_running: true } } })
    const wrapper = await mountMigration()

    await wrapper.findAll('button').find(b => b.text().includes('Cancel migration'))!.trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('Creating apps (shop-prod)')
    expect(wrapper.text()).not.toContain('Migrate to another cluster')
    expect(useToast().toasts.value.map(t => t.message)).toContain(busy)
  })

  it('keeps following the migration when the cancel request itself fails', async () => {
    vi.mocked(migrationApi.getActiveMigration).mockResolvedValue(session('running') as never)
    vi.mocked(migrationApi.cancelMigration).mockRejectedValue(new Error('network down'))
    const wrapper = await mountMigration()

    await wrapper.findAll('button').find(b => b.text().includes('Cancel migration'))!.trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('Creating apps (shop-prod)')
    expect(wrapper.text()).not.toContain('Migrate to another cluster')
  })
})
