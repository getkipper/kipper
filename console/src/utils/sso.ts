// Single sign-on into a gated host (a service UI or Grafana): the console
// mints a single-use code the gate exchanges for a per-host session cookie.

export function withSSOCode(rawURL: string, code: string): string {
  try {
    const u = new URL(rawURL)
    u.searchParams.set('kipper_sso', code)
    return u.toString()
  } catch {
    return rawURL
  }
}

export function hostOf(rawURL: string): string {
  try {
    return new URL(rawURL).host
  } catch {
    return ''
  }
}

// Open the tab synchronously to retain browser user activation, then attach
// a single-use SSO code when available. Fall back to the current tab if blocked
// and to the plain URL if minting fails.
export async function openWithSSO(url: string, mint: (host: string) => Promise<string | null>): Promise<void> {
  const tab = window.open('about:blank', '_blank')
  if (tab) tab.opener = null
  const host = hostOf(url)
  const code = host ? await mint(host) : null
  const target = code ? withSSOCode(url, code) : url
  if (tab) {
    tab.location.href = target
  } else {
    window.location.href = target
  }
}
