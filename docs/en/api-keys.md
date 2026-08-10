# API Keys & Usage Plans

Kipper can gate an app's route behind API keys, the way AWS API Gateway gates an API: you issue keys to your consumers, attach a usage plan (rate limit, burst, and an optional monthly quota), and see per-key usage. Everything runs inside your cluster; there is no external gateway service.

## How it fits together

- A **usage plan** defines limits: requests per second, burst size, and an optional quota per day, week, or month.
- An **API key** belongs to one plan and may be scoped to specific apps. The key travels in the `X-API-Key` request header.
- An app opts in with **Require API key** in its settings. From then on, only requests with a valid key for that app are served.

Plans and keys live per environment, like apps. A key issued in `shop-prod` opens nothing in `shop-test`.

## Gating an app

Open the app in the console, go to **Settings**, and switch on **Require API key**. Requests without a valid key get `401`; requests over their plan's rate or quota get `429`.

The gate is applied a moment after you flip the toggle, while Kipper wires the forwardAuth middleware onto the route. During that short window the Settings panel shows an amber notice that the gate isn't in place yet, so you can tell an engaged gate apart from one that is still being applied. It usually clears within a minute.

The check runs in a small in-cluster service (`kipper-authz`) that Traefik consults on every request to a gated route. It fails closed: if the service is down or cannot prove its view of the keys is current, gated routes answer `503` instead of letting unverified traffic through. Ungated apps are never affected. The key header is stripped before the request reaches your app, so your logs and backends never see key material.

The gate sits at the ingress, so it covers requests arriving through the app's public URL. Another pod that calls your app directly over its internal cluster address skips the gate, the same as any in-cluster traffic (see [Namespace isolation](./security#namespace-isolation)). Treat the key check as your edge control, and use NetworkPolicies if you also need to restrict who can reach the app from inside the cluster.

## Issuing keys

In the **API keys** tab of the project's settings panel (the gear icon on the project card in the Projects screen), create a plan first, then keys under it:

```
Plan:  bronze — 10 rps (burst 20), 100,000/month
Key:   acme partner — kip_ab12cd34_… — bronze
```

The full key is shown exactly once, at creation. Kipper stores only a hash; a lost key cannot be recovered, only replaced. Revoking a key (or switching it to Disabled) normally takes effect within seconds. Each checker replica continuously proves that key, plan, and usage changes still reach it by writing a canary object for each of those three data types and watching each write come back; a replica that cannot prove all three within 90 seconds stops serving and denies, so 90 seconds is the hard ceiling on a revoked key's lifetime and on stale plan or quota data.

Your consumers call the API like this:

```bash
curl -H "X-API-Key: kip_ab12cd34_k3xw..." https://api-shop.example.com/v1/orders
```

## The request contract

Hand this section to whoever integrates against your gated API. Every denial is a small JSON body with a stable `code` your client can match on, plus a human `message` that may change:

```json
{ "code": "rate_limited", "message": "rate limit exceeded for this API key" }
```

| Status | `code` | What it means | `Retry-After` |
|--------|--------|---------------|---------------|
| 401 | `invalid_key` | The key is missing, unknown, disabled, or not scoped to this app | no, waiting won't help |
| 429 | `rate_limited` | Over the plan's requests-per-second | yes, about a second |
| 429 | `quota_exhausted` | Over the plan's day, week, or month quota | yes, seconds until the period resets |
| 503 | `gate_unavailable` | The checker can't verify keys right now, so the route fails closed | yes, about ten seconds |
| 500 | `misconfigured` | The gate itself is misconfigured. This is a platform bug, not your request | no |

Honour `Retry-After` for backoff. A monthly quota returns the seconds until the next UTC month, so stock retry middleware won't hammer the gate for days.

A valid key presented to the wrong app reads as `invalid_key` (401), the same as an unknown key. The gate never tells a caller "this key is real but lacks access", so a key can't be used to probe which apps exist.

Denials carry CORS headers, so a browser client can read the body and `Retry-After` instead of getting an opaque CORS failure. A CORS preflight (an `OPTIONS` request with `Access-Control-Request-Method`) passes the gate without a key, so your app answers its own preflight.

## What your backend receives

On an allowed request, the gate adds two headers identifying the caller before it reaches your app:

- `X-Kipper-Key-Prefix` is the key's stable public handle, for example `ab12cd34`. Use this as the consumer identifier in your own logs and per-tenant logic.
- `X-Kipper-Key-Name` is the display name you gave the key, when it has one.

Any copies of these headers a client sends are stripped before the gate sets its own, so your backend can trust them. The secret half of the key is never forwarded.

## Expiry

A key can carry an optional expiry. Set it when you issue the key, and after that instant the gate rejects it on the same `invalid_key` (401) path as a disabled or unknown key, so nothing about the key leaks to the caller. Leave it unset and the key never expires.

In the console the expiry is a date, and the key stays valid through the end of that day in UTC. The management API takes a full RFC3339 timestamp on the create call and refuses one that is already in the past. A key row shows an amber badge in its last two weeks and a red one once it has expired, so you can spot a handover before it bites.

## Rotating a key

Rotation is a manual handover with no downtime:

1. Issue a new key on the same plan and app scope. Set an expiry on it if you want the next rotation scheduled.
2. Give it to the consumer and have them switch over.
3. Watch the old key's per-key usage. Once its traffic drops to zero, revoke it.

Revoking takes effect within the 90-second freshness ceiling. Until you revoke it, the old key keeps working, so there is no cut-over gap.

## Rate limits and quotas

Rate and burst are enforced per authz replica, so they are best-effort ceilings in the AWS style: with two replicas, a key can briefly exceed its nominal rate. Treat them as protection, not billing-grade accounting.

Quotas count against calendar periods (UTC; weeks start on Monday). Counters are collected in memory and written out in batches, so a quota can over-admit by a few seconds' worth of traffic around the boundary. Usage history is kept for 92 days.

## Per-key usage

Each key's row in the API keys panel shows its allowed, rate-denied, and quota-denied totals over the last 90 days, plus the last day it saw any traffic. The last-used date is what tells you whether an old key is safe to revoke. The rate and quota columns turn "our API is down" tickets into self-service: a consumer hitting a wall can see it is their own limit, not your outage. The numbers come from durable daily rollups, independent of the metrics stack and its retention.

For a specific billing window, the management API takes inclusive `from` and `to` UTC dates:

```
GET /api/v1/projects/shop-prod/api-keys/key-ab12cd34/usage?from=2026-03-01&to=2026-03-31
```

This endpoint uses your console session, not an API key. Days are UTC calendar days, matching how quotas count. History is kept for 92 days: a window that starts before that is pulled forward to the earliest kept day, and the response reports the effective `from` so the clamp is visible. A window that falls entirely before the cutoff is refused rather than returned empty.

For aggregate per-app traffic (all requests, keyed or not), see [Observability](/en/observability).

## Failure behaviour

The gate is fail-closed by design. During an authz outage a gated route returns a clean `503` naming `kipper-authz`, and ungated routes keep serving. The service runs two replicas with a disruption budget, so a total outage is a rare, alertable event rather than a routine one. There is no fail-open switch: a route that asked for key enforcement never silently serves anonymous traffic.
