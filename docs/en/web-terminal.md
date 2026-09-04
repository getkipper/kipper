---
title: 'Open a shell in a Kubernetes pod from your browser'
description: 'A terminal into any running pod from the console or the CLI, with no kubectl on your machine and no SSH to the node.'
---

# Web Terminal

The Connect tab in the app detail panel opens an interactive shell session inside a running pod directly in your browser. No CLI, no SSH keys, no `kubectl` knowledge required.

## Opening a terminal

1. Open the web console and navigate to your project
2. Click on an app to open its detail panel
3. Click the **Connect** tab

A terminal session opens with a welcome banner showing the app name, namespace, pod name, and container image:

```
   _  ___
  | |/ (_)_ __  _ __   ___ _ __
  | ' /| | '_ \| '_ \ / _ \ '__|
  | . \| | |_) | |_) |  __/ |
  |_|\_\_| .__/| .__/ \___|_|
         |_|   |_|

  App:        domain-service
  Namespace:  blog-test
  Pod:        domain-service-59fc699cff-77ngp
  Image:      registry.git.example.com:5005/blog/domain-service:latest

  Type 'exit' to disconnect
```

You are now inside a `/bin/sh` session in the container. Run any command you would normally run in a terminal.

## What you can do

### Check files and configuration

```
/ $ cat /app/config.yaml
/ $ ls -la /app/
```

### Run database queries

```
/ $ psql -U kipper app
app=# SELECT count(*) FROM users;
```

### Run migrations or maintenance tasks

```
/ $ npm run migrate
/ $ python manage.py collectstatic
```

### Debug running processes

```
/ $ ps aux
/ $ env | grep DATABASE
/ $ df -h
```

## Choosing a pod

If your app has multiple replicas, the Connect tab connects to the first running pod by default. Use the pod selector dropdown above the terminal to choose a specific pod before opening the session.

## Terminal features

The terminal supports full TTY interaction:

- Arrow keys and command history
- Tab completion
- Ctrl+C to interrupt running commands
- Window resizing (the terminal adapts when you resize the browser or side panel)
- Copy and paste (standard browser shortcuts)

## Ending a session

Type `exit` or close the side panel. The exec session is terminated automatically when the WebSocket connection closes.

## Authentication

Terminal sessions require a valid JWT. The console handles this automatically, so you do not need to pass tokens manually. Sessions are scoped to the same permissions as the logged-in user.

## CLI alternative

For terminal access from the command line, use `kip exec`:

```bash
# Open an interactive shell
kip exec myapp

# Run a single command
kip exec myapp -- cat /app/config.yaml

# Connect to a database
kip exec mydb -- psql -U kipper app
```

See [Team Access: Shell access](/en/team-access#shell-and-terminal-access) for the full CLI reference.
