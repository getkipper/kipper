#!/bin/sh
# When package.json is mounted, copy the read-only function source to
# /tmp/fn, install dependencies with npm, and load the handler there.

set -e

if [ -f /app/function/package.json ]; then
  mkdir -p /tmp/fn
  cp /app/function/. /tmp/fn/ -r 2>/dev/null || cp /app/function/* /tmp/fn/ 2>/dev/null || true
  cd /tmp/fn
  echo "Installing user dependencies..."
  npm install --production --no-audit --no-fund
  export KIPPER_FUNCTION_PATH="/tmp/fn/index.js"
fi

exec node /app/server.js
