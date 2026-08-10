<script setup lang="ts">
import { ref, computed, nextTick, watch } from 'vue'
import { Send, Sparkles, Copy, Check, ArrowDownToLine, Paperclip, X, FileText } from 'lucide-vue-next'
import { streamChat, type ChatMessage, type ChatBinding, type ChatDBSchema } from '@/api/ai-chat'
import { renderMarkdown, handleCodeCopyClick } from '@/utils/markdown'
import { useToast } from '@/composables/useToast'

interface Attachment {
  name: string
  size: number
  content: string
}

// UIMessage extends ChatMessage with display-only fields. The wire
// payload (content) carries file content as fenced blocks inside the
// user message; the bubble renders `displayText` and `attachments`
// chips so the chat history doesn't show a wall of code.
interface UIMessage extends ChatMessage {
  attachments?: Attachment[]
  displayText?: string
}

// Whitelist of text-ish extensions we accept. Binary formats (images,
// PDFs, archives) need provider-side multimodal support, which this
// implementation deliberately does not pursue — see the plan doc.
const ALLOWED_EXTENSIONS = new Set([
  'java', 'kt', 'scala', 'groovy',
  'py', 'js', 'jsx', 'ts', 'tsx', 'mjs', 'cjs',
  'go', 'rs', 'rb', 'php', 'cs', 'cpp', 'c', 'h', 'hpp',
  'yml', 'yaml', 'xml', 'json', 'toml', 'ini', 'properties', 'env',
  'sh', 'bash', 'zsh', 'fish',
  'sql', 'graphql', 'proto',
  'md', 'txt', 'log',
  'gradle', 'sbt', 'mod', 'sum', 'lock',
  'dockerfile', 'tf', 'hcl',
])

const MAX_FILE_BYTES = 256 * 1024
const MAX_TOTAL_BYTES = 1024 * 1024

interface Props {
  code: string
  runtime: string
  // Function-mode context (mode=code).
  bindings?: ChatBinding[]
  envKeys?: string[]
  secretKeys?: string[]
  dependencies?: Record<string, string>
  // Mode toggle: "code" (default) or "db".
  mode?: 'code' | 'db'
  // Database context (mode=db). Names + types only — no row data.
  dbSchema?: ChatDBSchema
  sql?: string
}

const props = withDefaults(defineProps<Props>(), { mode: 'code' })

const emit = defineEmits<{
  apply: [code: string]
  addDependency: [name: string]
}>()

const messages = ref<UIMessage[]>([])
const input = ref('')
const streaming = ref(false)
const messagesEl = ref<HTMLElement | null>(null)
const copiedIndex = ref<number | null>(null)
const fileInputEl = ref<HTMLInputElement | null>(null)
const dragOver = ref(false)
const pendingAttachments = ref<Attachment[]>([])
const toast = useToast()

const totalAttachmentBytes = computed(() =>
  pendingAttachments.value.reduce((sum, a) => sum + a.size, 0),
)

// Rough token estimate. The 4 bytes/token rule of thumb is close
// enough for source code; we surface it so users notice when they're
// approaching context-window territory.
const estimatedTokens = computed(() => {
  const messageBytes = messages.value.reduce((sum, m) => sum + m.content.length, 0)
  const inputBytes = input.value.length
  return Math.ceil((totalAttachmentBytes.value + messageBytes + inputBytes) / 4)
})

function fileExtension(name: string): string {
  const lower = name.toLowerCase()
  if (lower === 'dockerfile' || lower.endsWith('/dockerfile')) return 'dockerfile'
  const dot = lower.lastIndexOf('.')
  if (dot < 0) return ''
  return lower.slice(dot + 1)
}

async function readFileAsText(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result ?? ''))
    reader.onerror = () => reject(reader.error || new Error('read failed'))
    reader.readAsText(file)
  })
}

async function addFiles(files: FileList | File[]) {
  const list = Array.from(files)
  for (const file of list) {
    const ext = fileExtension(file.name)
    if (!ALLOWED_EXTENSIONS.has(ext)) {
      toast.error(`${file.name} — file type not supported (text source files only)`)
      continue
    }
    if (file.size > MAX_FILE_BYTES) {
      toast.error(`${file.name} — too large (max ${MAX_FILE_BYTES / 1024}KB per file)`)
      continue
    }
    if (totalAttachmentBytes.value + file.size > MAX_TOTAL_BYTES) {
      toast.error(`${file.name} — total attachment size would exceed ${MAX_TOTAL_BYTES / 1024}KB`)
      continue
    }
    try {
      const content = await readFileAsText(file)
      pendingAttachments.value.push({ name: file.name, size: file.size, content })
    } catch {
      toast.error(`${file.name} — could not be read`)
    }
  }
  if (fileInputEl.value) fileInputEl.value.value = ''
}

function removeAttachment(idx: number) {
  pendingAttachments.value.splice(idx, 1)
}

function onPickFiles(e: Event) {
  const target = e.target as HTMLInputElement
  if (target.files && target.files.length > 0) {
    addFiles(target.files)
  }
}

function onDrop(e: DragEvent) {
  e.preventDefault()
  dragOver.value = false
  if (e.dataTransfer?.files && e.dataTransfer.files.length > 0) {
    addFiles(e.dataTransfer.files)
  }
}

function onDragOver(e: DragEvent) {
  e.preventDefault()
  dragOver.value = true
}

function onDragLeave() {
  dragOver.value = false
}

// Build the wire content: attachment blocks first (so the model has the
// source before it sees the question), then the user's question. The
// fenced code block language tag is inferred from the extension so
// syntax highlighting works downstream if the model echoes it back.
function formatWireContent(text: string, attachments: Attachment[]): string {
  if (attachments.length === 0) return text
  const blocks = attachments.map((a) => {
    const ext = fileExtension(a.name)
    const lang = ext === 'java' ? 'java' : ext === 'kt' ? 'kotlin' : ext
    return `--- ${a.name} ---\n\`\`\`${lang}\n${a.content}\n\`\`\``
  })
  return `attached files:\n\n${blocks.join('\n\n')}\n\n${text}`
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`
}

function scrollToBottom() {
  nextTick(() => {
    if (messagesEl.value) {
      messagesEl.value.scrollTop = messagesEl.value.scrollHeight
    }
  })
}

async function send() {
  const text = input.value.trim()
  if ((!text && pendingAttachments.value.length === 0) || streaming.value) return

  const attachments = pendingAttachments.value
  const wireContent = formatWireContent(text, attachments)

  messages.value.push({
    role: 'user',
    content: wireContent,
    displayText: text,
    attachments: attachments.length ? [...attachments] : undefined,
  })
  input.value = ''
  pendingAttachments.value = []
  scrollToBottom()

  // Add placeholder for assistant response
  messages.value.push({ role: 'assistant', content: '' })
  streaming.value = true
  scrollToBottom()

  const assistantIdx = messages.value.length - 1

  // The wire format only carries `role` and `content`; strip the UI-only
  // attachments field before calling the API.
  const wireMessages: ChatMessage[] = messages.value
    .slice(0, -1)
    .map(({ role, content }) => ({ role, content }))

  await streamChat(
    {
      messages: wireMessages,
      code: props.code,
      runtime: props.runtime,
      mode: props.mode,
      bindings: props.bindings,
      env_keys: props.envKeys,
      secret_keys: props.secretKeys,
      dependencies: props.dependencies,
      db_schema: props.dbSchema,
      sql: props.sql,
    },
    (content) => {
      messages.value[assistantIdx].content += content
      scrollToBottom()
    },
    () => {
      streaming.value = false
    },
    (error) => {
      messages.value[assistantIdx].content = `Error: ${error}`
      streaming.value = false
    },
  )
}

function extractCodeBlocks(content: string): { text: string; lang: string; code: string }[] {
  const blocks: { text: string; lang: string; code: string }[] = []
  const regex = /```(\w*)\n([\s\S]*?)```/g
  let match
  while ((match = regex.exec(content)) !== null) {
    blocks.push({ text: match[0], lang: match[1], code: match[2].trim() })
  }
  return blocks
}

// missingImports returns the names of packages used in `code` that are
// not yet present in the function's declared dependencies. We use this
// to surface a one-click "Add to dependencies" CTA next to AI-suggested
// code blocks. Stdlib-style names (no slash, no scope) are filtered out
// for Node when they're known built-ins, and Python well-known stdlib.
function missingImports(code: string): string[] {
  const installed = new Set(Object.keys(props.dependencies || {}))
  const found = new Set<string>()
  if (props.runtime === 'python') {
    // Python 3.12 stdlib. Mirrors the server-side filter in
    // controllers/function_controller.go::pythonStdlib so the AI's
    // "+ pkg" suggestion never offers a stdlib module that pip can't
    // install (some are 404, some have empty PyPI placeholders).
    const stdlib = new Set([
      'abc', 'argparse', 'array', 'ast', 'asyncio',
      'base64', 'binascii', 'bisect', 'builtins', 'bz2',
      'calendar', 'codecs', 'collections', 'concurrent',
      'configparser', 'contextlib', 'contextvars', 'copy', 'copyreg', 'csv',
      'ctypes', 'curses',
      'dataclasses', 'datetime', 'decimal', 'difflib', 'dis', 'doctest',
      'email', 'encodings', 'enum', 'errno',
      'fcntl', 'filecmp', 'fileinput', 'fnmatch', 'fractions', 'functools',
      'gc', 'getopt', 'getpass', 'gettext', 'glob', 'graphlib', 'grp', 'gzip',
      'hashlib', 'heapq', 'hmac', 'html', 'http',
      'imaplib', 'importlib', 'inspect', 'io', 'ipaddress', 'itertools',
      'json',
      'keyword',
      'linecache', 'locale', 'logging', 'lzma',
      'math', 'mimetypes', 'mmap', 'multiprocessing',
      'netrc', 'numbers',
      'operator', 'os',
      'pathlib', 'pdb', 'pickle', 'pickletools', 'pkgutil', 'platform',
      'plistlib', 'poplib', 'posix', 'posixpath', 'pprint', 'profile',
      'pstats', 'pty', 'pwd', 'py_compile', 'pyclbr', 'pydoc',
      'queue', 'quopri',
      'random', 're', 'readline', 'reprlib', 'resource', 'rlcompleter', 'runpy',
      'sched', 'secrets', 'select', 'selectors', 'shelve', 'shlex', 'shutil',
      'signal', 'site', 'smtplib', 'sndhdr', 'socket', 'socketserver',
      'sqlite3', 'ssl', 'stat', 'statistics', 'string', 'stringprep', 'struct',
      'subprocess', 'symtable', 'sys', 'sysconfig', 'syslog',
      'tabnanny', 'tarfile', 'telnetlib', 'tempfile', 'termios', 'test',
      'textwrap', 'threading', 'time', 'timeit', 'tkinter', 'token', 'tokenize',
      'tomllib', 'trace', 'traceback', 'tracemalloc', 'tty', 'turtle', 'types', 'typing',
      'unicodedata', 'unittest', 'urllib', 'uu', 'uuid',
      'venv',
      'warnings', 'wave', 'weakref', 'webbrowser', 'wsgiref',
      'xdrlib', 'xml', 'xmlrpc',
      'zipapp', 'zipfile', 'zipimport', 'zlib', 'zoneinfo',
    ])
    const re1 = /^\s*import\s+([a-zA-Z_][\w]*)/gm
    const re2 = /^\s*from\s+([a-zA-Z_][\w]*)/gm
    for (const re of [re1, re2]) {
      let m: RegExpExecArray | null
      while ((m = re.exec(code)) !== null) {
        const name = m[1]
        if (!stdlib.has(name) && !installed.has(name)) found.add(name)
      }
    }
  } else {
    const nodeBuiltins = new Set([
      'fs', 'path', 'os', 'http', 'https', 'crypto', 'url', 'util',
      'stream', 'events', 'buffer', 'querystring', 'zlib', 'child_process',
      'process', 'assert', 'net', 'tls', 'dns', 'cluster', 'readline',
    ])
    const re1 = /require\(\s*['"]([^'"]+)['"]\s*\)/g
    const re2 = /from\s+['"]([^'"]+)['"]/g
    const re3 = /import\s+['"]([^'"]+)['"]/g
    for (const re of [re1, re2, re3]) {
      let m: RegExpExecArray | null
      while ((m = re.exec(code)) !== null) {
        const raw = m[1]
        // Skip relative imports.
        if (raw.startsWith('.') || raw.startsWith('/')) continue
        // Strip node: prefix.
        const cleaned = raw.replace(/^node:/, '')
        // Reduce sub-paths like "pg/promise" → "pg"; preserve scoped pkgs.
        const pkg = cleaned.startsWith('@')
          ? cleaned.split('/').slice(0, 2).join('/')
          : cleaned.split('/')[0]
        if (nodeBuiltins.has(pkg) || installed.has(pkg)) continue
        found.add(pkg)
      }
    }
  }
  return Array.from(found).sort()
}

function applyCode(code: string) {
  emit('apply', code)
}

function addDependency(name: string) {
  emit('addDependency', name)
}

// submitPrompt lets the parent fire a prepared message (e.g. "Explain
// this query: ..."). Exposed via defineExpose below.
async function submitPrompt(text: string) {
  if (streaming.value) return
  input.value = text
  await send()
}

defineExpose({ submitPrompt })

function copyCode(code: string, idx: number) {
  navigator.clipboard.writeText(code)
  copiedIndex.value = idx
  setTimeout(() => { copiedIndex.value = null }, 2000)
}

// renderMarkdown is imported from @/utils/markdown

watch(() => props.code, () => {
  // Reset when switching functions
  if (messages.value.length === 0) return
})
</script>

<template>
  <div class="flex h-full flex-col bg-slate-900">
    <!-- Header -->
    <div class="flex items-center gap-2 border-b border-slate-800 px-4 py-3">
      <Sparkles class="h-4 w-4 text-kipper-400" />
      <span class="text-xs font-semibold text-slate-300">AI Assistant</span>
    </div>

    <!-- Messages -->
    <div ref="messagesEl" class="flex-1 overflow-y-auto px-4 py-3 space-y-4">
      <!-- Empty state -->
      <div v-if="!messages.length" class="flex flex-col items-center justify-center py-8 text-center">
        <Sparkles class="mb-3 h-8 w-8 text-slate-600" />
        <p class="text-sm font-medium text-slate-400">{{ mode === 'db' ? 'Ask me about your schema or queries' : 'Ask me anything about your code' }}</p>
        <div class="mt-3 space-y-1.5">
          <template v-if="mode === 'db'">
            <button
              @click="input = 'List the tables in this database with a one-line description of each'; send()"
              class="block w-full rounded-lg border border-slate-700 px-3 py-1.5 text-left text-xs text-slate-500 transition-colors hover:border-slate-600 hover:text-slate-400"
            >
              Summarise the schema
            </button>
            <button
              @click="input = 'Suggest indexes that would help typical queries on this schema'; send()"
              class="block w-full rounded-lg border border-slate-700 px-3 py-1.5 text-left text-xs text-slate-500 transition-colors hover:border-slate-600 hover:text-slate-400"
            >
              Suggest useful indexes
            </button>
            <button
              @click="input = 'Write a query that joins all related tables and returns one row per top-level entity'; send()"
              class="block w-full rounded-lg border border-slate-700 px-3 py-1.5 text-left text-xs text-slate-500 transition-colors hover:border-slate-600 hover:text-slate-400"
            >
              Write a join query
            </button>
          </template>
          <template v-else>
            <button
              @click="input = 'Write a function that returns HTML for a landing page'; send()"
              class="block w-full rounded-lg border border-slate-700 px-3 py-1.5 text-left text-xs text-slate-500 transition-colors hover:border-slate-600 hover:text-slate-400"
            >
              Write a function that returns HTML for a landing page
            </button>
            <button
              @click="input = 'Add error handling to this code'; send()"
              class="block w-full rounded-lg border border-slate-700 px-3 py-1.5 text-left text-xs text-slate-500 transition-colors hover:border-slate-600 hover:text-slate-400"
            >
              Add error handling to this code
            </button>
            <button
              @click="input = 'Explain what this function does'; send()"
              class="block w-full rounded-lg border border-slate-700 px-3 py-1.5 text-left text-xs text-slate-500 transition-colors hover:border-slate-600 hover:text-slate-400"
            >
              Explain what this function does
            </button>
          </template>
        </div>
      </div>

      <!-- Message list -->
      <div v-for="(msg, i) in messages" :key="i" class="text-xs leading-relaxed">
        <!-- User message -->
        <div v-if="msg.role === 'user'" class="flex justify-end">
          <div class="max-w-[85%] rounded-lg bg-kipper-600 px-3 py-2 text-white">
            <!-- Attachment chips render before the text. The wire content
                 has the file content as fenced blocks; the bubble keeps
                 it tidy by showing just the names. -->
            <div v-if="msg.attachments && msg.attachments.length" class="mb-1.5 flex flex-wrap gap-1">
              <span
                v-for="(a, ai) in msg.attachments"
                :key="ai"
                class="inline-flex items-center gap-1 rounded-md bg-white/15 px-1.5 py-0.5 text-[10px] font-medium"
                :title="`${a.name} — ${formatBytes(a.size)}`"
              >
                <FileText class="h-2.5 w-2.5" />
                {{ a.name }}
              </span>
            </div>
            <span v-if="msg.displayText">{{ msg.displayText }}</span>
            <span v-else-if="!msg.attachments || !msg.attachments.length">{{ msg.content }}</span>
          </div>
        </div>

        <!-- Assistant message -->
        <div v-else>
          <div class="ai-prose text-slate-300" @click="handleCodeCopyClick">
            <div v-html="renderMarkdown(msg.content)" />

            <!-- Action buttons for code blocks -->
            <div v-for="(block, bi) in extractCodeBlocks(msg.content)" :key="bi" class="mt-1 flex flex-wrap items-center gap-1">
              <button
                @click="applyCode(block.code)"
                class="inline-flex items-center gap-1 rounded-md bg-kipper-600/20 px-2 py-1 text-[10px] font-medium text-kipper-400 transition-colors hover:bg-kipper-600/30"
              >
                <ArrowDownToLine class="h-3 w-3" />
                Apply to editor
              </button>
              <button
                @click="copyCode(block.code, i * 100 + bi)"
                class="inline-flex items-center gap-1 rounded-md bg-slate-800 px-2 py-1 text-[10px] font-medium text-slate-500 transition-colors hover:text-slate-400"
              >
                <Check v-if="copiedIndex === i * 100 + bi" class="h-3 w-3 text-emerald-400" />
                <Copy v-else class="h-3 w-3" />
                {{ copiedIndex === i * 100 + bi ? 'Copied' : 'Copy' }}
              </button>
              <button
                v-for="pkg in missingImports(block.code)"
                :key="pkg"
                @click="addDependency(pkg)"
                class="inline-flex items-center gap-1 rounded-md bg-amber-500/15 px-2 py-1 text-[10px] font-medium text-amber-400 transition-colors hover:bg-amber-500/25"
                :title="`Add ${pkg} to dependencies — the runtime will install it on next deploy`"
              >
                + {{ pkg }}
              </button>
            </div>
          </div>

          <!-- Streaming indicator -->
          <div v-if="streaming && i === messages.length - 1 && !msg.content" class="flex items-center gap-1.5 text-slate-500">
            <span class="inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-kipper-500" />
            <span class="inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-kipper-500" style="animation-delay: 0.15s" />
            <span class="inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-kipper-500" style="animation-delay: 0.3s" />
          </div>
        </div>
      </div>
    </div>

    <!-- Input -->
    <div
      class="relative border-t border-slate-800 px-4 py-3"
      :class="dragOver ? 'bg-kipper-950/40' : ''"
      @dragover="onDragOver"
      @dragleave="onDragLeave"
      @drop="onDrop"
    >
      <!-- Drop overlay -->
      <div
        v-if="dragOver"
        class="pointer-events-none absolute inset-1 flex items-center justify-center rounded-lg border-2 border-dashed border-kipper-500 bg-kipper-950/70 text-xs font-medium text-kipper-300"
      >
        Drop files to attach
      </div>

      <!-- Pending attachments -->
      <div v-if="pendingAttachments.length" class="mb-2 flex flex-wrap gap-1">
        <span
          v-for="(a, ai) in pendingAttachments"
          :key="ai"
          class="inline-flex items-center gap-1 rounded-md border border-slate-700 bg-slate-800 px-1.5 py-0.5 text-[10px] text-slate-300"
        >
          <FileText class="h-2.5 w-2.5 text-slate-500" />
          {{ a.name }}
          <span class="text-slate-500">{{ formatBytes(a.size) }}</span>
          <button
            @click="removeAttachment(ai)"
            class="ml-0.5 rounded-sm text-slate-500 hover:bg-slate-700 hover:text-slate-300"
            :title="`Remove ${a.name}`"
          >
            <X class="h-2.5 w-2.5" />
          </button>
        </span>
      </div>

      <div class="flex items-center gap-2">
        <input
          ref="fileInputEl"
          type="file"
          multiple
          class="hidden"
          @change="onPickFiles"
        />
        <button
          type="button"
          @click="fileInputEl?.click()"
          :disabled="streaming"
          class="rounded-lg p-2 text-slate-400 transition-colors hover:bg-slate-800 hover:text-slate-200 disabled:opacity-40"
          title="Attach files (drag-drop also works)"
        >
          <Paperclip class="h-3.5 w-3.5" />
        </button>
        <input
          v-model="input"
          type="text"
          placeholder="Ask about your code..."
          class="flex-1 rounded-lg border border-slate-700 bg-slate-800 px-3 py-2 text-xs text-slate-200 placeholder-slate-500 focus:border-kipper-500 focus:outline-none"
          :disabled="streaming"
          @keyup.enter="send"
        />
        <button
          @click="send"
          :disabled="(!input.trim() && !pendingAttachments.length) || streaming"
          class="rounded-lg bg-kipper-600 p-2 text-white transition-colors hover:bg-kipper-700 disabled:opacity-40"
        >
          <Send class="h-3.5 w-3.5" />
        </button>
      </div>

      <!-- Token estimate row. Only render when something is in flight
           (pending attachments or accumulated chat) so the empty state
           stays clean. -->
      <div
        v-if="estimatedTokens > 1000"
        class="mt-1.5 text-right text-[10px]"
        :class="estimatedTokens > 100000 ? 'text-amber-400' : 'text-slate-500'"
      >
        ~{{ estimatedTokens.toLocaleString() }} tokens
      </div>
    </div>
  </div>
</template>
