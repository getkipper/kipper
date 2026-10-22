// @vitest-environment jsdom
import { describe, expect, it, vi } from 'vitest'
import { handleCodeCopyClick, renderMarkdown } from '../markdown'

describe('renderMarkdown', () => {
  it('drops event handlers from raw HTML', () => {
    const html = renderMarkdown('look <img src="x" onerror="alert(1)"> here')
    expect(html).not.toContain('onerror')
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

  it('copies the code, never markup from the language label', () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    const message = document.createElement('div')
    message.innerHTML = renderMarkdown('```<pre><code>wrong</code></pre>\necho intended\n```')

    handleCodeCopyClick({ target: message.querySelector('[data-copy-code]') } as unknown as MouseEvent)

    expect(writeText).toHaveBeenCalledWith('echo intended')
  })
})
