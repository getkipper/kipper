---
title: 'Install Kubernetes from Windows'
description: 'Install and run a Kipper cluster from PowerShell with the native kip.exe. kip runs the install on the server and follows it from your machine.'
---

# Installing from Windows

You can install and manage a cluster from PowerShell with the native `kip.exe`. This page covers the path from a fresh Windows machine to a working cluster.

To connect to an existing cluster, ask an administrator for a `kip cluster export` file and follow [Team Access](/en/team-access).

## What you need

- Windows 10 version 1809 or later, or Windows 11. Both ship the OpenSSH client; check with `ssh -V`.
- `kip-windows-amd64.exe` from the [releases page](https://github.com/getkipper/kipper/releases/latest), renamed to `kip.exe` and put on your PATH.
- A fresh Ubuntu or Debian server you can reach as root, by password or with a key.

## Install the cluster

```powershell
kip install --host 203.0.113.10 --admin-email you@example.com
```

kip runs the installation as a persistent job on the server and streams its progress to PowerShell. This avoids opening a new SSH connection for every install command. kip downloads the matching release onto the server and verifies its checksum before starting.

When it finishes, kip adds the cluster to your machine as the active one, prints the admin email and password, and opens your browser to sign in. **Save the password now.** kip clears it from the server result after collection. If it goes missing, `kip auth reset-password` issues a new one.

### Your SSH key

kip looks for a key at `%USERPROFILE%\.ssh\id_ed25519`, or the one you name with `--ssh-key`. If there is none, it offers to create one. If the server rejects the key, kip offers to add it through a separate SSH login, which may prompt for the root password.

::: tip Servers that force a password change on first login
If your provider requires a password change on first login, connect with `ssh root@203.0.113.10`, change the password, then `exit` and run `kip install` again.
:::

For a passphrase-protected key, add it to Windows `ssh-agent` before installing. Start the service once from an elevated PowerShell, then add your key:

```powershell
Get-Service ssh-agent | Set-Service -StartupType Automatic
Start-Service ssh-agent
ssh-add $env:USERPROFILE\.ssh\id_ed25519
```

Windows OpenSSH refuses a key file that other users can read, which happens to keys copied from another machine. Restrict the file to your own account in its Security settings, or create a fresh key.

The first connection records the server's host key in `%USERPROFILE%\.ssh\known_hosts`. If a reinstalled server presents a different key, kip stops and names that file. Remove the old line for that address from it and run the command again.

### If the connection drops

Once the server job starts, it continues if your connection drops, your laptop sleeps, or you press Ctrl-C. Run the same `kip install` command to reconnect or collect the completed result. To change install options, first finish and collect the current job, or cancel it.

To stop an install that is running:

```powershell
kip install --host 203.0.113.10 --cancel
```

Stopping leaves a partial installation. Run `kip install` again to retry. When nothing is running, `--cancel` discards any uncollected result so the next install can start.

## After the install

Everything works from PowerShell: deploying apps, logs, secrets, scaling, rollbacks, backups, and the commands that maintain the server, such as `kip upgrade`.

Those maintenance commands open a new SSH connection for each command on Windows. To support them, kip disables SSH rate limiting by default when configuring the firewall during a server job. sshd keeps its own limit on connections in flight. To keep the firewall's rate limit anyway, install with `--no-ssh-rate-limit=false`, and run the maintenance commands from Linux, macOS or WSL.

If an earlier WSL install enabled SSH rate limiting, disable it with:

```powershell
kip cluster harden --no-ssh-rate-limit
```

## What kip leaves on the server

kip stores binaries under `/usr/local/lib/kipper`, named by checksum, with a link at `/usr/local/bin/kip`. Each new install removes the ones no install has used for an hour. Cluster settings live under `/root/.kip`. Job logs, state, and results live under `/root/.kip/jobs` with root-only access.

The job removes its request file after reading it because it may contain backup credentials. If the job never reads the file, the next install removes it. kip clears the admin password from the result after the client acknowledges collection; `--cancel` also removes it when discarding a completed result.

## Installing from WSL instead

If you prefer, install from a Linux shell in WSL exactly as on Linux, then hand the cluster to PowerShell:

```bash
kip install --host 203.0.113.10 --ssh-key ~/.ssh/id_ed25519 --admin-email you@example.com --no-login
kip cluster export > /mnt/c/Users/<you>/cluster.kip
```

```powershell
kip cluster add C:\Users\<you>\cluster.kip --set-current
kip auth login
```

## Windows quirks worth knowing

In `kip exec`, a remote shell keeps drawing to the size the window had when the session opened, because Windows reports a resize as console input rather than as a signal. Size the window before you connect, or reconnect after resizing.

`kip status` also checks the host over SSH; if that connection fails, it reports the unchecked sections and continues with the API results.
