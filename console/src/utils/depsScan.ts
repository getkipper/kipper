// Extract imports from inline functions and map them to PyPI/npm dependency
// names. Python aliases and preferred distributions are listed below.

import { PYTHON_STDLIB, NODE_BUILTINS } from './pythonStdlib'

// PYTHON_PACKAGE_MAP rewrites import names to the PyPI distribution
// the user almost certainly wants. The general rule: if a package
// commonly causes pip to compile C and the slim image lacks the
// system headers (pg_config, libxml2, etc.), prefer the
// pre-compiled -binary / -python variant.
const PYTHON_PACKAGE_MAP: Record<string, string> = {
  // PostgreSQL driver — the source build needs libpq-dev which isn't
  // in the slim image. psycopg2-binary ships precompiled wheels.
  psycopg2: 'psycopg2-binary',
  // OpenCV
  cv2: 'opencv-python',
  // Pillow uses "PIL" as the import name but ships as "Pillow" on PyPI.
  PIL: 'Pillow',
  // BeautifulSoup
  bs4: 'beautifulsoup4',
  // PyYAML
  yaml: 'PyYAML',
  // scikit-learn
  sklearn: 'scikit-learn',
  // Microsoft SQL Server via pyodbc.
  pyodbc: 'pyodbc',
  // MySQL — mysqlclient compiles against libmysqlclient. PyMySQL is
  // pure Python and works out of the box on slim images.
  MySQLdb: 'pymysql',
}

// Dependency pairs that trigger a replacement suggestion, in [keep, drop] order.
export const PYTHON_SIBLING_PAIRS: ReadonlyArray<readonly [string, string]> = [
  ['psycopg2-binary', 'psycopg2'],
  ['pymysql', 'mysqlclient'],
  ['Pillow', 'PIL'],
]

export interface ScannedImport {
  name: string
  version: string
}

// Scan import statements, exclude stdlib modules, and map distribution names.
// Results are deduplicated, with import matches followed by from matches.
export function scanPythonImports(code: string): ScannedImport[] {
  const found = new Map<string, string>()
  const re1 = /^\s*import\s+([a-zA-Z0-9_]+)/gm
  const re2 = /^\s*from\s+([a-zA-Z0-9_]+)/gm
  for (const re of [re1, re2]) {
    let m: RegExpExecArray | null
    while ((m = re.exec(code)) !== null) {
      const root = m[1]
      if (PYTHON_STDLIB.has(root)) continue
      const pkg = PYTHON_PACKAGE_MAP[root] ?? root
      if (!found.has(pkg)) found.set(pkg, '*')
    }
  }
  return [...found.entries()].map(([name, version]) => ({ name, version }))
}

// scanNodeImports walks the source for require() / import / from
// patterns and returns the package names. Keeps scoped names whole
// (@scope/pkg) and strips sub-paths (pkg/promise -> pkg).
export function scanNodeImports(code: string): ScannedImport[] {
  const found = new Map<string, string>()
  const patterns = [
    /require\(\s*['"]([^'"./][^'"]*)['"]\s*\)/g,
    /from\s+['"]([^'"./][^'"]*)['"]/g,
    /import\s+['"]([^'"./][^'"]*)['"]/g,
  ]
  for (const re of patterns) {
    let m: RegExpExecArray | null
    while ((m = re.exec(code)) !== null) {
      const raw = m[1].replace(/^node:/, '')
      const pkg = raw.startsWith('@')
        ? raw.split('/').slice(0, 2).join('/')
        : raw.split('/')[0]
      if (NODE_BUILTINS.has(pkg)) continue
      if (!found.has(pkg)) found.set(pkg, '*')
    }
  }
  return [...found.entries()].map(([name, version]) => ({ name, version }))
}

// detectSiblingConflicts returns the offending pairs present in the
// deps list. UI uses this to render a warning row per pair.
export function detectSiblingConflicts(depNames: string[]): Array<{
  keep: string
  drop: string
}> {
  const set = new Set(depNames)
  const conflicts: Array<{ keep: string; drop: string }> = []
  for (const [keep, drop] of PYTHON_SIBLING_PAIRS) {
    if (set.has(keep) && set.has(drop)) {
      conflicts.push({ keep, drop })
    }
  }
  return conflicts
}
