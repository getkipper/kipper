---
title: 'Give your team access to a Kubernetes cluster'
description: 'Invite people, give each a role per project, and let them deploy and debug without handing out the cluster admin certificate.'
---

# Team Access

Give each person a Kipper account, then assign the access they need. Project members can work in selected projects; cluster admins manage users and platform settings.

To use the CLI, everyone follows the same connection setup: import the cluster details and sign in with their own account. The export contains connection settings and the cluster's public CA certificate. **Importing it grants no permissions**; access comes from the signed-in user's roles and Kubernetes bindings.

## Scoped access for a team member

On the **Users** screen, click **Invite**, choose the project and role, and share the invitation link. Accepting the invite creates the account and project membership together.

From the CLI, invite first:

```bash
kip user invite --email jordan@acme.com --role viewer
```

After Jordan accepts the invite and sets a password, add the project membership:

```bash
kip project members add acme-shop jordan@acme.com deployer
```

Use the cluster `viewer` role for this setup and assign working permissions through project membership. See [Project Members](/en/project-members) for the available roles.

## Set up CLI access {#full-cluster-access}

Use these steps for a team member or another machine of your own.

### Step 1: Export the connection details

On a machine already configured for the cluster:

```bash
kip cluster export > acme-production.kip
```

Share the file with the recipient through your team's usual channel. It contains cluster addresses and trust information, so the recipient should obtain it from a trusted administrator.

### Step 2: Import and sign in

After installing the CLI, the recipient runs:

```bash
kip cluster add acme-production.kip --set-current
kip auth login
```

`--set-current` selects the imported cluster. The login command opens a browser for the recipient to sign in as themselves.

### Step 3: Verify access

```bash
kip auth verify
```

Console roles and Kubernetes permissions are separate. Project membership is projected into namespaced Kubernetes bindings; full Kubernetes administration requires a cluster-admin binding. Administrators can use `kip user list` to inspect the reported cluster access. See [Authentication](/en/authentication) for sessions and [CLI reference](/en/cli-reference#kip-auth-verify) for verification.

## Managing multiple clusters

If you manage multiple servers (your own product, a client project, a separate cluster for a different region), each gets its own cluster entry. Import as many as you need and switch between them.

::: tip Clusters vs environments
Use [project environments](/en/environments) for test, acc, and prod within one cluster, and `kip app promote` to promote app images between them. Use separate clusters when you need separate infrastructure, such as different servers, customers, or regions.
:::

### List all clusters

```bash
kip cluster list
```

```
→ my-startup (my-startup.kipper.run)
    Host: 203.0.113.10
    Provider: baremetal

  client-project (client-project.kipper.run)
    Host: 203.0.113.20
    Provider: baremetal
```

### Switch clusters

```bash
kip cluster use client-project
```

```
  ✔  Switched to client-project (client-project.kipper.run)
```

All subsequent `kip` commands operate against the selected cluster.

### Remove a cluster

When you no longer need access to a cluster:

```bash
kip cluster remove client-project
```

This removes the local cluster entry and kubeconfig. The server and other users' access remain in place.

## Connecting to databases

Services like PostgreSQL, MySQL, and Redis run inside the cluster and are not exposed to the internet. To connect with a desktop database client (DBeaver, TablePlus, pgAdmin, or any other tool), use `kip tunnel` to create a secure connection from your machine to the service.

### Open a tunnel

```bash
kip tunnel mydb
```

```
  ✔  Tunnel open: localhost:5432 → mydb (postgres)
  Press Ctrl+C to close
```

The tunnel maps the service's port to the same port on your local machine. PostgreSQL listens on 5432, Redis on 6379, MySQL on 3306, and so on.

Now open your database client and connect to:

- **Host:** localhost
- **Port:** 5432
- **Username:** kipper
- **Password:** (from `kip service info mydb`)
- **Database:** app

### Use a custom local port

If port 5432 is already in use on your machine (perhaps you have a local PostgreSQL running), pick a different port:

```bash
kip tunnel mydb --local-port 15432
```

```
  ✔  Tunnel open: localhost:15432 → mydb (postgres)
  Press Ctrl+C to close
```

Connect your database client to `localhost:15432` instead.

### Tunnel to Redis

```bash
kip tunnel cache
```

```
  ✔  Tunnel open: localhost:6379 → cache (redis)
  Press Ctrl+C to close
```

Use any Redis client (RedisInsight, redis-cli, or your application's Redis library) and point it at `localhost:6379`.

### Tunnel to services in a specific environment

If your services are deployed to a project environment, specify it:

```bash
kip tunnel db --project blog --environment staging
```

## Shell and terminal access

For debugging directly inside containers, see [Web Terminal](/en/web-terminal). You can also use `kip exec` from the CLI:

```bash
kip exec api --project myapp
```

### When a name matches more than one workload

Both `kip exec` and `kip tunnel` need the name to identify exactly one
workload. The same app name across `test` and `prod` is ordinary, since each
environment is its own namespace, so name the one you mean:

```bash
kip exec api --project blog --environment prod
```

If an app and a service share a name inside a single environment, naming the
project is not enough and `--kind` picks between them:

```bash
kip tunnel api --project blog --environment prod --kind service
```

Where the name still matches several workloads, both commands list the matches
and stop. See [Naming one workload](/en/cli-reference#naming-one-workload).

## Quick reference

| Task | Command |
|---|---|
| Export a cluster to share | `kip cluster export > file.kip` |
| Import cluster connection details | `kip cluster add file.kip --set-current` |
| List clusters | `kip cluster list` |
| Switch cluster | `kip cluster use <name>` |
| Remove local cluster config | `kip cluster remove <name>` |
| Tunnel to a service | `kip tunnel <service>` |
| Tunnel with custom port | `kip tunnel <service> --local-port <port>` |
| Open a shell | `kip exec <app>` |
| Run a command in a pod | `kip exec <app> -- <command>` |
| Pick between an app and a service of the same name | `kip exec <name> --kind service` |
| Open a web terminal | Console → App → Connect tab |
