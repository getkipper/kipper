# Project Members

A project groups related apps, services, and environments. Membership decides who can see a project and what they can do inside it. Someone who is not a member of a project cannot see it at all.

## Roles

There are two levels of access in Kipper.

The **cluster admin** runs the whole cluster. Admins see every project, create new projects, manage users, and change platform settings. The person who runs `kip install` is the first admin.

Everyone else gets access one project at a time, with a role that decides what they can do there:

- **Viewer** reads the project. They can look at apps, logs, and settings, but cannot change anything.
- **Deployer** does the day-to-day work: deploy apps, edit environment variables and secrets, restart, roll back, open a terminal.
- **Owner** is a deployer who also manages the project. Owners add and remove members, change roles, and edit the project itself.

A person can hold a different role in each project. Sam can be an owner of `acme-shop` and a viewer of `acme-marketing` at the same time.

## Managing members in the console

Open the **Projects** screen and click the gear icon on a project card to open its settings panel. The **Members** tab lists everyone with access and their role.

If you are an owner of the project (or a cluster admin), you also get controls to manage it:

- Type the email of an existing user, pick a role, and click **Add**.
- Change someone's role with the dropdown next to their name.
- Remove someone with the trash icon.

The person you add must already have a Kipper account. If they do not, invite them instead (see below). A project keeps at least one owner: removing the last one is refused, unless a cluster admin does it or the CLI is given `--force`. Adding a replacement owner first avoids needing either.

## Managing members with the CLI

The `kip project members` commands do the same thing from the terminal.

List who has access to a project:

```bash
kip project members list acme-shop
```

```
  EMAIL                                    ROLE
  sam@acme.com                             owner
  jordan@acme.com                          deployer
```

Add a member, or change their role by adding them again:

```bash
kip project members add acme-shop jordan@acme.com deployer
```

```
  ✔  jordan@acme.com is now deployer on acme-shop
```

Remove a member:

```bash
kip project members remove acme-shop jordan@acme.com
```

```
  ✔  removed jordan@acme.com from acme-shop
```

## Inviting someone straight into a project

If the person does not have an account yet, an invite creates their account and adds them to a project in one step.

On the **Users** screen, click **Invite**. Enter their email address, choose the project under **Add to project**, pick their access level, and send the link. The address is required: only it can accept the invite, and the account is created under it. When they accept and set a password, their account is created and they land in that project with the role you chose. They get no cluster-wide powers, so they only see the project you invited them to.

Leave **Add to project** set to "No project" to invite someone as a plain cluster user or admin instead.

### From the CLI, in two steps

`kip user invite` sends the invite and `kip project members add` scopes them once they have accepted.
The invite carries a cluster-wide role and nothing else, so the project role is a separate step, and
the account does not exist until the invite is accepted:

```bash
kip user invite --email jordan@acme.com --role viewer
```

```
  Invite link created
  For: jordan@acme.com
  Role: viewer
  Expires: 48h

  https://console.acme.com/invite/9f3c1a...

  Send this link to jordan@acme.com. It can only be used once, and only
  that address can accept it.
```

Send them the link. Accepting it is what creates the account, and the second command is refused
until it exists, so run it once they have opened the link and set a password:

```bash
kip project members add acme-shop jordan@acme.com deployer
```

```
  ✔  jordan@acme.com is now deployer on acme-shop
```

**Use `--role viewer` on the invite.** That flag sets the role across the whole cluster, not the
project. `--role deployer` would let them deploy to every project on the cluster, and the project
role you add afterwards would not take anything away. Viewer plus a project role is what the
console's project invite creates, and it is what you want here: they sign in with no cluster-wide
powers and act only where you added them.

Invite first, and add them once they have accepted. Membership is recorded as an address and
nothing later checks that it belongs to anyone, so both the console and the CLI refuse an address
with no account behind it — otherwise a typo becomes a member who can never sign in, looks correct
in `members list`, and counts as an owner in the rule that keeps a project from being left
ownerless.

If a project has already ended up owned by an address nobody can sign in as, add a real owner and
then remove the bad one. No flag is needed, and it ends with the project owned:

```bash
kip project members add acme-shop sam@acme.com owner
kip project members remove acme-shop typo@acme.com
```

The rule only ever refused one order of doing it. `kip project members remove --force` removes a
last owner outright, leaving the project with none, and a cluster admin can do the same from the
console — use those when the phantom should go before a replacement is chosen.

## Creating projects

Only cluster admins create projects. When an admin creates one, they become its first owner and can then add the rest of the team. This keeps the number of projects on a cluster under control while letting each team run itself once the project exists.

```bash
kip project create acme-shop --environments test,acc,prod
kip project members add acme-shop sam@acme.com owner
```
