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

## Why `kip install` is the exception

An install runs hundreds of commands over SSH and shares one connection between
them, which OpenSSH does through connection multiplexing. Windows OpenSSH has no
multiplexing, and Git Bash's ssh comes from the same family.

Nothing blocks an install from PowerShell. kip notices there is no multiplexing
and opens a connection per command instead, which is why it also leaves the
server's SSH port unrestricted in that case rather than rate-limiting itself off
a host it is halfway through building. What you lose is robustness: sshd drops
unauthenticated connections once ten are in flight, so a server under any
connection pressure can fail the install partway and leave a half-built host.
That failure is what WSL avoids, and it is why WSL is the path this page takes
and the one that gets tested.

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

`--no-login` keeps the install from depending on a browser. Without it, the
install finishes by opening one and waiting for the sign-in to come back to
`localhost:18741` on the machine running kip. Whether a Windows browser reaches a
listener inside WSL depends on your WSL networking, and kip has no Windows branch
in its browser opener anyway, so it prints the URL rather than opening anything.
Skipping the step costs nothing: the cluster is fully installed either way, and
you sign in from Windows in the next step.

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

Run the SSH ones from WSL as well, for the same reason the install goes there. The
install sets the server's firewall to rate-limit SSH at six connections per thirty
seconds per source, which comfortably fits one multiplexed connection. A client
that cannot multiplex opens a connection per command, and the heavier commands go
well past six: `kip cluster ca status` alone makes a series of independent reads
and probes. Lighter ones such as `kip cluster dns repair` stay under it on their
own, though the rule counts every connection from your address in that window, so
a second command or another person behind the same address can still push you
over. Installing with `--no-ssh-rate-limit` drops that rule if you would rather manage
the cluster from PowerShell. It removes Kipper's firewall limit and nothing else;
sshd keeps its own ceiling on connections in flight. An install that was not
multiplexing never gets the rule in the first place, for the same reason.
