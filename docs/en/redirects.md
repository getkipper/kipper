# URL Redirects

You can add URL redirect rules to any app with a route. Requests matching the source pattern get a redirect response (301 or 302) pointing to the target URL, without hitting your app.

Common uses: redirecting `/` to `/en/`, sending old URLs to new ones, or forwarding an entire path prefix somewhere else.

## Redirecting a whole domain

Redirect rules rewrite URLs on a hostname the app already serves. To send a different hostname to this app, for example `www.example.com` to `example.com` or an old domain after a rename, use [redirect domains](/en/domains#redirect-domains) on the route instead. A redirect domain answers every request with a 301 to the same path on the route's hostname, so it needs no rules on this page.

## Adding redirects via the console

Open the app's **Settings** tab and scroll to **URL Redirects**. Click **Add** to create a new rule.

Each rule has three fields:

- **Source:** a regex pattern matched against the full request URL
- **Target:** the destination URL or path. Supports regex group references like `$1`
- **301 checkbox:** checked means permanent redirect (301), unchecked means temporary (302)

Click **Save settings** to apply. The redirects take effect within a few seconds.

## Common patterns

Redirect root to a subpath:

| Source | Target | Type |
|--------|--------|------|
| `^https?://[^/]+/$` | `/en/` | 301 |

Redirect an old page to a new one:

| Source | Target | Type |
|--------|--------|------|
| `^https?://[^/]+/blog$` | `/articles` | 301 |

Redirect with path capture (everything under `/old/` goes to `/new/`):

| Source | Target | Type |
|--------|--------|------|
| `^https?://[^/]+/old/(.*)` | `/new/$1` | 301 |

Group references can also be written `${1}`. Use that form whenever the next character in the target is a letter, digit, or underscore, as in `/city-${1}-guide`. Written as `/city-$1-guide` the reference still works because `-` ends the name, but `$1guide` would be read as a group named `1guide` and expand to nothing.

## Adding redirects via kipper.yaml

```yaml
apiVersion: kipper.run/v1alpha1
kind: App
metadata:
  name: docs
spec:
  image: registry.example.com/docs:latest
  port: 80
  route:
    redirects:
      - source: "^https?://[^/]+/$"
        target: "/en/"
        permanent: true
```

## Permanent vs temporary

Use **301 (permanent)** when the old URL will never come back. Browsers and search engines cache 301s aggressively, so the redirect happens instantly on repeat visits.

Use **302 (temporary)** when the redirect might change. Browsers won't cache it, so they check every time. Good for A/B testing, maintenance pages, or redirects you're still experimenting with.

## How it works

Each redirect rule creates a Traefik `redirectRegex` middleware. The middleware intercepts requests before they reach your app. Multiple redirect rules are applied in order. If the first rule matches, the redirect fires and the remaining rules are skipped.

Redirect middlewares are named `{app}-redirect-0`, `{app}-redirect-1`, etc. They're automatically cleaned up when you remove rules.
