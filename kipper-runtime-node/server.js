// Kipper Node.js function runtime.
//
// Two modes, dispatched on the KIPPER_MODE env var:
//
//   http  (default) — start an Express server that exposes the user's
//                     handler at /, /event, and /health. Used by HTTP-
//                     triggered functions and the kipper-poll sidecar.
//
//   batch          — import the handler, invoke it once with a synthetic
//                     event ({type: cron, timestamp: <ISO>}), and exit.
//                     Used by cron-triggered functions running as a
//                     Kubernetes CronJob — no HTTP server, no scale-to-
//                     zero overlap, no stacked timeouts. Exit code is 0
//                     on success, 1 if the handler throws or returns a
//                     truthy `error` field.

const fnPath = process.env.KIPPER_FUNCTION_PATH || '/app/function/index.js'
const mode = process.env.KIPPER_MODE || 'http'

let handler

try {
  handler = require(fnPath)
  if (handler.default) handler = handler.default
  console.log(`Function loaded from ${fnPath}`)
} catch (err) {
  console.error(`Failed to load function from ${fnPath}:`, err.message)
  if (mode === 'batch') {
    process.exit(1)
  }
  handler = async () => ({ error: 'function failed to load' })
}

if (mode === 'batch') {
  runBatch().catch((err) => {
    console.error('Function error:', err)
    process.exit(1)
  })
} else {
  startHTTPServer()
}

async function runBatch() {
  const event = {
    type: process.env.KIPPER_TRIGGER || 'cron',
    timestamp: new Date().toISOString(),
  }
  const result = await handler(event, { mode: 'batch' })

  if (result && typeof result === 'object' && result.error) {
    console.error('Function returned error:', result.error)
    process.exit(1)
  }
  if (result !== undefined) {
    console.log(JSON.stringify(result))
  }
  process.exit(0)
}

function startHTTPServer() {
  const express = require('express')

  const app = express()
  app.use(express.json({ limit: '10mb' }))

  app.get('/health', (req, res) => res.json({ ok: true }))

  app.post('/event', async (req, res) => {
    try {
      const result = await handler(req.body, {
        method: req.method,
        headers: req.headers,
        path: req.path,
      })
      res.json(result || { ok: true })
    } catch (err) {
      console.error('Function error:', err)
      res.status(500).json({ error: err.message })
    }
  })

  app.all('*', async (req, res) => {
    try {
      const result = await handler({
        method: req.method,
        path: req.path,
        headers: req.headers,
        query: req.query,
        body: req.body,
      })

      if (typeof result === 'string') {
        res.send(result)
      } else if (result && result.statusCode) {
        res.status(result.statusCode)
        if (result.headers) {
          Object.entries(result.headers).forEach(([k, v]) => res.set(k, v))
        }
        res.send(result.body || '')
      } else {
        res.json(result || { ok: true })
      }
    } catch (err) {
      console.error('Function error:', err)
      res.status(500).json({ error: err.message })
    }
  })

  const port = process.env.PORT || 8080
  app.listen(port, () => console.log(`Kipper function ready on :${port}`))
}
