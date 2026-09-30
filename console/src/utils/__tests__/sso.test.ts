// @vitest-environment happy-dom
import { describe, it, expect, vi, afterEach } from 'vitest'
import { withSSOCode, openWithSSO } from '../sso'

afterEach(() => {
  vi.restoreAllMocks()
})

describe('withSSOCode', () => {
  it('adds the code and keeps the path and query', () => {
    expect(withSSOCode('https://grafana.example.com/explore?left=x', 'c1')).toBe(
      'https://grafana.example.com/explore?left=x&kipper_sso=c1',
    )
  })

  it('returns an unparseable URL unchanged', () => {
    expect(withSSOCode('not a url', 'c1')).toBe('not a url')
  })
})

describe('openWithSSO', () => {
  it('mints a code for the host and sends the new tab there', async () => {
    const tab = { opener: {} as unknown, location: { href: '' } }
    vi.spyOn(window, 'open').mockReturnValue(tab as unknown as Window)
    const mint = vi.fn().mockResolvedValue('code-1')

    await openWithSSO('https://grafana.example.com/d/abc', mint)

    expect(window.open).toHaveBeenCalledWith('about:blank', '_blank')
    expect(tab.opener).toBeNull()
    expect(mint).toHaveBeenCalledWith('grafana.example.com')
    expect(tab.location.href).toBe('https://grafana.example.com/d/abc?kipper_sso=code-1')
  })

  it('falls back to the plain URL when no code can be minted', async () => {
    const tab = { opener: {} as unknown, location: { href: '' } }
    vi.spyOn(window, 'open').mockReturnValue(tab as unknown as Window)

    await openWithSSO('https://grafana.example.com/', vi.fn().mockResolvedValue(null))

    expect(tab.location.href).toBe('https://grafana.example.com/')
  })
})
