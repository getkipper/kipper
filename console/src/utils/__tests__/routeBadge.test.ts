import { describe, expect, it } from 'vitest'

import { routeBadge } from '../routeBadge'

describe('routeBadge', () => {
  it('names each state of a route', () => {
    expect(routeBadge({ ingress_ready: true, tls_ready: true }, 'TLS').label).toBe('TLS')
    expect(routeBadge({ ingress_ready: true, tls_ready: false }, 'TLS').label).toBe('TLS pending')
    expect(routeBadge({ ingress_ready: false, tls_ready: false }, 'TLS').label).toBe('Provisioning')
  })

  it('says a closed route without public paths publishes nothing', () => {
    const badge = routeBadge({ ingress_ready: false, tls_ready: false, unpublished: true }, 'TLS')

    expect(badge.label).toBe('Nothing published')
    expect(badge.tone).toBe('neutral')
  })

  it('uses the caller label for a live route', () => {
    expect(routeBadge({ ingress_ready: true, tls_ready: true }, 'TLS active').label).toBe('TLS active')
    expect(routeBadge({ ingress_ready: true, tls_ready: true }, 'TLS active').tone).toBe('good')
  })
})
