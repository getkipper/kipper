---
title: 'What Kipper hardens on your server, and what stays yours'
description: 'The firewall rules, host hardening, tenant boundaries and security headers an install applies, and where the responsibility stays yours.'
---

# Security

Kipper configures host hardening, TLS at the ingress, workload network policies, and access controls. This page explains those defaults and the boundaries they protect.

Your team manages application security, dependencies, credentials, and access. See [Your responsibilities](#your-responsibilities) for the operational checklist.

## What Kipper provides out of the box

### TLS at the ingress {#tls-everywhere}

Public app routes use HTTPS. On custom domains, cert-manager obtains Let's Encrypt certificates for Traefik. Shared `*.kipper.run` routes terminate public TLS at the gateway. HTTP requests redirect to HTTPS at the ingress entrypoint. Inside the cluster, new Postgres services created through the CLI or console serve TLS by default with a certificate from the cluster's own service CA, once a full `kip upgrade` or `kip install` has created that CA. They also accept unencrypted connections, so the client chooses whether to use TLS. See [Encrypted Postgres connections](/en/services#postgres-tls). Other service types use the protocol configured by the service.

`X-Forwarded-*` headers are honored only from trusted proxies: the kipper.run gateway (trusted automatically when your cluster uses a kipper.run domain) and any addresses you pass with `--trusted-proxy`. From anyone else they are dropped. On the kipper.run path the gateway drops any client-supplied `X-Forwarded-For` and sets it to the address it measured, so the source IP in your logs is trustworthy. If you put your own load balancer in front and trust it with `--trusted-proxy`, the logged source is only as trustworthy as that balancer: most append to the client-supplied chain rather than replacing it, so treat the leftmost address as a claim and cross-check the `forwarded_for` chain that kipper-authz logs. Source IP is used for logging only, never to decide whether a key is valid.

On a `*.kipper.run` domain, the public TLS terminates at the gateway and the request travels to your cluster over a second HTTPS hop. Let's Encrypt cannot issue for that hop (its challenge would end at the gateway), so your cluster serves a stable Kipper-managed certificate instead, and the gateway verifies every connection against a pinned SHA-256 of that certificate's public key. Your cluster asserts the fingerprint through its daily gateway heartbeat, authenticated by the registration token it received at install time. Once the pin is active, someone sitting on the network path between the gateway and your server can only break connections visibly, never read or alter them, apart from a bounded window: for two minutes after a cluster's very first pin activation the gateway still accepts an as-yet-unseen certificate, so a lagging Traefik replica does not 502 during rollout. That two-minute tolerance is gone once proof-before-route is on, because then the served key must also hold a proof. A freshly installed cluster proxies unverified for the first moments until its console-api asserts the fingerprint, which normally happens seconds after installation completes. Rotating the pinned key is an explicit administrative step: annotate the `kipper-hop-cert` Secret in `kipper-system` with `kipper.run/rotate-key`, and the cluster stages a new key, clears it with the gateway, and swaps it in with both keys accepted during the handover. Traefik also keeps its own copy of the hop certificate, the Secret `kipper-hop-cert` in the `traefik` namespace, and reads it from a file when it starts. That way it serves the pinned key from its first connection, even while it cannot yet reach the Kubernetes API, for example right after a reboot. The copy only ever holds the live key, never one staged for rotation. When the copy changes, Traefik is restarted to load it, because a running Traefik does not reliably pick up changes to this mounted file. Rotate the key only with a console-api that maintains Traefik's copy: an older console-api rotates the key without updating Traefik's copy, so Traefik keeps serving the retired key. See [Downgrading console-api](/en/maintenance#downgrading-console-api) for recovery steps.

A `*.kipper.run` name is meant to route to a cluster only once that cluster has proven it controls the destination. Registration alone is not enough: the gateway issues a fresh challenge, and your cluster signs it with the same hop-certificate private key it serves at its IP. The gateway checks the signature against the public key it observes at that IP, so possession of the private key, not knowledge of the public certificate, is what proves control. Someone cannot point a `*.kipper.run` name at a server they do not control and have the gateway serve it, because that server cannot produce the signature. The proof is a renewable lease refreshed by the heartbeat, so a decommissioned cluster's name stops routing on its own. The lease covers one specific key, the one whose possession was proven, and while proof-before-route is on the gateway opens a hop connection only to that key. Any other certificate is refused, including one the pin set would otherwise tolerate during a rotation or a rollout. When the key changes, the gateway stops sending requests over the connections it had opened to the old one, so a pooled connection cannot outlive the proof that allowed it. Rotating the hop key therefore converges over two heartbeats: the first stages the new key and clears it with the gateway, the second asserts it and proves it, and traffic flows again as soon as that proof lands, about a minute later. This proof-before-route enforcement is enabled at the kipper.run gateway once the fleet has acquired proofs; the gateway operator turns it on, so a cluster does not control it.

### Host hardening

Before installing Kubernetes, `kip install` audits the server for surplus services exposed on public interfaces and disables them. It then installs a host firewall with rules that work correctly with k3s pod networking.

This matters because Kipper installs on whatever Linux server you point it at, and many distro defaults leave services exposed on the public network that you wouldn't want there. A default Ubuntu install ships `nfs-common` (pulled in by Longhorn), which pulls in `rpcbind` listening on `0.0.0.0:111`. Open portmappers are abuse vectors for DDoS reflection and routinely trigger upstream-provider abuse reports. Kipper closes this by default so you don't have to know it exists.

**What gets hardened**

- `rpcbind`. Disabled and the socket is masked so it cannot be re-enabled by a package upgrade. Reversible with `systemctl unmask rpcbind.socket`.
- Cockpit. Kipper stops and disables `cockpit.socket` when installed. To re-enable the web console, run `systemctl enable --now cockpit.socket`.
- SSH password login. Kipper turns off `PasswordAuthentication` and `KbdInteractiveAuthentication` with the drop-in `/etc/ssh/sshd_config.d/00-kipper-key-only.conf`. When it writes that file, it also changes `PermitRootLogin yes` to `prohibit-password` and preserves stricter values. See [Key-only SSH](#key-only-ssh).

**Key-only SSH** {#key-only-ssh}

`kip install` turns off SSH password login on the server. `kip cluster harden` does the same on every recorded node. Both require a successful fresh connection using only a key before changing that node's SSH settings. This confirms that kip can log in with a key alone before changing the configuration. Every other password or keyboard-interactive login on the host stops working, so give those users keys first or pass `--allow-ssh-passwords`. Running `kip install` again, for example to rotate storage credentials, repeats this hardening step. `kip node add` leaves a worker's SSH settings as they are, so run `kip cluster harden` after joining workers to turn off password login on them.

Kipper stops before changing the configuration when `/etc/ssh/sshd_config` does not include `/etc/ssh/sshd_config.d/*.conf` before its first `Match` block, when an additional Include appears inside a `Match` block or an included file, or uses a quoted or escaped path, when a `Match` block could still allow a password, or when `00-kipper-key-only.conf` holds settings Kipper does not manage. Files included at the top level, such as the crypto-policies file Rocky Linux, AlmaLinux and CentOS Stream include, are checked with the same rules. The default configurations of Ubuntu, Debian, Rocky Linux, AlmaLinux and CentOS Stream pass.

Kipper checks the new configuration with `sshd -t` before reloading sshd. It then confirms three things: `sshd -T` reports both settings off and the expected `PermitRootLogin`, a new key-only login works, and a new login without a key is refused without even being offered a password. If applying the change or any subsequent check fails, Kipper immediately attempts to restore the previous configuration and reports any restoration failure. The drop-in preserves key authentication settings and stricter root-login policies. Recovery runs immediately on failure, with no timed rollback. `kip install` warns and continues if SSH hardening fails, while `kip cluster harden` returns an error. Installs run with `--via-server` or `--local` require a separate external login check, so finish SSH hardening with `kip cluster harden` from your workstation.

A host whose configuration already has password login off is left as it is, including its `PermitRootLogin` setting, once a fresh connection without a key confirms that the running SSH server refuses both password and keyboard-interactive authentication for kip's login. Kipper reloads sshd only when it changes the configuration, so configuration edits made since the last reload may leave older rules in effect for other users until you reload sshd.

Verify the result on the host with `sshd -T | grep -Ei 'passwordauthentication|kbdinteractiveauthentication|permitrootlogin'`.

**What the firewall opens**

On Ubuntu and Debian, UFW is installed with a default-deny incoming policy; the RHEL family gets firewalld instead, described below. The following ports are allowed:

| Port | Reason |
|---|---|
| Every SSH port reported by `sshd -T` | Preserve access through all configured SSH ports. |
| 80, 443 | Traefik ingress |
| 6443 | k3s API |

On a fresh install, pods reach the host only on the specific ports k3s needs, the kubelet (`10250`) and node-exporter (`9100`), rather than every host port, so a compromised pod cannot reach SSH or other host services. Pod-to-apiserver traffic arrives on `6443` after the service address is translated, already covered above. UFW's `DEFAULT_FORWARD_POLICY` is set to `ACCEPT` so pod-to-pod and pod-to-service traffic is not blocked.

Before changing UFW rules, Kipper reserves the firewall, saves its configuration and active state, and schedules a rollback on the host. Each command under the firewall lock renews the three-minute deadline. If kip stops making progress, rollback waits for the deadline and any command holding the lock, then restores the saved configuration. It also removes any ownership claim created by this run. If UFW was active, rollback starts it again, checks its chain links, and compares the loaded rules with those captured before the change. For SSH runs, Kipper checks a fresh connection before taking this snapshot. Local runs skip this connection check.

If restoration or verification fails, rollback attempts to disable UFW and allow incoming traffic. `kip status` reports this attempt, but the record does not confirm the current firewall state. Run `kip cluster harden` to enable the server's firewall again.

After starting UFW, the host checks its chain links and established-traffic rule for IPv4 and, where UFW manages it, IPv6. If startup or these checks fail, the same shell attempts to disable UFW to preserve SSH access. Kipper also checks the SSH rule and peer rules. Over SSH, Kipper commits a successful change after a fresh SSH connection succeeds. A `--local` install commits after successful rule configuration and checks. A failed change or connection triggers an immediate restoration attempt. Kipper retains ownership of an inactive UFW firewall it previously configured, so later firewall configuration can turn it back on.

A NodePort or host-network service you add yourself needs its own UFW rule, since the pod-to-host default is now deny.

Prometheus collects node metrics over TLS from kube-rbac-proxy on port `9100`, using its service account token. The proxy forwards authorized requests to node-exporter on `127.0.0.1:8100`. Its authorization check requires permission to get the `services/kube-prometheus-stack-prometheus-node-exporter` subresource in `monitoring`; the chart grants this permission to Prometheus. Prometheus verifies the proxy's certificate using the CA certificate provisioned by cert-manager in `monitoring`, keeping node metrics encrypted between nodes. The proxy also answers an unauthenticated health check on port `8888` on every interface, which returns only `ok`. Kipper's managed firewalls keep that port closed; on a host whose firewall you manage, keep `8888` closed to outside traffic.

The kubelet runs with `protect-kernel-defaults`, so it refuses to start unless the host's kernel tunables already match the values Kubernetes expects. `kip install` sets those (OOM handling, panic-on-oops self-healing, and the kernel keyring limits containers rely on) before k3s starts, rather than letting the kubelet silently overwrite them.

**Existing firewall detected**

Kipper preserves active firewalls you manage unless you authorize their adoption. On Ubuntu and Debian, it reconfigures active UFW when `/etc/kipper/firewall-managed` is present or you grant permission with `--adopt-firewall`. On Rocky Linux, AlmaLinux and CentOS Stream, it reconfigures active firewalld when `/etc/kipper/firewalld-managed` is present or you grant permission with `--adopt-firewall`. For interactive SSH installs, `kip install` and `kip cluster harden` preview eligible takeovers and ask for permission. With `kip install --via-server`, the prompt appears on your computer before a new job starts. For firewalld, permission applies to the previewed public zone, saved file inventory, `firewalld.conf`, saved public zone and configured SSH ports. For UFW, it applies to the saved rules, configured SSH ports, the cluster's nodes, the operator tunnel's rule and the default policies in `/etc/default/ufw`. If these change before takeover, Kipper stops. Rerun the command to review the changes. Without a terminal, use the flag. On other distributions, you manage active firewalld yourself.

Taking over UFW applies Kipper's current rules and removes other saved rules, preserving TCP allow and limit rules for the configured SSH ports. It also sets UFW's default policies to deny incoming traffic and allow outgoing and routed traffic, because pod networking needs forwarding. The preview lists each default policy change, making clear when takeover removes a host's default restriction on outgoing traffic. The takeover also brings a cluster installed before the marker existed up to the current rules, which grant the pod network specific access to TCP ports 10250 and 9100. Pods can also reach ports open to everyone. The takeover runs under the same three-minute rollback as a fresh install, and requires SSH or `--via-server` so Kipper can check a fresh SSH connection from outside the host. Kipper refuses takeover if a port in sshd's configuration has no listening socket or a rule to remove contains quotes or characters it cannot safely pass to `ufw delete`. It also stops if a foreign rule appears after its check. The takeover covers the rules listed by `ufw show added`. Hand edits to `/etc/ufw/before.rules` and `after.rules` stay as they are.

`--firewall=false` skips firewall configuration. To take over an existing Kipper-managed firewall, remove the marker and maintain the rules yourself. Avoid changing firewall policy during an installation.

For a firewall you manage, allow the SSH port and ports 80, 443, and 6443 from the clients that need them.

Cloud-side firewalls (Hetzner Cloud Firewall, AWS Security Groups, etc.) are external to the host and are not detected by Kipper. If you use one, make sure those four ports are reachable on the server.

`kip node add` inherits the server's kip-managed firewall setting by default. If kip leaves the server's firewall management to you, it also leaves the joining node's firewall to you and prints the required ports. Use `--firewall` or `--firewall=false` to override this default. Existing kip-managed firewalls always get updated peer rules, even with `--firewall=false`. A UFW firewall that kip does not manage on the server or another existing node stops the join, even with the ports open. Switch it off first. On the server, run `kip cluster harden --adopt-firewall` if kip should manage it. Hetzner's Cloud Firewall does not filter traffic inside a Cloud Network. Use a host firewall to restrict access from other servers in that network.

**Firewall on the RHEL family**

On Rocky Linux, AlmaLinux and CentOS Stream, Kipper configures firewalld before starting k3s. The public zone allows every SSH port reported by `sshd -T` and TCP ports 80, 443 and 6443, and keeps `dhcpv6-client`. Explicit port rules replace the stock `ssh` service; the `cockpit` service is removed. A `kipper-pods` zone covers the pod and service networks and allows TCP ports 6443, 10250 and 9100 to the host. Forwarding policies support pod internet access with masquerading and load balancer traffic to Traefik.

Kipper schedules a three-minute rollback timer before changing firewall configuration files. After loading the rules, it checks a fresh SSH connection; with `--via-server`, the computer running kip performs that check. If verification fails, Kipper attempts to restore the previous firewall immediately. The timer provides a fallback if the connection is lost before the change is committed. SSH is allowed without rate limiting on these hosts.

Kipper starts a stopped firewalld only when its saved configuration passes checks for stock or Kipper-managed settings. For an active firewall you manage, Kipper checks public ports on the internet-facing interface and pod access to host ports 6443, 10250 and 9100. It accepts a trusted pod zone and reports missing rules or custom rules it cannot evaluate. This check does not verify forwarding between pods and the internet.

Use `--adopt-firewall` to let Kipper replace the public zone and add its pod zone and forwarding policies. Adoption requires saved settings that Kipper can safely replace and matching running rules, so rollback can restore the previous configuration. Run adoption over SSH or with `--via-server`; `--local` cannot verify external SSH access and rejects `--adopt-firewall`. Kipper starts a stopped firewalld only after its saved configuration passes checks for stock or Kipper-managed settings. For a stopped, unclaimed firewall with a saved public zone, the service list must match the expected stock services: SSH, dhcpv6-client and Cockpit. Removing Cockpit counts as a customization; restore the stock settings or use `--firewall=false` to manage the firewall yourself.

Kipper enables k3s's SELinux support and preserves the host's SELinux mode. The k3s installer handles the `k3s-selinux` policy package. Velero's volume backup and restore pods and Promtail need access across pod SELinux labels:

- Velero's `privilegedFsBackup` setting runs volume backup and restore containers in privileged mode. The node agent keeps its default security settings. Privileged containers have all Linux capabilities and access to host devices. They bypass SELinux and seccomp confinement and the usual `/proc` and `/sys` restrictions. Kipper enables this setting on RHEL-family hosts regardless of SELinux mode, so backups remain configured for a later switch to enforcing mode.
- Promtail ships logs to Loki using the unconfined SELinux type `spc_t`. It retains the chart's read-only root filesystem, dropped Linux capabilities and disabled privilege escalation. It also uses the runtime's default seccomp profile on every supported distribution.

SELinux's `dontaudit` rules suppress some denial logs. To troubleshoot a "permission denied" error, run `semodule -DB`, reproduce the problem, and inspect denials with `ausearch -m avc -ts recent`. Then restore the rules with `semodule -B`.

On Rocky Linux, AlmaLinux and CentOS Stream 10, Kipper installs `kernel-modules-extra` for the running kernel if a required module is missing. If that package is unavailable, update the host, reboot and retry. With `--harden`, Kipper disables Cockpit socket activation and stops its service when the socket listens on all interfaces.

**Opting out**

```bash
kip install --host <ip> --harden=false     # leave surplus services running
kip install --host <ip> --firewall=false   # manage the host firewall yourself
```

When opted out, `kip install` still prints any findings so you know what is being left in place. Use these flags only when you manage host security yourself.

**Retro-fitting an existing cluster**

For clusters installed before host hardening was the default, apply the same defaults in place:

```bash
kip cluster harden                    # uses the current cluster
kip cluster harden --cluster storefront  # specific cluster
kip cluster harden --firewall=false   # only disable surplus services
```

The command is idempotent. It hardens every recorded node: the server first, then each worker one at a time, each with the firewall for its role. A worker allows SSH from all sources and cluster ports from other nodes, with no public access to ports 80, 443 or 6443. kip stops at the first node it cannot reach or harden, names it, and starts again from the server on a rerun. It runs the same audit as `kip install`, then disables installed `rpcbind` units and stops Cockpit when its socket listens on all interfaces. It configures UFW, or firewalld on Rocky Linux, AlmaLinux and CentOS Stream, under the same ownership rules as `kip install`, and accepts `--adopt-firewall` too. After loading firewalld, Kipper restarts k3s if it is running to restore its network rules. The restart can briefly interrupt pod networking.

### Operator access list {#operator-access}

By default, SSH and the Kubernetes API (6443) accept connections from any address and rely on key-only SSH and API authentication. An operator access list adds a second layer: new connections to every node's SSH ports and to 6443 are accepted only from the prefixes you list. Ports 80 and 443 stay public. The nodes, the cluster's pods and a host that is joining keep reaching the API, and the server keeps reaching each worker's SSH port.

The list is opt-in. A cluster without one behaves as before. Start it from your workstation:

```bash
kip cluster access allow --here                  # the address your SSH connection comes from
kip cluster access allow 203.0.113.0/24 --note office
kip cluster access show                          # the list, recent changes, and a check of every node
kip cluster access revoke 198.51.100.7/32
kip cluster access apply                         # load the recorded list on every node again
kip cluster access reset --yes                   # remove the list and open SSH and the API
```

`--here` opens a new SSH connection over each address family the server's name resolves to and adds the address the server saw. An IPv4 address becomes a `/32`. An IPv6 address becomes the `/64` around it, because workstations rotate temporary IPv6 addresses inside their `/64`, so `--here` grants your whole subnet. Pass a `/128` to `allow` to admit one IPv6 address, for example on a VPS whose provider shares a `/64` between customers.

Add a stable second prefix, such as an office range or a VPN exit. A single `/32` changes with DHCP, your ISP or a corporate NAT with several outgoing addresses, and `kip status` warns while the list has one entry.

**Who the list affects**

The list restricts every kubeconfig issued for the cluster, not only kip. A CI pipeline that deploys with a kubeconfig needs its outgoing addresses listed. Git webhooks reach the console on 443 and need nothing. Behind a jump host, `--here` records the jump host's address, so add your workstation's prefix with `allow` if kubectl runs from there. Workloads on the cluster's own workers that call the server's public address are refused too, because they leave through the worker's public address. Use the in-cluster API address instead, as everything Kipper runs already does.

**How it is enforced**

Each node gets an nftables table, `inet kipper_access`, whose input chain runs before UFW and firewalld (priority `filter - 10`). It accepts established connections first, so a change never cuts an open session, and it drops new connections to the SSH ports and 6443 from everyone else. UFW and firewalld keep their rules and are never reloaded by these commands, which is why `ufw status` and `firewall-cmd --list-all` still show the SSH port and 6443 as open to everyone. Check the list itself with:

```bash
nft list table inet kipper_access
systemctl status kipper-access
cat /etc/kipper/access/table.nft /etc/kipper/access/operators.nft
```

The record lives in the `kipper-access` ConfigMap in `kipper-system`. It holds the prefixes, who added each one and from which address, and the last 100 changes. Those addresses identify your own staff, so treat the record as personal data your organization controls.

Each change is loaded on every node, workers first and the server last, and checked there before it is kept: a new SSH connection over each address family must arrive from a listed address, and on the server the API must answer too. A failed check restores the previous list on that node at once. If kip stops in the middle of a change, the node restores its previous list about 180 seconds after kip's last step. After restoring its previous list, a node still allows the addresses on that list, so `kip status` and `kip cluster access show` warn that its generation is older than the record. Run `kip cluster access apply` to bring the node up to date. kip refuses a change when the API does not answer from your workstation beforehand, since it could not check it afterwards. It also refuses a change that would not admit the address your nodes see for your workstation, refuses to revoke the last entry unless an [operator device](#operator-devices) is active (`reset` opens the ports instead), and stops before changing anything when a node cannot be reached. Revoking an address refuses its new connections. Sessions it already has stay open until they end.

These checks open several new SSH connections to each node, up to six for a dual-stack `kip node add`. UFW's [SSH rate limit](/en/installation#ssh-rate-limiting) allows six connections in thirty seconds from one source address. A quick retry or a second operator sharing that address can reach the limit, causing the check to fail and the change to be rolled back. Wait thirty seconds and run the command again. `kip cluster harden --no-ssh-rate-limit` turns the limit off on every recorded node whose UFW firewall kip configures. Like the rest of `kip cluster harden`, it stops at the first node it cannot reach. Run the command again to finish checking and configuring the remaining nodes.

`kip node add` carries the list to the new node and adds the new node to every existing node's list. While the node joins, the server temporarily admits the joining host's addresses on 6443. If a join is abandoned, that temporary entry stays on the server until the next `apply`, a restart of `kipper-access` or a reboot, and `show` reports it. `kip node remove` removes the node's addresses from the other nodes' lists. `kip cluster uninstall` removes the table from every host it wipes, so a reused host never boots with a list from a cluster that no longer exists.

**At boot**

`kipper-access.service` loads the table before SSH and k3s start. There are four outcomes:

1. The node has no list: the service is skipped and the node boots as it would without one.
2. The list loads: the node enforces it before any connection is accepted.
3. The operator prefixes cannot be loaded: the node boots closed. It admits the cluster but no operator address, and `kip status` and `show` say so. Add your address from the provider console (below), then run `kip cluster access apply`.
4. nftables cannot load the table at all, for example because the `nftables` package was removed: the service fails and the node falls back to the host firewall's rules behind key-only SSH and API authentication. `kip status`, `kip cluster access show` and `kip node reboot` report it, and `kip cluster access apply` restores the list. `show` also checks the saved table with the installed `nft`, so an upgrade that would break the next boot is reported before that boot.

Every other failure (a failed change, a lost lease, a restart of the service, a crashed kip or an orderly reboot) leaves a node with the last checked list or a narrower one, never a wider list than you asked for. Case 4 and deliberate changes by root on the host, such as deleting the table, are reported rather than prevented.

**nftables.service**

Enabling the distribution's own `nftables.service` conflicts with the list. Its configuration starts with `flush ruleset`, and stopping it runs `nft flush ruleset`, which removes UFW's and firewalld's rules as well as the access table. kip refuses to load a list while `nftables.service` is enabled or running and `show` reports it. Disable it with `systemctl disable --now nftables`. kip loads its table from `kipper-access.service`.

**When your address changes**

1. From another listed address, run `kip cluster access allow --here` and, if you like, `kip cluster access revoke` for the old one.
2. From the provider's console, run this on every node that refuses you, in the order `kip node list` prints them, then `kip cluster access allow --here` from your workstation to make it permanent. The line is the same on every supported distribution and lasts until the next change or reboot. Use `admin6` for an IPv6 address.

   ```bash
   nft add element inet kipper_access admin4 { 198.51.100.9 }
   ```

   `nft delete table inet kipper_access` removes the whole list from that node until the next change or reboot.
3. When the service failed at boot (case 4 above), the node is reachable on the host firewall's rules. `systemctl status kipper-access` names the cause and `kip cluster access apply` restores the list.

### Operator devices {#operator-devices}

A listed address works as long as you work from that network. An operator device lets you connect from different networks: your workstation gets a WireGuard key, every node gets a small WireGuard interface, and kip reaches SSH and the API through that tunnel from wherever you are. Add this workstation as a device with:

```bash
kip cluster access device add laptop               # set up the tunnel and register this workstation
kip cluster access device add laptop --device-only # and then drop its address from the list
kip cluster access device list
kip cluster access device remove laptop
kip cluster access via auto|tunnel|direct          # how this workstation reaches the nodes
```

The tunnel runs inside kip with its own network stack, so it leaves your workstation's interfaces, routes and DNS alone and needs no administrator rights. kip starts it when a command needs it, and it stops after 15 minutes without connections. The nodes listen on UDP 443 by default and hand out addresses from `10.99.0.0/24` (`--range`). Use `--port` when adding the first device to choose any valid UDP port except 51820, which the cluster's own WireGuard network uses. kip refuses a range that overlaps the pod or Service network, a node's address or any route on a node.

The first `device add` installs `wireguard-tools` on every node, creates a key on each node that never leaves it, opens the UDP port in the node's firewall, and loads the interface. When the cluster has no access list yet, it lists your workstation's address first so you keep a way in. The device is recorded as pending until kip has checked that a new SSH connection to every node through the tunnel arrives from the device's address, and that the API answers through it on the server. Only then is it active.

On a node with UFW, kip saves an `allow <port>/udp` rule, even when UFW is inactive. Where kip manages firewalld, it saves the port in its public and peer zones. If firewalld is running, kip also opens the port in those zones without a reload. Traffic on the tunnel interface uses the public zone, which already allows SSH and, on the server, the API.

If you manage a running firewalld yourself, the port must already be open in both the running and saved configuration of the zone assigned to the node's default-route interface. Otherwise kip stops before loading the interface and prints the `firewall-cmd` commands that open it. On such a node, traffic on the tunnel interface uses your default zone unless you bind `kipops` to another zone. That zone must allow SSH and, on the server, TCP 6443. kip checks the tunnel before it marks a device active, so a zone that drops this traffic leaves the device pending.

With an active device the access list may be empty. SSH and the API then take new connections from outside the cluster only through registered devices, and from an unlisted network the tunnel is the only way in. Connections that are already open, loopback and the cluster's own nodes, including one that is joining, are still admitted, as with any access list. `--device-only` empties the list after the device is active, and only when the list holds nothing but this workstation's own address. In a terminal, `device add` asks. A change is still checked before it is kept: kip refuses one that would leave neither the tunnel nor a listed address working for both SSH and the API.

With `via auto`, the default, kip uses the tunnel and falls back to the direct path when the tunnel does not come up within 10 seconds, for example on a network that blocks UDP. `--via` sets the path for one command. The interface on each node accepts SSH, the API, ping and ICMP errors from the device range, which includes a pending device while kip checks it. It drops everything else, forwards nothing and reaches no other host or pod. Those rules are in a separate `kipper_ops` table. Flushing a node's ruleset with `nft flush ruleset`, or starting Debian's `nftables.service` with a configuration that flushes the ruleset, removes both this table and the access table while the interface stays up. `kip cluster access show` reports both, and `kip cluster access apply` or a reboot loads them again.

**Other tools**

```bash
kip cluster access ssh-config >> ~/.ssh/config     # Host blocks for ssh and scp through the tunnel
kip cluster access forward                          # 127.0.0.1:16443 to the API, until Ctrl+C
kip cluster access kubeconfig                       # a <context>-tunnel context for kubectl
```

`ssh-config` prints one `Host <cluster>-<node>` block per node, with the node's own SSH user when it has one. Its `ProxyCommand` calls `kip` through your `PATH`, so print it again after adding or removing a node. `kubeconfig` adds a context whose server is `https://127.0.0.1:16443`, with the same CA, credentials and TLS server name, so kubectl still checks the API's certificate. It preserves an existing `<context>-tunnel` entry unless it matches the generated entry apart from the local port, which can be updated. While `forward` runs, any local user of your workstation can reach the API through that port. The API still asks for credentials.

**A lost or stolen device**

Remove it from another device or a listed address with `kip cluster access device remove <name>`. `device remove` requires every node to be reachable when it starts. It saves the removal in the access record first, then removes the device from each node. A node that becomes unreachable before its update keeps the device until `kip cluster access apply` reaches it. If a node is unreachable, revoke the device on the other nodes from the provider console as described below. Remove a permanently lost node with `kip node remove <node> --force`. When no other way in is left, revoke the device from the provider's console on every node, then allow a recovery address:

```bash
sh /etc/kipper/ops/ops.sh revoke-device laptop     # on every node, from the console
nft add element inet kipper_access admin4 { 198.51.100.9 }
```

From that address, `kip cluster access allow --here --via direct` records it, and `kip cluster access device remove laptop --via direct` removes the device for good. kip refuses every other change, and `kip node add`, while a node has revoked a device the record still lists, because a new node would otherwise admit it again.

When this workstation's device key file is missing or damaged, the next `kip cluster access` command warns, stops using the device and continues over the direct path. Remove the device from a listed address and add it again.

**Reboot, reset and uninstall**

`kipper-ops.service` loads the interface at boot. `kip status` and `kip cluster access show` compare each node's interface, port, address and loaded devices with the record, and its `kipper_ops` table with the one kip loaded. They also check that `kipper-ops.service` is enabled and its files are in place, so that the interface comes back after a reboot. `show`, `kip status` and the `kip cluster access` commands that change the record also update this workstation's list of nodes, so a node added from another workstation becomes reachable through the tunnel. `kip node reboot` also updates the list when it looks up node records to find the requested node. A reinstalled node gets a new WireGuard key, and kip refuses that node until `kip cluster access device repin <node>` reads the new key over a direct connection and records it. Then run `repin` for that node on every other workstation with a device. `kip cluster access reset` opens SSH and the API, then checks that every node answers a direct SSH connection and that the API answers directly. Only then does it remove the interfaces and the record over a new direct session and forget this workstation's device. When a node or the API does not answer directly, the tunnel stays and a rerun finishes the reset. Removing the last device leaves the interfaces and the UDP port in place on every node, ready for the next device. Use `reset` to remove them. `kip cluster uninstall` attempts to disable `kipper-ops.service` immediately on every host it wipes and schedules removal of the interface, its table, its firewall rule and `/etc/kipper/ops/` about ten seconds later. This lets uninstall finish over the tunnel first. If the scheduled removal fails after the service has been disabled, the interface remains until the next reboot.

`kip node add` gives the new node its interface before it joins. kip records the node's operator address, adds the node to this workstation's list, loads its interface and then checks that a new SSH connection through the tunnel reaches it. With an active device, the workstation uses the tunnel by default. In that case, even when the cluster's access list is empty, you can join a node without adding an address to the list. The rest of the join runs through the tunnel. A workstation without a device, or one using `--via direct`, still needs its address on the list. kip initially reaches a host missing from this workstation's device node list directly, even with `--via tunnel`, to prepare its operator interface. The new node's UDP port must be reachable, so open it in the provider's firewall (an AWS security group, for example) before the join. If the tunnel cannot reach the node and the list does not admit this workstation's address, kip stops before the node joins and names the port. kip also stops before the join if the node cannot support devices, for example because its kernel has no WireGuard module or an externally managed firewalld keeps the port closed.

### Security headers

Every response from your applications includes security headers enforced at the ingress level:

| Header | Value | Protection |
|---|---|---|
| `Strict-Transport-Security` | `max-age=31536000; includeSubDomains; preload` | Forces HTTPS for 1 year |
| `X-Frame-Options` | `SAMEORIGIN` | Prevents clickjacking |
| `X-Content-Type-Options` | `nosniff` | Prevents MIME type sniffing |
| `X-XSS-Protection` | `1; mode=block` | Blocks reflected XSS attacks |
| `Content-Security-Policy` | Restricts script, style, image, and connection sources to `self` | Prevents XSS and data injection |
| `Referrer-Policy` | `strict-origin-when-cross-origin` | Controls referrer information leakage |
| `Server` / `X-Powered-By` | Removed | Hides server technology from attackers |

Each app gets its own Traefik middleware with these headers. You do not need to configure them in your application.

### Customizing security settings per app

The defaults work for most applications. Settings can be changed per app from the web console (app detail → Settings tab) or the CLI:

```bash
# Disable security headers for a specific app
kip app deploy --name backend --image myimg --port 3000 --no-security-headers

# Set a custom rate limit (requests per second)
kip app deploy --name public-api --image myimg --port 8080 --rate-limit 500
```

Apps without these flags use the cluster defaults (security headers enabled, 100 req/s rate limit).

### Internal paths

A route publishes everything its app answers under the path prefix, so a repository copied into the image and any endpoint a framework mounts on the application port are published with it. Kipper refuses `/actuator`, `/debug/pprof`, `/internal`, `/.git` and `/.env` at the ingress, and an app names anything further in `route.internalPaths`. The endpoints are not switched off, only unpublished: `kip tunnel <app>` still reaches them, because a tunnel port-forwards to the pod and meets no ingress rule.

```bash
kip platform internal-paths show     # what the cluster refuses, and on which routes
kip platform internal-paths on       # refuse the default list everywhere
kip app update backend --internal-path /admin
kip app update backend --public-path /actuator/prometheus
```

A new cluster installs with the default list refused. A cluster upgraded from an earlier release keeps serving what it served until an operator turns it on, because the change is visible to whatever is calling those paths today.

The refusal is an ingress rule matching the path as written, so it closes the accidental exposure rather than filtering what reaches your app: case, path parameters on a servlet container, and encoded separators are spellings it can miss. An endpoint that must never be public belongs on a port the Service does not publish. [What a path prefix publishes](/en/routing#what-a-path-prefix-publishes) has the detail, including the Spring Boot setting that moves the actuator off the app port entirely.

### CSP allowlist

The default Content Security Policy blocks external resources. If your app loads fonts, stylesheets, scripts, or connects to APIs on other domains, add them to the CSP allowlist.

**From the web console:** Open the app's Settings tab → enter comma-separated domains in the CSP allowlist field → Save.

**Example:** To load Google Fonts, add `fonts.googleapis.com` to the allowlist. The domains are added to `style-src`, `font-src`, `script-src`, and `connect-src` directives.

Functions have the same CSP settings. Open the function detail panel, then the Settings tab.

::: tip
Only add domains you trust. Each allowlisted domain can serve scripts, styles, and fonts to your users.
:::

### Rate limiting

All endpoints are rate-limited to **100 requests per second per IP address** with a burst allowance of 200. This protects against:

- Brute force attacks on login endpoints
- Basic DDoS attacks
- Credential stuffing
- API abuse

The rate limit applies at the ingress level before traffic reaches your application.

### Pod Security Standards

User namespaces enforce the `baseline` Pod Security Standard and warn and audit against `restricted`. Baseline enforcement rejects settings such as privileged containers, host networking, and host-path volumes. The stricter warnings help identify workloads that could further restrict privileges, including running as a non-root user.

System components use their own namespaces and security settings.

### Authentication

The web console is protected by Dex (OAuth2/OIDC identity provider) with bcrypt-hashed passwords. API access requires a valid JWT token.

Service-UI share links are the one deliberate exception: an admin can mint a signed, expiring link that opens a single browseable service UI without a Dex login. Each link is a bearer capability backed by a server-side grant, structurally separate from Dex tokens, and it reaches nothing but that one UI. If a link leaks, revoke it (or revoke every link) and rotate the signing key twice. See [Sharing a UI without a login](/en/service-uis#sharing-a-ui-without-a-login) for the full model and the leak runbook.

### Secrets management

Application secrets are stored as Kubernetes Secrets, encrypted at rest by k3s. They are:

- Never returned in API list responses (only revealed on explicit request)
- Scoped to the project namespace (one project cannot access another's secrets)
- Support previous version tracking and rollback
- Injected into containers via `envFrom` (not visible in deployment YAML)

### Revealing stored credentials

Git credentials and container-registry credentials stored under Settings are masked in both the console UI and the list API. If you lose your copy of a token, you can recover it two ways:

- **In the console:** click the eye icon next to a credential in Settings. You'll be asked to re-enter your password before the value is shown. It stays visible for 30 seconds, then re-masks.
- **From the CLI:** run `kip credentials get <name>`. This reads the secret directly from the cluster, so it requires kubeconfig access to `kipper-system` (effectively cluster-admin).

Only users with the `admin` role can use the console reveal flow for these stored credentials. Both paths log an audit line on success and on failed attempts.

A token set per app (under the app's Git settings) is a separate case. It lives in a secret in the app's own namespace, not under Settings, so it does not appear in the lists above. Reveal it from the app's Git source panel with the same password re-entry, or from the CLI with `kip credentials get --app <app> --project <p> --environment <e>`. The app reveal is open to the `deployer` role, looser than the admin-only Settings reveal, because a deployer already manages the app's git source and can rotate that token. Scope app tokens per repository if you don't want a deployer recovering a credential that reaches other repos.

### Namespace isolation

Each project environment runs in its own Kubernetes namespace. This provides **logical isolation**: each namespace has its own Deployments, Services, Secrets, and ConfigMaps. A project cannot access another project's secrets or environment variables.

### Route names {#route-names}

Kipper checks new app and service UI routes for conflicting names, including names reserved for Kipper components. Choose a different app or service name if the route is refused.

Existing app and service routes that conflict with each other remain published after an upgrade. If their ports match, requests can reach the wrong app or service. Give them distinct names. The upgrade removes app routes managed by Kipper that conflict with an installed Kipper component on the same port. See [Route names](/en/deploying-apps#route-names) and the [upgrade checks](/en/maintenance#route-names).

### Network isolation between namespaces

Kipper installs a NetworkPolicy called `kipper-workload-egress` in every project namespace, before
the namespace is usable. It denies egress by default and then permits three things: DNS, pods in the
same namespace, and the public internet with the cluster's own internal and node ranges excluded.

The practical effect is that a pod in `blog-test` cannot open a connection to a service in
`blog-prod`, or in any other project's namespace, even knowing its DNS name. Test reaching a prod
database is the case this closes, and it does not need separate clusters.

Two limits worth knowing:

- **Ingress is deliberately unrestricted.** Traefik has to reach your pods to serve them, and KEDA
  and the metrics stack have to scrape them. The boundary is on what a workload can call out to,
  not on what can call in.
- **The public-egress rule depends on the cluster knowing its own address ranges.** When those
  cannot be read, the namespace still gets the deny baseline of DNS and same-namespace traffic, and
  public egress is left out rather than opened.

Cross-namespace access that you *want* is arranged explicitly with [app links](/en/routing#linking-apps),
which open a path between two named workloads rather than between whole namespaces.

::: tip Per-project resource quotas
On shared clusters, assign each project a tier: every environment namespace then carries a ResourceQuota capping total CPU and memory, so one busy project can never crowd out the others. Projects without a tier run uncapped, which is fine for a single-team cluster. See [Project quotas](/en/resource-management#project-quotas) for tiers, per-environment overrides, and what happens when a namespace hits its ceiling.
:::

### Project access and roles

Runtime isolation is separate from who is allowed to touch a project. Access is decided by membership: a person only sees and works on projects they belong to, with a per-project role (Viewer, Deployer, or Owner) that sets what they can do. Cluster admins see and manage everything. See [Project Members](/en/project-members) for the full model.

### Namespace ownership {#membership-is-not-the-same-as-owning-the-namespace}

Project membership grants access to namespaces owned by that project. Kipper checks ownership using the namespace’s `kipper.run/project` label.

Two project/environment combinations can resolve to the same namespace name. The first project to claim it keeps ownership; the other reports a conflict and its members receive `403` for that environment. Resolve the name collision before using or copying the environment. Check the Project’s status for the conflicting namespace.

### Platform service account scope

The console API checks project membership before operating with its platform service account. That account can manage Secrets and execute commands in pods across namespaces to support deployment, database tools, and migration. Protect its credentials and administrative access to `kipper-system`.

Direct Kubernetes access uses the operator’s own Kubernetes role bindings. A kubeconfig identifies the connection and authentication method; its permissions depend on the identity it uses. The server’s k3s admin kubeconfig grants cluster-admin access. See [Team Access](/en/team-access).

### Grafana access {#grafana-access}

Grafana requires a Kipper session for its hostname and monitoring access. Cluster admins receive Grafana's Admin role; other users with monitoring access receive its Editor role. Both can see every project's logs and metrics. Service-UI share links cannot grant Grafana access.

The public route removes client-supplied identity and authorization headers before Kipper authenticates the request. Kipper then supplies the verified user and role, and the route removes cookies before forwarding to Grafana. This prevents a Grafana session from replacing the Kipper access check. The route also blocks Grafana's `/login` path, and Grafana checks cross-site requests even without a login cookie.

Grafana trusts these forwarded identity headers, so direct network access must be restricted. Kipper's NetworkPolicy allows the ingress controller and Prometheus pods, selected by namespace and labels, to reach Grafana. The cluster must enforce NetworkPolicies for this protection to work. Kipper applies the policy before submitting the monitoring chart during installation, upgrades, and monitoring enablement. For a custom ingress controller, configure the [ingress controller selector](/en/service-uis#ingress-controller-selector).

Every request checks cached roles and grants. Requests refresh the cache when it is at least 30 seconds old. If the grant list is malformed or the last successful read is more than one minute old, Grafana access is denied for everyone, including admins.

Use a current kip version when removing and re-adding users. Older versions can leave a monitoring grant attached to an email address, restoring that access if the account is added again.

### Automatic backups

The daily backup schedule runs at 3:00 AM with seven-day retention. It covers user resources and persistent volume data, with system namespace exclusions. See [Backups & Restore](/en/backups) for coverage and storage options.

### What leaves your cluster {#what-leaves-your-cluster}

Kipper sends no telemetry of its own, and the `kip` CLI does not check for updates. The components Kipper bundles would report usage or check for new versions by default, so Kipper switches that off for Grafana, Loki, Traefik, Longhorn and MinIO, for mongosh telemetry and snippet index downloads in MongoDB services and the optional AI bundle, and for Qdrant telemetry in the optional RAG bundle.

Normal outbound connections include:

- **The kipper.run gateway heartbeat.** Clusters with a registered `*.kipper.run` name renew it at `https://kipper.run` when the console API starts and daily thereafter. Retries happen sooner after failures or while certificate verification is pending. Requests include the subdomain and server IP, plus the management token and certificate fingerprint when available. The cluster signs gateway challenges to prove it holds the certificate's private key. A cluster that was later moved to a domain of your own keeps renewing its old name only while something still serves under it, and stops a day after the last such host is gone. See [changing the name later](/en/domains#changing-the-name-later). Clusters installed with only a custom domain do not send this heartbeat. See [what the gateway records](/en/domains#what-the-gateway-records).
- **Software downloads.** Installation and upgrades fetch container images, Helm charts and component manifests from their registries and repositories. Nodes also download images as needed. Installation downloads k3s and installs Longhorn's iSCSI and NFS clients from the server's package mirrors: `open-iscsi` and `nfs-common` on Ubuntu and Debian, `iscsi-initiator-utils` and `nfs-utils` on Rocky Linux, AlmaLinux and CentOS Stream. On those distributions, firewall setup installs `firewalld` if needed. Version 10 hosts also get `kernel-modules-extra` if a required module is missing. The k3s installer handles the `k3s-selinux` policy package.
- **Let's Encrypt**, when cert-manager requests or renews a certificate for a custom domain.
- **DNS lookups** through the configured resolvers.

Optional features make additional connections:

- **AI features** send code, logs or diagnostic context to your configured AI provider. See [AI provider settings](/en/configuration#ai-provider-settings).
- **Alerts** go to your configured Slack webhook or SMTP server.
- **Git integrations** contact your Git provider to clone repositories, validate access tokens and report commit build status when configured.
- **Backups** go to your configured S3 bucket. Backups using the in-cluster store stay in the cluster.
- **The AI bundle** downloads the selected models through Ollama.

Your apps and their services may also make outbound connections, depending on their configuration.

For an existing cluster, run `kip upgrade` with system components enabled (omit `--skip-system`) to apply these settings. For each installed AI or RAG bundle, rerun `kip ai install` or `kip ai rag install`, respectively, using your original options.

## Docker image best practices

Kipper works with any Docker image, but for production deployments we recommend following these security practices:

### Run as a non-root user

```dockerfile
FROM node:20-alpine

# Create a non-root user
RUN addgroup -S app && adduser -S app -G app

WORKDIR /app
COPY --chown=app:app . .
RUN npm ci --production

# Switch to non-root user
USER app

EXPOSE 3000
CMD ["node", "server.js"]
```

For Java applications:

```dockerfile
FROM eclipse-temurin:21-jre-alpine

RUN addgroup -S app && adduser -S app -G app

WORKDIR /app
COPY --chown=app:app build/libs/*.jar app.jar

USER app

EXPOSE 8080
CMD ["java", "-jar", "app.jar"]
```

### Use minimal base images

Prefer Alpine-based images (`node:20-alpine`, `eclipse-temurin:21-jre-alpine`) over full Debian images. Smaller images have fewer packages and a smaller attack surface.

### Supply secrets at runtime {#don-t-store-secrets-in-images}

Supply passwords and API keys through Kipper’s secret management:

```bash
kip app secret set my-app DATABASE_URL
kip app secret set my-app API_KEY
```

Kipper injects these values as environment variables at runtime. Keep credentials out of Dockerfiles and files copied into images: anyone who can pull the image may be able to recover them.

### Pin image versions

Use specific version tags instead of `:latest` in production:

```bash
# Development
kip app deploy --name backend --image registry.git.example.com/api:latest --port 8080

# Production
kip app update backend --image registry.git.example.com/api:v1.2.3
```

### Scan images for vulnerabilities

Before deploying to production, scan your images for known CVEs:

```bash
# Using Trivy (free, open source)
trivy image registry.git.example.com/api:v1.2.3
```

Kipper does not run image scanning automatically. Build scanning into your CI pipeline so vulnerable images never reach the cluster.

## Basic authentication

You can password-protect any app with HTTP basic auth. This is useful for staging environments, internal tools, or documentation sites that aren't ready for public access yet.

Basic auth applies one access rule to the whole app. Applications that need user roles, sessions, or account management should implement those features in their own authentication flow.

### Enabling via the console

Open the app's **Settings** tab and scroll to **Basic authentication**. Enter a username and password, then click **Add user**. The app is immediately protected. Visitors see a browser login prompt.

Enabling basic auth before adding users, for example with `basicAuth: true` in a manifest, locks access to the app. Visitors see a login prompt; add a user to allow access. Until then, the App's `BasicAuthReady` condition is `False` with reason `NoUsers`.

You can add multiple users. Each one gets their own credentials.

To remove basic auth entirely, click **Remove all**. The password prompt disappears and the app is publicly accessible again.

### How it works

Kipper creates a Traefik `basicAuth` middleware that checks credentials before requests reach your app. Usernames and bcrypt-hashed passwords are stored in a Kubernetes Secret named `{app}-basic-auth` in the app's namespace.

Your app never sees the authentication. Traefik handles it at the ingress level.

### Limitations

- All users with valid credentials have the same access.
- The browser sends credentials on every request.
- Credentials are sent base64-encoded (not encrypted) in the Authorization header. Always use HTTPS.
- For proper user authentication with sessions, tokens, and roles, use Dex and the built-in auth system.

## Step-up 2FA for destructive operations

Cluster migration, the operation that copies every secret and database to another cluster, requires a TOTP code from an enrolled authenticator app on top of the admin login, and the factor must be at least 7 days old. Enrollment itself needs a one-time code issued from the cluster host with `kip 2fa bootstrap`, so a stolen console login can never enroll its own device. Every migration event and every 2FA change alerts all admins out-of-band.

The gate protects against a compromised console identity, not against a compromised server or kubeconfig. Someone with root on the node or the cluster-admin kubeconfig bypasses the console entirely. Harden the host and guard the kubeconfig accordingly. The full model, including the host-pinned notification channels and the outbound-migration kill switch, is in [Cluster Migration → Security model](/en/migration#security-model).

## Your responsibilities

Regardless of what infrastructure-level protections Kipper provides, your team is responsible for:

- **Secure application code:** parameterized queries, input validation, output encoding, CSRF protection
- **Dependency management:** keeping libraries and base images up to date, monitoring for CVEs
- **Secret hygiene:** never committing credentials to git, rotating secrets regularly, using the hidden prompt (`kip app secret set <app> KEY` without `=VALUE`)
- **Access control:** limiting who can deploy to production, using strong passwords, enrolling 2FA early so the factor is mature when a migration needs it
- **Kubeconfig custody:** `kip cluster export` carries no credential, so the file is safe to send; the person importing it signs in as themselves with `kip auth login`. The cluster's admin kubeconfig on the server is the credential to guard: it holds full cluster admin, so know who has a copy, store it like a root credential, and never paste it into chat, tickets, or shell history
- **Monitoring:** reviewing Grafana dashboards and Loki logs for suspicious activity, and making sure SMTP or Slack is configured so security alerts leave the box

## Verifying your security headers

You can verify that security headers are active on your deployment:

```bash
curl -sI https://your-app--203-0-113-10.kipper.run | grep -i 'strict-transport\|x-frame\|x-content\|x-xss\|content-security\|referrer'
```

Expected output:

```
strict-transport-security: max-age=31536000; includeSubDomains; preload
x-frame-options: SAMEORIGIN
x-content-type-options: nosniff
x-xss-protection: 1; mode=block
content-security-policy: default-src 'self'; ...
referrer-policy: strict-origin-when-cross-origin
```
