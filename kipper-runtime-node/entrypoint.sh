#!/bin/sh
# Kipper Node runtime entrypoint.
#
# When the user has declared third-party dependencies (Phase 3), the
# Function controller writes a package.json into the code ConfigMap.
# That mount is read-only, so we copy the code into a writable /tmp/fn
# and run npm install there. The server then loads the handler from
# /tmp/fn instead of /app/function.
#
# Image-based functions (no inline source, no ConfigMap mount) skip
# this entirely — they are a single container with whatever bundle
# the user shipped.

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
