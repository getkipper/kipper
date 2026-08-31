---
title: 'Move a Docker Compose stack to Kubernetes'
description: 'Map a compose file onto a Kipper project: which containers become apps, which become managed services, and which have no equivalent at all.'
---

# Moving a Docker Compose stack to Kubernetes

Most self-hosted applications start life as a `docker-compose.yml` on one server. This page maps that file onto a Kipper cluster piece by piece, so you can see what each part becomes before you move anything.

There is no importer. You do the mapping once, by hand, and the result is a set of `kip` commands or a `kipper.yaml` you keep in the repository. That is deliberate: a compose file says how containers run on one machine, and a fair amount of it (bind mounts, host networking, a reverse proxy container) has no equivalent here because the platform already does the job.

## What each part becomes

| In `docker-compose.yml` | In Kipper |
|---|---|
| A service running your own image | An [app](/en/deploying-apps): `kip app deploy --image` |
| A service that builds from a Dockerfile | An app built in the cluster from git: `kip app deploy --git` |
| A service running postgres, mysql, mongodb, redis, rabbitmq, opensearch or minio | A [managed service](/en/services): `kip service add postgres --name db` |
| `ports: "80:3000"` | `--port 3000`. The public side is HTTPS on a hostname, and you never publish a port |
| `environment:` and `env_file:` | [Environment variables](/en/secrets): `kip app env set app KEY=VALUE` or `--from-file` |
| A `DATABASE_URL` you wrote by hand | `kip service bind db app`, which injects the credentials it generated |
| `depends_on:` | Nothing, and your app needs to cope. Start order is not guaranteed, and a container that stays alive while its database is missing is never restarted for you |
| A named volume shared between containers | A [shared volume](/en/shared-storage): `kip volume create` then `kip volume mount` |
| A bind mount of a host directory | Either a shared volume, or object storage. There is no host path to mount |
| Another container reached by its compose name | `kip app link`, which injects the target's internal URL |
| An nginx, Traefik or Caddy container in front | Nothing. The cluster routes and terminates TLS for you |
| `restart: always` | Nothing. Kubernetes restarts a failed container by default |
| `deploy.replicas: 3` | `--replicas 3`, or [autoscaling](/en/resource-management) |
| A cron container, or ofelia | A [scheduled job](/en/jobs): `kip job schedule --cron` |

## A worked example

Take a small stack: a Node app that builds from the repository, Postgres, Redis, and nginx in front of it.

```yaml
services:
  web:
    build: .
    ports: ["80:3000"]
    env_file: .env
    depends_on: [db, cache]
    volumes:
      - uploads:/app/public/uploads
  db:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: hunter2
    volumes:
      - pgdata:/var/lib/postgresql/data
  cache:
    image: redis:7
  nginx:
    image: nginx
    ports: ["443:443"]
    volumes:
      - ./nginx.conf:/etc/nginx/nginx.conf
```

That becomes four commands and no nginx.

```bash
kip project create shop --environments test,prod

kip service add postgres --name db --project shop --environment prod --storage 10Gi
kip service add redis --name cache --project shop --environment prod

kip app deploy --name web --git https://github.com/acme/shop --port 3000 \
  --project shop --environment prod
```

Then connect them. `kip service bind` injects the service's connection details into the app, so no password is written down anywhere:

```bash
kip service bind db web --project shop --environment prod
kip service bind cache web --project shop --environment prod
```

A database binding injects the connection in parts, prefixed `DB_`: `DB_HOST`, `DB_PORT`, `DB_USERNAME`, `DB_PASSWORD` and `DB_NAME`. The credentials are the ones generated when the service was created. Redis arrives as `REDIS_HOST` and `REDIS_PORT`, because it runs without a password. An app that wants one `DATABASE_URL` builds it from those parts, either in its own code or in the manifest below.

A plain bind attaches the app to the service's own database, which is what a single-app compose stack wants. Several apps on one Postgres is the case to think about: pass `--database shop_web` (or `database:` in the manifest) and that app gets a database of its own inside the same service.

The `.env` file carries across as it is:

```bash
kip app env set web --from-file .env --project shop --environment prod --restart
```

Leave the compose-era `POSTGRES_PASSWORD` and `DATABASE_URL` out of that file. The binding generated its own credentials, and a stale copy sitting in the environment is the usual reason an app moves across and then connects to nothing.

The uploads volume, if the app really does need a shared filesystem rather than object storage:

```bash
kip volume create uploads --size 10Gi --project shop --environment prod
kip volume mount uploads web --path /app/public/uploads --project shop --environment prod
```

`pgdata` needs no equivalent. A managed service brings its own persistent volume, sized with `--storage`.

## The same thing as a manifest

The commands are the quick way in. For anything you will change again, put it in a [`kipper.yaml`](/en/gitops) instead and keep it next to the code:

```yaml
project: shop
environment: prod
environments:
  - test
  - prod

apps:
  web:
    git:
      url: https://github.com/acme/shop
      branch: main
    port: 3000
    env:
      DATABASE_URL: postgres://${DB_USERNAME}:${DB_PASSWORD:urlencode}@${DB_HOST}:${DB_PORT}/${DB_NAME}
    serviceBindings:
      - name: db
        prefix: DB_
      - name: cache
    route:
      host: shop.example.com

services:
  db:
    type: postgres
    version: "16"
    storage: 10Gi
  cache:
    type: redis

volumes:
  uploads:
    size: 10Gi
    mounts:
      - app: web
        mountPath: /app/public/uploads
```

`kip diff -f kipper.yaml` shows what would change, and `kip apply -f kipper.yaml` makes it so.

An app declared with a `git` block is created holding a placeholder image, because the manifest describes a repository rather than a build. `kip app rebuild web --project shop --environment prod` runs the first one. `kip app rebuild` takes the default project when you leave the scope off, so pass it here. Set up a [webhook](/en/webhooks) if you want a push to build without you.

## What does not carry across

- **Bind mounts of host paths.** `./nginx.conf:/etc/nginx/nginx.conf` and friends assume a filesystem your containers share with the host. Configuration belongs in the image or in environment variables; files your app writes belong in a volume or in object storage.
- **A reverse proxy container.** Routing, TLS, redirects, rate limiting and the security headers are the platform's job. Keeping your own proxy in front means maintaining certificates it will not renew for you.
- **`network_mode: host` and published ports.** Apps reach each other by service name inside the cluster, and the outside world reaches them by hostname. Nothing needs a port on the host.
- **Reaching another container by its compose name.** Inside one project the DNS name is `service.namespace.svc.cluster.local`. Between projects there is no path inside the cluster until you make one with `kip app link`, which opens exactly the one port it needs. Anything with a public route is still reachable the way the rest of the internet reaches it.
- **Privileged containers and anything mounting the Docker socket.** A build agent that drives the host Docker daemon has no home here. Builds run in the cluster from git.

## After the move

Check the app is serving and its dependencies are wired:

```bash
kip app list --project shop --environment prod
kip service list --project shop --environment prod
kip app env list web --project shop --environment prod
kip app logs web --project shop --environment prod
```

Moving the data itself is a separate job. `kip service import` loads a database dump into a managed service, which is the usual path for a Postgres or MySQL container you are retiring. See [stateful services](/en/services) for the dump and restore commands.
