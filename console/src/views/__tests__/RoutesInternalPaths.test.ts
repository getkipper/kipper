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

function group(routeGuard: boolean, refused: string[], publicPaths: string[] = [], refusalReady = true) {
  return {
    name: 'shop-webapp-test',
    namespace: 'shop-test',
    host: 'shop-webapp-test.example.com',
    tls: true,
    project: 'shop',
    environment: 'test',
    health: HEALTH,
    route_guard: routeGuard,
    routes: [{
      path: '/domains-api/',
      service: 'domain-service',
      app: 'domain-service',
      port: 8080,
      health: HEALTH,
      refused_paths: refused,
      public_paths: publicPaths,
      refusal_ready: refusalReady,
    }],
  }
}

async function mountRoutes(g: ReturnType<typeof group>) {
  setActivePinia(createPinia())
  useProjectsStore().globalNamespace = 'shop-test'
  vi.mocked(routesApi.fetchRoutes).mockResolvedValue([g] as never)
  vi.mocked(appsApi.fetchApps).mockResolvedValue([] as never)
  const wrapper = mount(Routes, { attachTo: document.body, global: { stubs: { RouterLink: true } } })
  await flushPromises()
  return wrapper
}

describe('what a route publishes', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    document.body.innerHTML = ''
  })

  it('lists the refused prefixes next to the path they sit under', async () => {
    await mountRoutes(group(true, ['/domains-api/actuator', '/domains-api/.env']))

    const html = document.body.innerHTML
    expect(html).toContain('route-refused-paths')
    expect(html).toContain('/domains-api/actuator')
    expect(html).toContain('/domains-api/.env')
  })

  it('names the paths an opt-out put back into service', async () => {
    await mountRoutes(group(true, ['/domains-api/actuator'], ['/domains-api/actuator/prometheus']))

    expect(document.body.innerHTML).toContain('route-public-paths')
    expect(document.body.innerHTML).toContain('/domains-api/actuator/prometheus')
  })

  it('separates a refusal that is in place from one that is still pending', async () => {
    await mountRoutes(group(true, ['/domains-api/actuator'], [], false))
    expect(document.body.innerHTML).toContain('route-refusal-pending')

    document.body.innerHTML = ''
    await mountRoutes(group(true, ['/domains-api/actuator']))
    expect(document.body.innerHTML).not.toContain('route-refusal-pending')
  })

  it('says the whole prefix is published when nothing is refused', async () => {
    await mountRoutes(group(false, []))

    const html = document.body.innerHTML
    expect(html).toContain('route-publishes-everything')
    expect(html).not.toContain('route-refused-paths')
  })
})
