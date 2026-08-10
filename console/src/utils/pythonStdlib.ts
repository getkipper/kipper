// Python 3.12 standard library modules. Used by the scan-imports and
// AI-assist features to keep stdlib out of the dependency list — pip
// can't install most of these, and even when it can the result is an
// empty placeholder package masking the real stdlib import.
//
// The server-side equivalent lives in
// console-api/controllers/function_controller.go::pythonStdlib. Keep
// the two in sync — both are loaded into the same conversation, so a
// drift would be immediately visible during testing.

export const PYTHON_STDLIB: ReadonlySet<string> = new Set([
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

// NODE_BUILTINS lists Node 22 built-in modules. Same role as the
// Python list above — keep these out of package.json since they
// don't exist on npm.
export const NODE_BUILTINS: ReadonlySet<string> = new Set([
  'assert', 'async_hooks', 'buffer', 'child_process', 'cluster',
  'console', 'constants', 'crypto', 'dgram', 'diagnostics_channel',
  'dns', 'domain', 'events', 'fs', 'http', 'http2', 'https',
  'inspector', 'module', 'net', 'os', 'path', 'perf_hooks', 'process',
  'punycode', 'querystring', 'readline', 'repl', 'stream',
  'string_decoder', 'sys', 'timers', 'tls', 'trace_events', 'tty', 'url',
  'util', 'v8', 'vm', 'wasi', 'worker_threads', 'zlib',
])
