// @vitest-environment jsdom
import { describe, expect, it } from 'vitest'
import { renderMarkdown } from '../markdown'

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
    expect(html).toContain('data-copy-code="0"')
  })
})
