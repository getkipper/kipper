import DOMPurify from 'dompurify'
import { marked, type Tokens, type Token, type TokensList } from 'marked'
import hljs from 'highlight.js/lib/core'
import javascript from 'highlight.js/lib/languages/javascript'
import typescript from 'highlight.js/lib/languages/typescript'
import python from 'highlight.js/lib/languages/python'
import bash from 'highlight.js/lib/languages/bash'
import yaml from 'highlight.js/lib/languages/yaml'
import json from 'highlight.js/lib/languages/json'
import sql from 'highlight.js/lib/languages/sql'
import dockerfile from 'highlight.js/lib/languages/dockerfile'

hljs.registerLanguage('javascript', javascript)
hljs.registerLanguage('js', javascript)
hljs.registerLanguage('typescript', typescript)
hljs.registerLanguage('ts', typescript)
hljs.registerLanguage('python', python)
hljs.registerLanguage('bash', bash)
hljs.registerLanguage('sh', bash)
hljs.registerLanguage('shell', bash)
hljs.registerLanguage('yaml', yaml)
hljs.registerLanguage('yml', yaml)
hljs.registerLanguage('json', json)
hljs.registerLanguage('sql', sql)
hljs.registerLanguage('dockerfile', dockerfile)

marked.setOptions({
  breaks: true,
  gfm: true,
})

/** Preserves inline formatting while omitting the paragraph wrapper. */
function parseInline(tokens: Token[]): string {
  return marked.parser([{ type: 'paragraph', raw: '', text: '', tokens }] as TokensList)
    .replace(/^<p>/, '').replace(/<\/p>\n?$/, '')
}

function escapeHTML(text: string): string {
  return text
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
}

const renderer = new marked.Renderer()

renderer.heading = (token: Tokens.Heading) => {
  const content = token.tokens ? parseInline(token.tokens) : token.text
  switch (token.depth) {
    case 1: return `<h1 class="mt-4 mb-2 text-lg font-bold text-slate-100">${content}</h1>`
    case 2: return `<h2 class="mt-4 mb-1.5 text-sm font-bold text-slate-100">${content}</h2>`
    case 3: return `<h3 class="mt-3 mb-1 text-xs font-bold text-slate-200">${content}</h3>`
    default: return `<h4 class="mt-2 mb-1 text-xs font-semibold text-slate-300">${content}</h4>`
  }
}

renderer.code = (token: Tokens.Code) => {
  let highlighted: string
  if (token.lang && hljs.getLanguage(token.lang)) {
    highlighted = hljs.highlight(token.text, { language: token.lang }).value
  } else {
    highlighted = escapeHTML(token.text)
  }

  const langLabel = escapeHTML(token.lang || '')

  return `<div data-code-block class="group/code relative my-2">` +
    `<div class="flex items-center justify-between rounded-t-lg bg-slate-800 px-3 py-1.5">` +
    `<span class="text-[10px] font-medium text-slate-500">${langLabel}</span>` +
    `<button data-copy-code class="text-[10px] text-slate-500 hover:text-slate-300 transition-colors">Copy</button>` +
    `</div>` +
    `<pre class="rounded-b-lg bg-slate-950 p-3 font-mono text-xs leading-relaxed overflow-x-auto"><code class="hljs">${highlighted}</code></pre>` +
    `</div>`
}

renderer.codespan = (token: Tokens.Codespan) => {
  return `<code class="rounded-sm bg-slate-800 px-1.5 py-0.5 font-mono text-xs text-kipper-300">${token.text}</code>`
}

renderer.table = (token: Tokens.Table) => {
  let header = '<tr class="border-b border-slate-700">'
  for (const cell of token.header) {
    const content = cell.tokens ? parseInline(cell.tokens) : cell.text
    header += `<th class="px-2 py-1.5 font-medium text-left text-slate-400">${content}</th>`
  }
  header += '</tr>'

  let body = ''
  for (const row of token.rows) {
    body += '<tr class="border-b border-slate-800">'
    for (const cell of row) {
      const content = cell.tokens ? parseInline(cell.tokens) : cell.text
      body += `<td class="px-2 py-1.5">${content}</td>`
    }
    body += '</tr>'
  }

  return `<table class="my-3 w-full text-xs"><thead>${header}</thead><tbody class="text-slate-300">${body}</tbody></table>`
}

renderer.blockquote = (token: Tokens.Blockquote) => {
  const content = token.tokens ? marked.parser(token.tokens as TokensList) : token.text
  return `<blockquote class="my-2 border-l-2 border-kipper-500 pl-3 text-slate-400 italic">${content}</blockquote>`
}

renderer.hr = () => '<hr class="my-4 border-slate-700" />'

marked.use({ renderer })

/**
 * Returns sanitized HTML with Markdown formatting, syntax highlighting and code copy buttons.
 * Sanitization protects against untrusted cluster data in AI responses.
 */
export function renderMarkdown(text: string): string {
  return DOMPurify.sanitize(marked.parse(text) as string)
}

/**
 * Copies code from the clicked button's block in the rendered DOM so each message copies its own code.
 * Ignores clicks outside copy buttons and buttons with no matching code element.
 * Requires an element event target and the Clipboard API.
 * Shows “Copied!” for two seconds without waiting for the clipboard write to complete.
 * Clipboard errors are not handled.
 */
export function handleCodeCopyClick(e: MouseEvent): void {
  const target = e.target as HTMLElement
  const btn = target.closest('[data-copy-code]') as HTMLElement
  if (!btn) return

  const code = btn.closest('[data-code-block]')?.querySelector(':scope > pre > code')?.textContent
  if (code != null) {
    navigator.clipboard.writeText(code)
    btn.textContent = 'Copied!'
    setTimeout(() => { btn.textContent = 'Copy' }, 2000)
  }
}
