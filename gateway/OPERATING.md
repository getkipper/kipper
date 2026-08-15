# Operating the gateway

The gateway is the single process every registered cluster is reached through. This is what to watch and
what to do about it.

## Endpoints

| Path        | Auth                       | Use                                                  |
|-------------|----------------------------|------------------------------------------------------|
| `/health`   | none                       | Liveness. Answers `{"status":"ok"}` and nothing else. |
| `/status`   | `Authorization: Bearer …`  | Posture: registration counts and the two exposures.   |

`/health` deliberately carries no posture. Whether proof-before-route is enforcing, and how many
registrations are unpinned or unproven, tells a caller when the gateway is fail-open and which
registrations to aim at.

`/status` needs `KIPPER_STATUS_TOKEN` set on the gateway. With no token set the endpoint answers 404
rather than 401, so an unconfigured gateway does not advertise that it has one. Generate a token the way
you would any other secret, for example `openssl rand -hex 32`, and keep it wherever the deployment keeps
its other secrets.

```bash
curl -sf -H "Authorization: Bearer $KIPPER_STATUS_TOKEN" https://gateway.example.com/status
```

```json
{
  "status": "ok",
  "registrations": 12,
  "unpinned": 0,
  "unpinned_oldest_seconds": 0,
  "unproven": 1,
  "unproven_oldest_seconds": 240,
  "proof_before_route": true
}
```

## What to alert on

Poll `/status` on the interval that suits you; every 60 seconds is plenty for all of these. Nothing here
needs sub-minute resolution, because every condition is a state that persists rather than a spike.

**`proof_before_route` is false — page someone.**
Enforcement is off, so a registration that has never proven possession of the key at the address it
registered still routes traffic there. That is the transition mode, and it should never be the steady
state. Alert on the value being false at all, not on it changing, or a restart that loses the setting
goes unnoticed.

**`unproven` above zero for more than 15 minutes — investigate.**
A cluster acquires its proof on its first heartbeat, so a few minutes is normal after a fresh
registration or a cluster restart. Longer means the cluster is not completing the proof exchange: it
cannot reach the gateway, its hop certificate changed without a re-assertion, or its heartbeat is failing
for its own reasons. Under enforcement the affected cluster is not routing, so this is a live outage for
whoever it serves. `unproven_oldest_seconds` tells you whether it is one stuck registration or a
churning set.

**`unpinned` above zero for more than 15 minutes — investigate.**
The hop to that cluster proxies without an enforced certificate pin. It is the designed fail-open state
before a cluster's first assertion, and it should be brief. A registration sitting unpinned for hours is
either a cluster that never asserted a fingerprint or one whose assertion keeps failing.

**`registrations` dropping sharply — investigate.**
Registrations expire after their inactivity window, and only a token-authenticated renewal resets that
clock. A cliff means a fleet stopped heartbeating, not that labels were released on schedule.

**`/health` unreachable — page someone.** Everything behind the gateway is unreachable with it.

## Logs worth a rule

These are written whether or not anything polls `/status`, and each one names a condition the counters
alone do not distinguish.

- `pin assertion for <sub> pending: asserted SPKI …, observed "…"` — the cluster claims a key the gateway
  does not see at that address. A handful during a rollout is convergence. Repeating for one cluster is
  a misconfigured hop certificate, or someone asserting a key they do not serve.
- `proxying <sub> unpinned (grace)` — the fail-open path is being used. Should stop once the cluster
  asserts.
- `refusing to route <sub>` — enforcement rejected a request, with the reason: never proven, lease
  expired, or the pin moved to a key no proof covers. Each leads somewhere different.
- `registration <sub> moved to <ip>: pin and proof cleared` — a token holder changed address. Expected
  after a cluster migration, and worth a look if you were not migrating anything.
- `no client address on a request` — the reverse proxy in front stopped setting the client header. Rate
  limiting degrades to per-destination budgets while this is true.

## Configuration

| Variable                     | Default        | Meaning                                                     |
|------------------------------|----------------|-------------------------------------------------------------|
| `KIPPER_PROOF_BEFORE_ROUTE`  | on             | Route only registrations that have proven possession. Set to `false` for transition mode. |
| `KIPPER_STATUS_TOKEN`        | unset          | Bearer token for `/status`. Unset disables the endpoint.    |
| `KIPPER_DATA_PLANE_RPM`      | 600            | Proxied requests per minute per client address. `0` disables, for metering at the edge. |
| `KIPPER_CLUSTER_INFLIGHT`    | 128            | Concurrent proxied requests per cluster. `0` disables.      |
| `BASE_DOMAIN`                | `kipper.run`   | The wildcard domain registrations live under.               |
| `DATA_PATH`                  | `/var/lib/kipper-gateway/registry.json` | Where the registry is persisted. The shipped compose file overrides this to `/data/registry.json`, which is where its volume mounts. |
| `PORT`                       | `8080`         | Listen port. The gateway expects a reverse proxy in front.  |

## Restarts

The gateway drains in-flight requests for up to 20 seconds on SIGTERM, then flushes the registry.
`stop_grace_period` in `docker-compose.yml` must stay above that or the runtime kills it mid-drain and
the flush is lost. Proxied WebSocket streams are hijacked connections: they are not drained and end with
the process.

## Updating

The gateway runs a published image, so an update is a pull and a recreate. CI builds
`kipper-gateway` on every merge to `main`, which is what makes a new one available; merging is
therefore the first half of a gateway deploy and this is the second.

```bash
cd /opt/kipper-gateway
docker compose pull gateway
docker compose up -d gateway
```

Naming the service leaves Caddy alone. Caddy holds the TLS certificates and terminates every connection,
so restarting it interrupts live traffic when only the gateway changed.

The compose file in this repository is the development one and builds the gateway from source
(`build: .`). A deployment overrides that with an `image:` line, so `git pull` on the host changes
nothing about which gateway runs. Caddy is the other way round: it builds locally from `./caddy` and
bind-mounts its Caddyfile, so a Caddyfile change needs `docker compose restart caddy` rather than a
pull. Restart it rather than reloading: `caddy reload` reaches Caddy over its admin API, and where that
API is unavailable the command adapts the new config, fails to apply it, and says nothing, so the
gateway keeps running the old one while everything looks fine.

Three things to have right before running it:

- **`KIPPER_STATUS_TOKEN` needs to be in the environment, or in a `.env` beside the compose file.**
  Compose interpolates it at `up` time and an unset variable becomes an empty one, which disables
  `/status` silently. The endpoint answers 404 from then on and monitoring goes quiet with it.
- **The registry survives the update.** It lives in the `gateway-data` volume mounted at `/data`, so
  registrations, tokens and held names outlast the container. That volume is what to back up; the image
  holds nothing you need.
- **Let it stop cleanly.** `up -d` sends SIGTERM and honours `stop_grace_period`, so the drain and the
  final flush both run, as above. `docker kill` loses whatever the registry had not yet written.

Read the first lines it logs. Startup re-applies the current label policy to the persisted registry and
reports up to three things.

It names any registration it dropped: a label the current policy refuses that nothing ever served under.
It names any registration holding a label the policy would refuse which has served, and keeps it. A
reservation added later governs new claims, so a cluster already running under such a name stays up;
retire one by agreement with whoever runs it rather than by restarting the gateway. And it names any
registration whose label spells an address it no longer points at, which also keeps serving: each is
either a cluster that moved servers and kept the name its links were published under, or a name claimed
from another server back when that was allowed, and only an operator can tell those apart.

Then confirm it is up:

```bash
curl -sf https://gateway.example.com/health
curl -sf -H "Authorization: Bearer $KIPPER_STATUS_TOKEN" https://gateway.example.com/status
```
