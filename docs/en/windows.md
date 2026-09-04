---
title: 'Install Kubernetes from Windows using WSL'
description: 'Run the install and the server-maintenance commands from WSL, where SSH multiplexes, and everything else from PowerShell with the native kip.exe.'
---

# Installing from Windows

Day-to-day work runs from the native Windows binary. The install, and a handful
of commands that maintain the server itself, run from WSL. This page covers the
whole path from a fresh Windows machine to a working cluster you manage from
PowerShell.

If someone on your team has already installed the cluster, you need none of this.
Ask them for a `kip cluster export` file, then read [Team Access](/en/team-access).

## Why the install runs from WSL

An install sends hundreds of commands to your server over SSH. WSL's OpenSSH
multiplexes, so they all share one connection. Windows OpenSSH does not, and Git
Bash's ssh comes from the same family, so from PowerShell each command opens its
own.

The difference shows up on a busy server. sshd stops accepting new
unauthenticated connections once ten are in flight, and a public IP collecting
the usual background scans can sit close to that line, so one of those hundreds
of connections gets refused and the install stops partway through. A single
reused connection never meets the limit.

Both routes install the same cluster. WSL is the one this page takes, and the
one that gets tested.

Deploying, logs, secrets, scaling and the rest run from `kip.exe`, talking to
the Kubernetes API over HTTPS. A short list of commands maintains the server
itself and goes over SSH; they are named at the end of this page.

## 1. Set up WSL

From an elevated PowerShell:

```powershell
wsl --install -d Ubuntu
```

Reboot when it asks, then open Ubuntu from the Start menu and set a username and
password.

::: warning Use Ubuntu, not Alpine
A minimal distro costs more time than it saves here. Alpine ships without `sudo`,
`curl`, `apt` or `ssh-keygen`, all of which the steps below need.
:::

If `wsl --install` fails, the usual causes are virtualization disabled in the
UEFI firmware, the Virtual Machine Platform Windows feature being off, or device
management policy on a corporate machine. The last one is worth checking early,
because no amount of retrying gets past it.

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

`--no-login` keeps the install self-contained. Left off, the install ends by
waiting for a browser sign-in to come back to `localhost:18741` on the machine
running kip, which puts your WSL networking in the path for no gain here. The
cluster is fully installed either way, and you sign in from Windows in the next
step, where the browser is already to hand.

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

These reach the server over SSH: `kip install`, `kip upgrade`, `kip cluster
domain` (except `--repair`, which is local), `kip cluster ca status`, `kip cluster auth sync`, `kip cluster dns
repair`, `kip cluster harden`, `kip cluster uninstall` and `kip node add`.
Everything else talks to the Kubernetes API. `kip status` sits in between: it opens a connection to audit the
server's DNS resolver file, but that check is best-effort, so from PowerShell it
reports that it could not read the file and prints the rest of the status as
normal.

Keep those in WSL too, for the same reason the install goes there. The install
protects the server's SSH port with a rate limit of six connections per thirty
seconds from any one address, which one multiplexed connection sits comfortably
inside. Run the heavier ones from PowerShell and they can cross it on their own:
`kip cluster ca status` makes a series of independent reads and probes. Lighter
ones like `kip cluster dns repair` stay under it, though the limit counts every
connection from your address in that window, so a colleague behind the same
address counts towards yours.

If you would rather run everything from PowerShell, install with
`--no-ssh-rate-limit` and the rule is left off. That changes Kipper's firewall
and nothing else, so sshd keeps its own ceiling on connections in flight. An
install that ran without multiplexing skips the rule anyway, so a cluster you
installed from PowerShell is already set up for a client that opens a connection
per command.
