# Team Access

Kipper is designed for teams. There are two ways to give someone access to a cluster, and the right one depends on how much they should be able to do. Neither shares SSH keys or server passwords.

**Scoped access, for most people.** Invite a developer or contractor into specific projects with a role that limits what they can do. They log in as themselves and only see the projects you added them to. This is the right choice for team members, and the full model is in [Project Members](/en/project-members). The short version is below.

**Full cluster access, for another admin or your own machines.** Export the cluster credentials and share the file. Whoever imports it can run every `kip` command against the cluster, the same as you. Use this for a co-admin, or to set up the CLI on a second machine of your own. It is not a way to hand out limited access.

## Scoped access for a team member

First invite them. On the **Users** screen click **Invite**, choose the project and their role, and send the link. When they accept and set a password, their Kipper account is created and they land in that project with no cluster-wide powers. From the CLI it is two commands, because an invite carries a cluster-wide role and the project role
is set separately:

```bash
kip user invite --email jordan@acme.com --role viewer
# once they have opened the link and set a password:
kip project members add acme-shop jordan@acme.com deployer
```

The second command needs their account to exist, which happens when they accept, so it is refused
until then. Keep the invite at `--role viewer`. That flag applies across the whole cluster, so `--role deployer`
would let them deploy to every project and the project role would take nothing back. See
[Project Members](/en/project-members) for the longer version.

They then point the `kip` CLI at the cluster and log in as themselves:

```bash
kip auth login
```

This opens a browser to the cluster's login (Dex) and stores a session token, refreshed automatically until the refresh token expires. From then on their commands run with their own identity, and their per-project role decides what they can do: a viewer reads apps, logs, and settings; a deployer deploys and edits; an owner also manages members. See [Project Members](/en/project-members) for the full model.

## Full cluster access

To give another admin complete CLI access, or to set up `kip` on a second machine of your own, export the credentials and import them on the other machine.

### Step 1: Export the cluster (admin)

The admin runs:

```bash
kip cluster export > acme-production.kip
```

This creates a file called `acme-production.kip` containing the cluster connection details and credentials.

### Step 2: Share the file

Send the `.kip` file to your team member however you normally share files: Slack, email, a shared drive. The file is sensitive (it grants cluster access), so use a secure channel when possible.

### Step 3: Import and connect (team member)

The team member installs the `kip` CLI, then imports the file:

```bash
kip cluster add acme-production.kip --set-current
```

The `--set-current` flag makes this the active cluster immediately. Without it, the cluster is saved but not selected.

They can verify it worked:

```bash
kip status
```

```
  Cluster: acme.kipper.run
  Host:    203.0.113.10

  Nodes:
    ✔  ubuntu-server-1    master   Ready    v1.34.5+k3s1

  Components:
    ✔  k3s              1 node(s)
    ✔  Traefik          1/1 replicas available
    ✔  cert-manager     1/1 replicas available
    ✔  Longhorn         1/1 replicas available
    ✔  Dex              1/1 replicas available
    ✔  Console API      1/1 replicas available
    ✔  Console          1/1 replicas available
```

Whoever imported the file now has the same full access to the cluster as you and can run every `kip` command. For access limited to certain projects, use the scoped path above instead.

## Managing multiple clusters

If you manage multiple servers (your own product, a client project, a separate cluster for a different region), each gets its own cluster entry. Import as many as you need and switch between them.

::: tip Clusters vs environments
You do not need a separate cluster for each environment. A single cluster can have test, acc, and prod environments using [project environments](/en/environments). Use `kip app promote` to move code between them. Multiple clusters are for genuinely separate infrastructure: different servers, different customers, different regions.
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

This only removes the local credentials. It does not affect the cluster itself or anyone else's access.

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
and stop. See [Naming one workload](/en/installation#naming-one-workload).

## Quick reference

| Task | Command |
|---|---|
| Export a cluster to share | `kip cluster export > file.kip` |
| Import cluster credentials | `kip cluster add file.kip --set-current` |
| List clusters | `kip cluster list` |
| Switch cluster | `kip cluster use <name>` |
| Remove local cluster config | `kip cluster remove <name>` |
| Tunnel to a service | `kip tunnel <service>` |
| Tunnel with custom port | `kip tunnel <service> --local-port <port>` |
| Open a shell | `kip exec <app>` |
| Run a command in a pod | `kip exec <app> -- <command>` |
| Pick between an app and a service of the same name | `kip exec <name> --kind service` |
| Open a web terminal | Console → App → Connect tab |
