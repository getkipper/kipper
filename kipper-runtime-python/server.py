"""Kipper Python function runtime.

Two modes, dispatched on the KIPPER_MODE env var:

    http  (default) -- start an HTTP server that exposes the user's
                       handler at /, /event, and /health. Used by HTTP-
                       triggered functions and the kipper-poll sidecar.

    batch           -- import the handler, invoke it once with a
                       synthetic event ({type: cron, timestamp: <ISO>}),
                       and exit. Used by cron-triggered functions
                       running as a Kubernetes CronJob -- no HTTP
                       server, no scale-to-zero overlap, no stacked
                       timeouts. Exit code is 0 on success, 1 if the
                       handler raises or returns a dict with an "error"
                       key.
"""

import importlib.util
import json
import os
import sys
import traceback
from datetime import datetime, timezone
from http.server import HTTPServer, BaseHTTPRequestHandler

fn_path = os.environ.get('KIPPER_FUNCTION_PATH', '/app/function/handler.py')
mode = os.environ.get('KIPPER_MODE', 'http')

handler = None
try:
    spec = importlib.util.spec_from_file_location('handler', fn_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    handler = (
        getattr(mod, 'handler', None)
        or getattr(mod, 'handle', None)
        or getattr(mod, 'main', None)
    )
    if handler:
        print(f'Function loaded from {fn_path}')
    else:
        msg = f'no handler/handle/main function found in {fn_path}'
        print(f'Error: {msg}')
        if mode == 'batch':
            sys.exit(1)
        handler = lambda event, ctx=None: {'error': msg}
except Exception as e:
    print(f'Failed to load function from {fn_path}: {e}')
    if mode == 'batch':
        traceback.print_exc()
        sys.exit(1)
    handler = lambda event, ctx=None: {'error': str(e)}


if mode == 'batch':
    event = {
        'type': os.environ.get('KIPPER_TRIGGER', 'cron'),
        'timestamp': datetime.now(timezone.utc).isoformat(),
    }
    ctx = {'mode': 'batch'}
    try:
        result = handler(event, ctx)
        if isinstance(result, dict) and 'error' in result:
            print(f'Function returned error: {result["error"]}')
            sys.exit(1)
        if result is not None:
            print(json.dumps(result, default=str))
        sys.exit(0)
    except Exception as e:
        traceback.print_exc()
        sys.exit(1)


class FunctionHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        content_length = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(content_length) if content_length else b'{}'

        try:
            event = json.loads(body)
        except json.JSONDecodeError:
            event = {'raw': body.decode('utf-8', errors='replace')}

        try:
            result = handler(event, {
                'method': 'POST',
                'path': self.path,
                'headers': dict(self.headers),
            })

            response = json.dumps(result or {'ok': True}).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(response)))
            self.end_headers()
            self.wfile.write(response)
        except Exception as e:
            traceback.print_exc()
            error = json.dumps({'error': str(e)}).encode()
            self.send_response(500)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(error)))
            self.end_headers()
            self.wfile.write(error)

    def do_GET(self):
        if self.path == '/health':
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            self.wfile.write(b'{"ok":true}')
            return

        try:
            result = handler({
                'method': 'GET',
                'path': self.path,
                'headers': dict(self.headers),
            })
            response = json.dumps(result or {'ok': True}).encode()
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(response)))
            self.end_headers()
            self.wfile.write(response)
        except Exception as e:
            traceback.print_exc()
            error = json.dumps({'error': str(e)}).encode()
            self.send_response(500)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(error)))
            self.end_headers()
            self.wfile.write(error)

    def log_message(self, format, *args):
        print(f'{self.client_address[0]} - {format % args}')


port = int(os.environ.get('PORT', '8080'))
print(f'Kipper function ready on :{port}')
HTTPServer(('0.0.0.0', port), FunctionHandler).serve_forever()
