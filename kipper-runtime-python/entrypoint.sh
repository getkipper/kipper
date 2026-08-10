#!/bin/sh
# Kipper Python runtime entrypoint.
#
# When the user has declared third-party dependencies (Phase 3), the
# Function controller writes a requirements.txt into the code ConfigMap.
# That mount is read-only, so we copy the code into a writable /tmp/fn
# and pip install there. The server then loads the handler from
# /tmp/fn instead of /app/function.
#
# Image-based functions (no inline source) skip this entirely.

set -e

if [ -f /app/function/requirements.txt ]; then
  mkdir -p /tmp/fn
  cp /app/function/. /tmp/fn/ -r 2>/dev/null || cp /app/function/* /tmp/fn/ 2>/dev/null || true
  cd /tmp/fn
  echo "Installing user dependencies..."
  pip install --no-cache-dir --target /tmp/fn -r requirements.txt
  export PYTHONPATH="/tmp/fn:${PYTHONPATH}"
  export KIPPER_FUNCTION_PATH="/tmp/fn/handler.py"
fi

exec python /app/server.py
