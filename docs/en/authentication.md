# Authentication

Kipper uses browser-based authentication for both the web console and the CLI. The identity provider is [Dex](https://dexidp.io/), deployed automatically during installation.

## CLI authentication

Before using commands that interact with the console API (like `kip app rebuild` or `kip service bind`), authenticate with your cluster:

```bash
kip auth login
```

```
  Logging in to example...

  Opening browser for authentication...
```

Your browser opens the Dex login page. After signing in, you'll see a confirmation page and the terminal shows:

```
  ✔  Authenticated as admin@203-0-113-10.kipper.run
```

### How it works

1. The CLI starts a temporary local HTTP server on `localhost:18741`
2. Your browser opens the Dex authorization page at `dex-{cluster-domain}.kipper.run`
3. You sign in with your Kipper credentials (the same ones you use for the web console)
4. Dex redirects back to the local server with an authorization code
5. The CLI exchanges the code directly with Dex for an ID token and refresh token
6. Tokens are stored in `~/.kip/auth.json` (per cluster)

::: tip
When you set a custom domain with `kip cluster domain`, the Dex URL moves with it (e.g. `dex.kipper.example.com` after moving to `kipper.example.com`). No manual configuration needed.
:::

### Token lifetime

- **ID tokens** expire after 24 hours (Dex's default)
- The CLI refreshes ID tokens automatically using the refresh token, so you stay signed in without logging in again
- **Refresh tokens** don't expire on their own with the default install, so a session lasts until you run `kip auth logout` or an admin revokes your access

Run `kip auth login` again whenever you want a fresh session.

### Check your auth status

```bash
kip auth status
```

```
  Cluster: example
  Email:   admin@203-0-113-10.kipper.run
  Status:  authenticated
```

### Log out

```bash
kip auth logout
```

This removes stored tokens for the current cluster. Other clusters are not affected.

## Console authentication

The web console authenticates through the same Dex instance. When you visit the console URL, you're redirected to the Dex login page. After signing in, a session token is stored in your browser.

## Admin credentials

During `kip install`, an admin account is created automatically. The credentials are displayed once. Save them securely.

### Reset the admin password

If you lose the admin password:

```bash
kip auth reset-password
```

```
  Admin password reset.
  Email:    admin@203-0-113-10.kipper.run
  Password: 5b2bf14ef6525...
```

This generates a new password, updates the Dex configuration, and restarts Dex. The new credentials take effect immediately.
