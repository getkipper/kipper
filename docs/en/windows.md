---
title: 'Install Kubernetes from Windows using WSL'
description: 'Run the install and the server-maintenance commands from WSL, where SSH multiplexes, and everything else from PowerShell with the native kip.exe.'
---

# Installing from Windows

Day-to-day work runs from the native Windows binary. The install, and a handful
of commands that maintain the server itself, run from WSL. This page covers the
whole path from a fresh Windows machine to a working cluster you manage from
PowerShell.

To connect to an existing cluster, ask an administrator for a `kip cluster export` file and follow [Team Access](/en/team-access).

## Why the install runs from WSL

An install sends hundreds of commands to your server over SSH. WSL's OpenSSH
multiplexes, so they all share one connection. Windows OpenSSH does not, and Git
Bash's ssh comes from the same family, so from PowerShell each command opens its
own.

Reusing a connection reduces SSH handshakes and helps installs stay within the server's connection limits. This guide uses WSL for installation and host maintenance, then the native `kip.exe` for daily work over HTTPS.

## 1. Set up WSL

From an elevated PowerShell:

```powershell
wsl --install -d Ubuntu
```

Reboot when it asks, then open Ubuntu from the Start menu and set a username and
password.

::: tip Use Ubuntu for this walkthrough
The commands below assume Ubuntu and its package tools.
:::

If `wsl --install` fails, the usual causes are virtualization disabled in the
UEFI firmware, the Virtual Machine Platform Windows feature being off, or device
management policy on a corporate machine. Check your organisation's WSL policy if you use a managed device.

## 2. Install kip inside WSL

```bash
curl -sL https://getkipper.com/install | sh
kip --version
```

Some networks resolve `getkipper.com` but time out connecting to it. When that
happens, build the CLI from source instead. The [contributing
guide](/en/contributing#building-kip-inside-wsl) covers the two flags a
Windows-hosted checkout needs.

## 3. Put your SSH key on the server

Do this from WSL. PowerShell has no `ssh-copy-id`.

```bash
ssh-keygen -t ed25519 -N "" -f ~/.ssh/id_ed25519
ssh-copy-id root@203.0.113.10
ssh root@203.0.113.10 "echo works"
```

::: tip Servers that force a password change on first login
Many providers ship a root password that must be changed the first time you log
in. `ssh-copy-id` cannot drive that prompt, because it allocates no terminal. Log
in once interactively with `ssh root@<ip>`, change the password, `exit`, then run
`ssh-copy-id`.
:::

## 4. Install the cluster

```bash
kip install --host 203.0.113.10 --ssh-key ~/.ssh/id_ed25519 \
  --admin-email you@example.com --no-login
```

Around ten minutes. It prints the console URL and an admin email and password.
**Copy the password now**, it is shown once. If it goes missing, `kip auth
reset-password` issues a new one.

`--no-login` completes installation without waiting for a browser sign-in in WSL. You will sign in from Windows in the next step.

## 5. Hand the cluster to Windows

Still in WSL, export the cluster and put the file somewhere Windows can see:

```bash
kip cluster export > /mnt/c/Users/<you>/cluster.kip
```

Then from PowerShell, with `kip.exe` on your PATH:

```powershell
kip cluster add C:\Users\<you>\cluster.kip --set-current
kip auth login
kip status
```

`kip auth login` prints a URL. Open it in your browser, sign in with the admin
credentials from step 4, and the session lands back in `kip`. The export carries no
credential of its own, so signing in is what turns it into a cluster you can use.

From here everything works from PowerShell: deploying apps, logs, secrets, scaling,
rollbacks, backups.

## Windows quirks worth knowing

In `kip exec`, a remote shell keeps drawing to the size the window had when the
session opened, because Windows reports a resize as console input rather than as a
signal. Size the window before you connect, or reconnect after resizing.

Host maintenance commands use SSH, including upgrades, domain changes, host repair, hardening, and node management. Run them from WSL to reuse SSH connections.

Routine workload commands use the Kubernetes or console API. `kip status` also attempts SSH host checks; if that connection fails, it reports the unchecked sections and continues with the API results.

The UFW SSH rate limit counts new connections from the same source address, including colleagues behind a shared VPN or office connection.

If you would rather run everything from PowerShell, install with
`--no-ssh-rate-limit` and the rule is left off. That changes Kipper's firewall
and nothing else, so sshd keeps its own ceiling on connections in flight. An
install that ran without multiplexing skips the rule anyway, so a cluster you
installed from PowerShell is already set up for a client that opens a connection
per command.
