---
title: 'Move a Kubernetes cluster to another server'
description: 'Copy projects, apps, databases and volumes from one Kipper cluster to another, with a plan you approve before anything moves.'
---

# Cluster Migration

Kipper can migrate entire projects (apps, services, databases with data, volumes, functions, jobs, and secrets) from one cluster to another. No kubectl, no manual exports.

**Migration copies. Your source cluster keeps running untouched until you decommission it yourself.** Nothing on the source is deleted, scaled, or reconfigured by a migration; the cutover only writes route updates on the target. If anything goes wrong before writes land on the target's databases, point DNS back and you are exactly where you started. Once new writes have landed on the target, rolling back means losing them, so treat the cutover as the moment of commitment.

## When to use migration

- Moving from an old server to a new one
- Upgrading hardware (bigger server, different provider)
- Splitting a cluster (move some projects to a separate server)
- Disaster recovery (restore from a running cluster to a new one)

## How it works

Migration is a console feature with visual progress. The flow:

1. **Generate token** on the new cluster (target)
2. **Paste token** on the old cluster (source), select projects
3. **Review the plan**: what moves, what gets skipped, capacity numbers, conflicts
4. **Start** by confirming with a code from your authenticator app
5. **Watch progress** as data transfers automatically
6. **Verify** apps on temporary URLs before touching DNS
7. **Cutover** custom domains (authenticator code again) and update DNS

### What gets migrated

| Resource | How |
|---|---|
| Project CRs | Created on target, namespaces auto-provisioned |
| Kubernetes Secrets | Transferred directly with their types intact (env vars, registry auth). Two kinds travel differently: see the two rows below |
| Service CRs | Created on target, StatefulSets auto-provisioned. A service's credentials travel inside the same handover, so the target owns them from the moment they land and its engine starts on the password your data was written with |
| Per-binding credentials | Rebuilt on the target from the service's credentials, so they are not sent. They are a projection of the shared credentials with the binding's own database applied, and each workload's controller renders its own |
| Postgres data | Each database dumped with pg_dump and restored into the same database name, then verified by table count. Dumps stream directly between the clusters. On a confirmed overwrite, databases that exist only on the target are dropped, so the result is a replacement rather than a merge |
| MySQL / MongoDB data | Exported via mysqldump (including routines, events, and triggers) / mongodump, imported on target |
| Redis data | A fresh RDB snapshot is taken with a blocking SAVE, streamed over, and loaded as-is on the target |
| RabbitMQ definitions | Exported and imported via rabbitmqctl (definitions only, queued messages stay behind) |
| Volumes | PVC data tarred, transferred, extracted on target |
| Git-built apps | Rebuilt from their git source on the target; the branch head is built, exactly like a fresh deploy |
| Apps from external registries | Pulled by the target directly; their pull secrets migrate with the namespace |
| App CRs | Created on target with temporary routes (route flags like rate limits stay active) |
| Function CRs | Created on target |
| Job CRs | Created on target |
| Custom domain routes | Applied after user verifies apps work |

Before anything moves, the source checks the target has enough free CPU, memory, and disk for the selected projects and refuses the migration with a clear message if the box is too small. The disk check compares the projects' volume claims against the target's storage headroom, so treat it as a sanity check rather than a guarantee. Both clusters must also run the same major Kipper version.

### What is NOT migrated

- System components (Traefik, cert-manager, Longhorn, Dex), which already exist on the target from `kip install`
- User accounts (Dex). The migration shows a reminder with the `kip user import` steps; the target starts with only its bootstrap admin
- TLS certificates, because cert-manager issues new ones on the target
- Postgres globals. Databases move one by one, so roles other than the service user, role passwords and settings, and database-level settings (`ALTER DATABASE ... SET`) stay behind. Kipper-managed services use a single service user and are unaffected
- Service share links. The grants and their signing key stay on the source cluster, and an old link fails closed on the target. Mint fresh links after the cutover
- Build history (git apps are rebuilt fresh on the target)
- Pod logs (ephemeral by nature)

## Security model

Migration moves every secret and database a project has to whatever endpoint the token names, so it is the most attractive operation on the cluster for an attacker with a stolen admin login. Three controls guard it.

**Two-factor authentication with a waiting period.** Starting a migration (and applying a cutover) requires a code from an authenticator app, on top of the admin login. The factor must have been enrolled at least 7 days earlier: an attacker who steals a login and enrolls their own device still has to stay undetected for a week while the enrollment alert sits in every admin's inbox. Enrolling a factor requires a one-time code issued from the cluster host (`kip 2fa bootstrap`), so a stolen console login alone can never create one. See [Two-factor authentication](#two-factor-authentication) below.

**A mandatory plan.** Every migration starts from a plan report, and the server refuses a start that did not come from one. The plan is a preview, never a reservation: the start re-checks everything against live state.

**Loud notifications.** Token generation, migration start, cutover, cancel, and every 2FA change alert all admins by email (when SMTP is configured), Slack, the console bell, and an unmissable line in the console-api log. The alert names the initiating user and the target endpoint, which is the detail a colleague recognises as wrong. Because a compromised admin can edit the console's SMTP and Slack settings (the change itself alerts the previous destination), hosts that want a channel no console login can silence can pin one at install time with the `KIPPER_SECURITY_SMTP_*` or `KIPPER_SECURITY_WEBHOOK` environment variables on the console-api Deployment. The installer ships them commented out.

**What this covers, and what it does not.** The 2FA gate protects against a compromised console identity. It does not protect against a compromised server or kubeconfig: anyone with root on the node or a copy of the cluster-admin kubeconfig bypasses the console entirely and can read every secret and dump every database with kubectl alone. Harden the host (`kip cluster harden`, on by default), and treat the cluster's admin kubeconfig on the server as the crown jewels: know who holds a copy, and never paste it into chat, tickets, or shell history. What `kip cluster export` produces is not that file, and carries no credential. Migration tokens are bearer credentials of the same weight while they are valid.

**Kill switch.** A cluster that will never migrate away can refuse outbound migration entirely: set `KIPPER_DISABLE_OUTBOUND_MIGRATION=1` on the console-api Deployment (commented out in the installer manifest). Only host access can lift it, which is the point.

## Two-factor authentication

Migration start and cutover require a TOTP factor (Google Authenticator, Authy, 1Password, or any RFC 6238 app). Setup:

1. A host operator issues a one-time enrollment code:

```bash
kip 2fa bootstrap admin@example.com

  ✔  Enrollment code for admin@example.com

     K7QT-M3XP-9WLC-R2VD

  Valid for 15 minutes, single-use.
  Enter it in Console → Settings → Two-factor authentication to enroll.
```

2. In **Console → Settings → Two-factor authentication**, enter the code, scan the QR code with your authenticator app, and confirm with the first code the app shows. A wrong code voids the enrollment (nobody gets to guess at a pending factor), so a typo means starting over with a fresh bootstrap code.
3. Save the recovery codes. They are shown exactly once, and each works once to replace the factor if the phone is lost.

A freshly confirmed factor waits 7 days before it can authorise a migration; the plan screen shows the exact date. Hosts can adjust the wait with `KIPPER_MIGRATION_2FA_MIN_AGE_DAYS`, and lowering it below 7 logs a persistent security warning. Replacing a factor (Settings, with a current code or a recovery code) restarts the clock.

Lost the phone and the recovery codes? Host-level recovery:

```bash
kip 2fa remove admin@example.com
kip 2fa bootstrap admin@example.com
```

Then re-enroll. The new factor waits the full period again.

## Step-by-step guide

### 1. Prepare the target cluster

Install Kipper on the new server:

```bash
kip install --host 203.0.113.20
```

The target cluster should be running with no projects deployed (or projects that don't conflict with what you're migrating).

### 2. Generate a migration token

On the **target cluster's** console, go to **Migration** and click **Receive from another cluster**. Copy the generated token.

The token is valid for 24 hours and can only be used once.

### 3. Freeze writes on the source

Data is copied while the source apps keep running. Anything written to a database or volume after its copy has been taken stays behind on the source and never reaches the target. For test projects that can be acceptable. For a production project, stop writes before starting and keep them stopped until the domain cutover:

```bash
kip app scale api --replicas 0 --project shop --environment prod
kip app scale worker --replicas 0 --project shop --environment prod
```

Autoscaled apps need one step first: the HPA keeps them running whatever the replica count says, so `kip app scale` refuses them until autoscaling is off. Disable it, scale down, and re-enable it on the target after the cutover:

```bash
kip app autoscale api --off
kip app scale api --replicas 0 --project shop --environment prod
```

Scaling the source apps to zero clears the "source apps still running" warning on the plan, which changes the plan itself. If you already reviewed a plan with that warning, the console asks you to review a fresh one before starting. That is expected: the warning-free plan is the one you want to confirm.

The migration also lists any still-autoscaled apps as a "Write freeze check" warning in the progress view, so nothing keeps writing unnoticed.

The apps come up on the target with the replica counts they had when their App configs were copied, so scale the source down right before starting the migration and scale the target's apps up from its console if needed. The capacity precheck sizes app demand from the App configs (frozen apps count with at least one replica), so freezing first does not weaken it. The console shows this warning again on the start screen.

### 4. Review the migration plan

On the **source cluster's** console, go to **Migration** and click **Migrate to another cluster**. Paste the token, select which projects to migrate, and click **Review migration plan**.

The plan shows everything before anything moves:

- The consent line: which projects, databases, and secrets go to which cluster at which endpoint
- **Blockers** (red) that stop the start: target unreachable, version mismatch, not enough capacity, unconfirmed overwrites, or a 2FA factor that is missing or still inside its 7-day wait
- **Warnings** (amber): autoscaled apps that keep serving through a freeze, missing notification channels
- Capacity numbers: CPU, memory, and disk the projects need against what the target has free
- **Data that will be skipped**: databases over the 500MB cap stay behind with manual steps. Volumes and service storage always move, chunked and verified, with no size cap
- What will migrate, item by item, with git apps marked "rebuilt from the branch head"
- What never migrates (Dex users, TLS certificates, share links, Postgres globals, build history)

If a project already exists on the target, confirm the overwrite by typing its name on the plan; the plan then refreshes with the overwrite applied as a warning.

### 5. Start the migration

The Start button lives on the plan. Enter the 6-digit code from your authenticator app and click **Start migration**. A plan is startable for 15 minutes; after that, review a fresh one.

Cancelling later takes effect immediately, aborting whatever transfer is mid-stream.

### 6. Monitor progress

The migration view shows:
- Two cluster icons with animated data flow indicators
- A step-by-step list with checkmarks, spinners, and progress counters
- A detail panel showing the current operation

Typical steps:
1. Creating project on target
2. Waiting for namespaces
3. Transferring secrets
4. Creating services and waiting for databases to start
5. Exporting and importing database data
6. Transferring volume data
7. Creating apps (with temporary URLs; git apps start rebuilding)
8. Verifying health

The health check waits up to 10 minutes for the target's deployments to come up. A fresh server pulling large images can need longer; set `KIPPER_MIGRATION_HEALTH_TIMEOUT` (a duration like `20m`) on the source's console-api to extend it.

A run that skipped data (an oversized database) finishes as **completed with skipped items**, and the completion screen repeats every skip. The skipped data lives only on the source until the manual steps are done, so move it before decommissioning anything.

### 7. Verify your apps

After migration completes, apps are live on temporary `*.kipper.run` subdomains. The verification dashboard shows each app with its temporary URL and status, straight from the target cluster.

Git-built apps rebuild on the target during this phase. Until a build finishes, the app serves a "building" page and the dashboard shows the build phase next to it. Use **Refresh** to see builds complete.

Click each URL to verify the app works correctly. Check that pages load, database connections work, and functionality is intact.

### 8. Apply custom domains

When everything looks good, enter a fresh authenticator code and click **Everything looks good, apply custom domains**. The cutover repoints production domains, so it carries the same 2FA requirement as the start. Kipper applies the original route configuration and shows the exact DNS records to change.

The cutover refuses to run while a git rebuild is still going or has failed: the custom domain would serve the "building" page instead of the app. The console lists the unfinished builds and offers **Cut over anyway** for the case where an app can go live later.

Complete verification and cutover within 7 days of starting the migration. After that the target stops accepting the session and the routes have to be applied by hand.

Update your DNS records at your DNS provider to point to the new server's IP address. Ideally do this right after the cutover: certificates for the custom domains are issued once their DNS points at the target. Kipper checks propagation automatically and marks a domain green only when it actually points at the new cluster.

If a route fails to apply, the cutover stops and reports which one. The session stays in the verification phase, so you can fix the cause and run the cutover again.

## Limitations

- **Database size**: Databases up to ~500MB transfer automatically. Larger ones are skipped with manual instructions shown in the progress view. The dump streams between the clusters with a one-hour budget per database, so the cap is about transfer time on a normal uplink rather than memory.
- **Volumes and service storage have no size cap**: Shared volumes move through a chunked transfer that verifies every file and resumes from the last completed chunk after a network or pod failure within the run. MinIO and OpenSearch storage moves the same way as raw bytes; the service pauses on both clusters for the duration of its transfer and restarts automatically. If the console API itself restarts mid-run, the migration fails, both clusters clean up and restart paused services on their own, and a fresh migration retransfers the data.
- **Stop apps before migrating**: The data transfer copies each volume once. Writes that land while a transfer runs are not picked up, so scale the source apps to zero first. The plan screen reminds you when source apps are still running.
- **Redirect domains move at cutover**: An app's route carries one serving hostname plus any [redirect domains](/en/domains#redirect-domains). During the verification phase the target serves neither; both switch over together at cutover, and each redirect domain's DNS record must be repointed just like the main hostname's.
- **Git apps build the branch head**: The rebuild on the target checks out the configured branch fresh. If the branch moved since the last deploy on the source, the target runs the newer code.
- **Active connections**: Apps on the source cluster continue running during migration. There is no automatic traffic cutover. DNS changes propagate gradually. Writes that land on the source after its data was copied stay there, which is why the guide says to freeze writes first.

## Troubleshooting

### Migration token expired

Tokens are valid for 24 hours. Generate a new one on the target cluster.

### Target cluster unreachable

The source cluster connects to the target's console-api over HTTPS. Ensure the target's console URL is accessible from the source server (no firewall blocking outbound HTTPS).

### Database import failed

If the database import fails, the Service CR still exists on the target. Export the data from the source cluster and load it into the target with the built-in transfer commands:

```bash
# Pointed at the source cluster: export
kip service export mydb --file dump.dump --project blog --environment prod

# Pointed at the target cluster: import
kip service import mydb --file dump.dump --project blog --environment prod
```

Both commands stream through the Kubernetes API, so they work from any machine with cluster access. See [Importing and exporting data](/en/services#importing-and-exporting-data) for formats and options. This is also the path for databases too large for the automated transfer.

### Version mismatch

The migration handshake refuses to start when the clusters run different major Kipper versions. Upgrade the older cluster first with `kip upgrade`.

### Target cluster too small

The capacity precheck refuses migrations the target cannot fit, listing the CPU and memory the projects request against what the target has free. A separate refusal covers disk: the projects' volume claims against the target's storage headroom. Free up capacity on the target, delete unused volumes, or use a bigger server.

### console-api restarted mid-migration

Migration sessions survive a console-api restart. A run that was actively transferring cannot resume and shows up as failed with the reason; start it again with a fresh token. A migration waiting in the verification phase stays fully usable, including the cutover.

### Retrying a failed migration

Generate a new token on the target and start the migration again. Resources the first attempt already created on the target are updated in place, database restores replay cleanly, and git apps rebuild. Confirm the project overwrite when asked, since the projects now exist on the target.

### Apps stuck in pending

After migration, apps need time to pull container images and start. Git-built apps additionally rebuild on the target first. Check the verification dashboard for build phases, and the target cluster's console for pod status and logs.
