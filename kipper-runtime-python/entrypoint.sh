#!/bin/sh
# When requirements.txt is mounted, copy the read-only function source to
# /tmp/fn, install dependencies with pip, and load the handler there.

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
