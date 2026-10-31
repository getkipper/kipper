# Certificate Authority

Every Kipper cluster creates its own certificate authority when you install it. You will probably
never have to think about it. This page exists so that if you ever do, you know exactly where you
stand and what to do next.

## What it does

The authority does two jobs at once, which is the only reason it needs a page of its own.

It signs the certificate your cluster serves to the Kipper gateway, and your cluster's API server is
given the same authority as the thing it trusts when it verifies a login. Both sides have to agree. As
long as they do, logins work and nothing needs your attention.

It is valid for 30 years. The certificate it signs is valid for up to 10 years and expires no later than the
authority itself. Kipper reissues the certificate when less than a year remains, so renewal
is automatic and you do not need to schedule rotation.

## Checking it

```bash
kip cluster ca status
```

```
  Certificate authority — demo-cluster

    Authority        kipper-hop-ca (a3f2b81c), expires 27 Jul 2056 (29 years)
    Served cert      kipper-hop, expires 27 Jul 2036 (9 years)
    Login issuer     demo-cluster.kipper.run

    ✔ The anchor covers what signed this cluster's certificate
    ✔ The API server has loaded that anchor
    ✔ The wire confirms the certificate in use

  Everything agrees. Nothing to do.
```

Those three checks are separate, because they can disagree and each one fails for a different reason.

The first reads files. The second asks the running API server what it has actually loaded, which is
not the same thing as what is on disk, since an interrupted change leaves a file nothing has read. The
third opens a real connection and looks at the certificate that comes back, rather than trusting what
is stored in the cluster.

A check that cannot be made says so rather than showing a tick. `?` means the API server did not
answer, and `–` means there was nothing to ask. Neither is a pass.

The command reaches your server over SSH, so it keeps working when logins do not. That makes it the
right first thing to run when something looks wrong.

## If logins stop working

Your cluster is still there. Certificate authentication over SSH is completely separate from the login
path this page describes, so you can always reach the server.

```bash
kip cluster ca status
```

If it reports that the API server has not loaded the anchor on disk:

```bash
kip cluster auth sync
```

That rebuilds the API server's authentication config from the authority already on the server and
waits for it to load. It keeps the same login issuers, so it repairs trust without changing who can
sign in. Before it writes anything it opens a verifying connection to each gateway-fronted issuer, so
it refuses rather than handing the API server an anchor that cannot verify what your cluster serves.

If the status instead reports that the anchor does not cover what your cluster serves, `auth sync`
cannot help. It re-renders from the anchor file and adds nothing to it, so the anchor itself has to be
corrected first. That is the recovery at the end of this page.

## Replacing the authority

You need this if the authority's private key has been exposed. Realistically that means a backup
leaked, someone who has left had access to it, or a server was compromised. If none of those happened,
leave it alone. Replacing an authority is riskier than the thing it prevents.

There is no `kip` command that does this for you, and that is a decision rather than an omission.
The change spans three Kubernetes Secrets and two files on the host, and nothing can change all five as
one transaction. A tool that got half way and stopped would leave your cluster serving a certificate
the API server refuses, which locks every operator out of the login path. So each phase below ends at
a gate you verify yourself before carrying on, and the cluster is safe to sit at any of them
indefinitely.

This is the one operation where Kipper asks you to use `kubectl` directly.

### Does this apply to your cluster?

Look at the "Login issuer" line in `kip cluster ca status`.

If it is a `*.kipper.run` host, this page applies in full.

If every issuer is a custom domain, **the dangerous part does not apply to you.** Your API server
verifies that issuer against the public web trust store, not against this authority, so this authority
plays no part in your logins and narrowing trust to it cannot lock anyone out. The wire check prints
`–` for exactly that reason. You still want to replace a leaked authority, because the gateway hop
uses it, but you can do phases 1 to 3 without treating the gates as blocking: the failure they guard
against is not reachable on your cluster. Keep the certificate's key throughout regardless, which
matters for a different reason explained below.

### What an exposed authority actually gets an attacker

Enough to matter, but not on its own. With the private key they can mint a certificate for your
cluster's gateway host. To use it they also need to get between the API server and the Dex host, at
which point they can serve their own keys and mint tokens the API server accepts as an admin.

Two things blunt it. The gateway pins the hop certificate's public key, so a forged certificate
carrying a new key is rejected on that path. And the API server reaches Dex over a loopback pin on the
node itself, so "get between them" means already being on the node. Treat exposure as urgent, not as
an active breach in progress.

### The safety net

Certificate authentication is untouched by everything on this page. If you break the anchor, OIDC
logins stop working and `kubectl` on the node keeps working, because k3s authenticates its own admin
config with a client certificate at `/etc/rancher/k3s/k3s.yaml`.

So the worst case is "operators cannot log in until someone with SSH fixes it", not "the cluster is
gone". Keep root SSH to the node open in a terminal you are not going to close for the whole
procedure. If you lose it, stop.

### Before you start

Have all of these. If any is missing, get it first.

- Root SSH to the node, in a session you will not close.
- A second terminal on your own machine with `kip` on it, for the three commands that run from there.
- Your cluster's gateway host. Everything below calls it `demo-cluster.kipper.run`.
- An hour when nobody needs to deploy. Nothing here takes workloads down, but you want to be able to
  stop and think.
- Somewhere to put the old material that is not the cluster.

Start on your own machine, and check that logins work now so you can tell later whether you broke
them:

```bash
kip cluster ca status
kip auth verify
```

Do not start if the status reports anything under "Needs attention", or if it reports that a
replacement is already part-way through. Both mean the cluster is in a state this procedure does not
begin from. A half-finished earlier replacement is not an error and will not appear under "Needs
attention". It is reported as a phase, and you should finish it before starting another.

If the status tells you the domain check could not run, confirm it on the node before going any
further. A domain change rewrites the same files this procedure does, and `kip` will refuse a domain
change while a replacement is in flight, but the two must not overlap in either direction:

```bash
kubectl get clusteridentities -o jsonpath='{.items[*].status.transition.phase}'; echo
```

Empty output means none is in flight.

Then set up the shell on the node. Use a directory that persists after you close the terminal, because
you may want to stop between phases and come back tomorrow. The `umask` gives newly created private-key
files permissions that allow only root to read or write them:

```bash
ssh root@203.0.113.10
export HOST=demo-cluster.kipper.run
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
umask 077
mkdir -p /root/ca-replacement && cd /root/ca-replacement
```

Take the backup you will want if anything goes wrong. These are the decoded files the recovery at the
end of this page rebuilds from, so keep them even if you also keep the Secret YAML:

```bash
save() {
  kubectl -n kipper-system get secret "$1" -o jsonpath="{.data.$2}" | base64 -d > "$3"
  if [ ! -s "$3" ]; then echo "BACKUP FAILED: $3 is empty — stop here"; rm -f "$3"; return 1; fi
  echo "  saved $3"
}

save kipper-hop-ca   'tls\.crt' backup-ca.crt   &&
save kipper-hop-ca   'tls\.key' backup-ca.key   &&
save kipper-hop-cert 'tls\.crt' backup-leaf.crt &&
save kipper-hop-cert 'tls\.key' backup-leaf.key &&
cp /etc/rancher/k3s/kipper-hop-ca.crt  backup-anchor.crt &&
cp /etc/rancher/k3s/authn-config.yaml  backup-authn-config.yaml &&
echo "BACKUP COMPLETE"
```

Do not go on without `BACKUP COMPLETE`. A `base64 -d` that receives nothing
still succeeds and leaves an empty file, so an unguarded backup looks like it
worked and is discovered to be empty at the one moment it is needed.

Copy those somewhere off the node. They contain private keys, so put them where the old authority's
key was supposed to be, which is nowhere casual.

## Phase 1: widen trust

The goal is an API server that trusts both the old and the new authority. Nothing is signed by the new
one yet, so this phase cannot break anything.

### 1.1 Extract the current material

```bash
cp backup-ca.crt old-ca.crt
cp backup-leaf.crt leaf.crt
cp backup-leaf.key leaf.key
```

Read the subject alternative name off the existing certificate and keep it exactly, by taking it from
the certificate rather than typing it, because the wildcard has to keep matching your gateway host:

```bash
SAN=$(openssl x509 -in leaf.crt -noout -ext subjectAltName \
  | tail -1 | tr -d ' ')
echo "$SAN"
#   DNS:*.kipper.run
```

### 1.2 Mint the new authority

Same shape as the one the installer creates: ECDSA P-256, a path-length-zero CA, 30 years.

```bash
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out new-ca.key

openssl req -x509 -new -key new-ca.key -sha256 -days 10950 \
  -subj "/CN=kipper-hop-ca" \
  -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -out new-ca.crt

openssl x509 -in new-ca.crt -noout -subject -dates -ext basicConstraints,keyUsage
```

Confirm it says CA:TRUE, path length 0, and an expiry about thirty years out.

Use `genpkey` rather than `ecparam -genkey`, and check the header before you go on:

```bash
head -1 new-ca.key
#   -----BEGIN PRIVATE KEY-----
```

It has to say `BEGIN PRIVATE KEY`. `ecparam -genkey` writes the same key as `BEGIN EC PRIVATE KEY`
instead, which Kipper cannot read, and `openssl verify` will happily accept everything you build with
it. If that key reaches the cluster, `kip cluster ca status` reports the authority's key as not
matching its certificate, and Kipper's attempts to reissue the hop certificate fail every time it
tries, so the cluster keeps serving its current certificate until it needs a fresh one and then drops
off the gateway. Re-encoding the same key repairs it, and the certificate is unaffected because the
public key does not change:

```bash
openssl pkcs8 -topk8 -nocrypt -in new-ca.key -out new-ca.pkcs8.key && mv new-ca.pkcs8.key new-ca.key
```

### 1.3 Store it as the pending authority

The new authority goes into the Secret alongside the old one, which is still the active signer.

Every `base64` capture below is checked before it is used. An empty capture in a merge patch does not
fail: it writes an empty value, or removes the key, and the next thing you read tells you everything
is fine when it is not.

```bash
NEW_CA_B64=$(base64 -w0 < new-ca.crt)
NEW_KEY_B64=$(base64 -w0 < new-ca.key)

if [ -n "$NEW_CA_B64" ] && [ -n "$NEW_KEY_B64" ]; then
  kubectl -n kipper-system patch secret kipper-hop-ca --type merge \
    -p "{\"data\":{\"pending.crt\":\"$NEW_CA_B64\",\"pending.key\":\"$NEW_KEY_B64\"}}"
else
  echo "STOP: the material is empty — do not continue"
fi
```

The patch is inside the `if` on purpose. A check that prints a warning and lets
the next line run is not a check: an empty value is still valid JSON in a merge
patch, so the command succeeds and writes nothing where the certificate should
be, or removes the key entirely.

From here `kip cluster ca status` reports a replacement in flight and tells you which step comes next,
so you can walk away and come back to it.

### 1.4 Widen the anchor

The anchor file holds the authorities the API server will trust, the active signer first.

```bash
cat old-ca.crt new-ca.crt > /etc/rancher/k3s/kipper-hop-ca.crt
```

### 1.5 Load it into the API server

From your own machine, not the node:

```bash
kip cluster auth sync
```

That re-renders the authentication config from the anchor you just wrote and waits for the API server
to report it active. Doing this step by hand means reproducing inline certificate indentation exactly,
under pressure, which is the single most error-prone thing on this page.

### Gate 1

Do not continue until the API server has actually loaded the config. A written file proves nothing.
`kip cluster auth sync` only returns once this is true, so the check is a confirmation rather than a
wait:

```bash
kip cluster ca status
```

```
    ✔ The anchor covers what signed this cluster's certificate
    ✔ The API server has loaded that anchor
    ✔ The wire confirms the certificate in use

  A replacement is part-way through (expanded). This is a safe state to sit in:
  the cluster trusts one authority more than it strictly needs to, which
  affects nothing. Finish it before changing this cluster's domain.
  Resume at step 2.2, promoting the new authority:
  https://docs.getkipper.com/en/certificate-authority
```

Then confirm a login still works:

```bash
kip auth verify
```

If either fails, use the recovery at the end of this page.

## Phase 2: move the signature

The goal is a cluster serving a certificate the new authority signed. The old authority stays trusted
throughout, so this phase is recoverable too.

The order matters and is not the obvious one. The authority is promoted **before** the re-signed
certificate is installed, because Kipper re-signs the served certificate under whichever authority is
currently active. Promote first and every other writer converges in the same direction you are going.
Install the certificate first and Kipper will helpfully sign it back under the old authority, undoing
your work.

### 2.1 Re-sign the existing certificate

Nothing here touches the cluster. The key does not change, only the signature does, and that is what
keeps the gateway's pin valid and your cluster routable.

```bash
openssl x509 -in leaf.crt -noout -pubkey > old.pub

openssl req -new -key leaf.key -subj "/CN=kipper-hop" -out leaf.csr

cat > leaf.ext <<EOF
subjectAltName=$SAN
keyUsage=critical,digitalSignature
extendedKeyUsage=serverAuth
basicConstraints=critical,CA:FALSE
EOF

openssl x509 -req -in leaf.csr -CA new-ca.crt -CAkey new-ca.key -CAcreateserial \
  -days 3650 -sha256 -extfile leaf.ext -out new-leaf.crt
```

Verify three things before this goes anywhere near the cluster:

```bash
# It chains to the new authority.
openssl verify -CAfile new-ca.crt new-leaf.crt

# The public key is unchanged, so the gateway's pin still matches.
openssl x509 -in new-leaf.crt -noout -pubkey > new.pub
diff old.pub new.pub && echo "PIN INTACT" || echo "PIN MOVED — STOP, DO NOT INSTALL THIS"

# The name survived, and serverAuth is present.
openssl x509 -in new-leaf.crt -noout -ext subjectAltName,extendedKeyUsage
```

If the pin moved you generated a new key somewhere. Start this step again from the stored `leaf.key`.

### 2.2 Promote the new authority

The new authority becomes the active signer, and the old one is retained as trusted.

```bash
OLD_CA_B64=$(base64 -w0 < old-ca.crt)
NEW_CA_B64=$(base64 -w0 < new-ca.crt)
NEW_KEY_B64=$(base64 -w0 < new-ca.key)

if [ -n "$OLD_CA_B64" ] && [ -n "$NEW_CA_B64" ] && [ -n "$NEW_KEY_B64" ]; then
  kubectl -n kipper-system patch secret kipper-hop-ca --type merge -p "{\"data\":{
    \"tls.crt\":\"$NEW_CA_B64\",
    \"tls.key\":\"$NEW_KEY_B64\",
    \"previous.crt\":\"$OLD_CA_B64\",
    \"pending.crt\":null,
    \"pending.key\":null
  }}"
else
  echo "STOP: the material is empty — do not continue"
fi
```

Check `previous.crt` actually landed. If that capture had been empty the key would have been removed
instead of set, and the status would read as a finished replacement with the old authority still
trusted:

```bash
kubectl -n kipper-system get secret kipper-hop-ca -o jsonpath='{.data.previous\.crt}' | head -c 20; echo
```

### 2.3 Install the re-signed certificate

```bash
NEW_LEAF_B64=$(base64 -w0 < new-leaf.crt)
NEW_PUB=$(openssl x509 -in new-leaf.crt -noout -pubkey)
RV=$(kubectl -n kipper-system get secret kipper-hop-cert -o jsonpath='{.metadata.resourceVersion}')
LIVE_PUB=$(kubectl -n kipper-system get secret kipper-hop-cert -o jsonpath='{.data.tls\.key}' \
  | base64 -d | openssl pkey -pubout)

if [ -n "$NEW_LEAF_B64" ] && [ -n "$NEW_PUB" ] && [ -n "$RV" ] && [ "$NEW_PUB" = "$LIVE_PUB" ]; then
  kubectl -n kipper-system patch secret kipper-hop-cert --type merge \
    -p "{\"metadata\":{\"resourceVersion\":\"$RV\"},\"data\":{\"tls.crt\":\"$NEW_LEAF_B64\"}}"
else
  echo "STOP: the certificate is empty or does not match the live key. Do not continue."
fi
```

The block first compares the new certificate with the current live key, which may have been rotated
since the certificate was signed in 2.1. The patch includes the Secret's resource version from before
that read, so a concurrent rotation causes a conflict instead of pairing the certificate with a
different key. A conflict can also mean Kipper has already re-signed the certificate under the
authority promoted in 2.2, so run the block again. If it then stops on the key check, the key was
rotated: in a new shell define `save` again from "Before you start"
and read `SAN` as in 1.1, then fetch the current pair with
`save kipper-hop-cert 'tls\.crt' leaf.crt && save kipper-hop-cert 'tls\.key' leaf.key` and redo 2.1 and 2.3.

Only `tls.crt` is written. The private key is never part of any patch on this
page: the gateway pins its public half, so anything that replaces it takes the
cluster off the gateway.

Traefik serves its own copy of the certificate from the `traefik` namespace, so give the copy the
same certificate. The function below checks that the live key matches the new certificate, then
writes both to the copy. This also repairs a copy that fell behind a key rotation. The key travels
through files and pipes to keep it off the command line, and the function removes its key file on
exit. A running Traefik does not reliably pick up changes to this mounted copy, so the function then
records the copy's new version on Traefik's Deployment to trigger a restart and waits for the rollout:

```bash
update_traefik_copy() (
  local cert=$1 version live_pub cert_pub
  umask 077
  trap 'rm -f live.key hop.yaml' EXIT
  if ! kubectl -n kipper-system get secret kipper-hop-cert -o jsonpath='{.data.tls\.key}' \
      | base64 -d > live.key || [ ! -s live.key ] \
    || ! kubectl -n traefik get secret kipper-hop-cert -o jsonpath='{.data.hop\.yaml}' \
      | base64 -d > hop.yaml || [ ! -s hop.yaml ]; then
    echo "STOP: could not read the live key or Traefik's copy. If the copy is missing, run kip upgrade first."; return 1
  fi
  if ! live_pub=$(openssl pkey -in live.key -pubout) || ! cert_pub=$(openssl x509 -in "$cert" -noout -pubkey) \
    || [ -z "$live_pub" ] || [ "$live_pub" != "$cert_pub" ]; then
    echo "STOP: $cert does not match the live key. Traefik's copy is unchanged."; return 1
  fi
  if ! kubectl -n traefik create secret generic kipper-hop-cert --from-file=tls.crt="$cert" \
      --from-file=tls.key=live.key --from-file=hop.yaml=hop.yaml --dry-run=client -o yaml \
      | kubectl apply -f - > /dev/null 2>&1; then
    echo "STOP: writing Traefik's copy failed. Its output is withheld because it can contain the private key."; return 1
  fi
  if ! version=$(kubectl -n traefik get secret kipper-hop-cert -o jsonpath='{.metadata.resourceVersion}') \
    || [ -z "$version" ] \
    || ! kubectl -n traefik patch deployment traefik --type merge \
      -p "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"kipper.run/hop-copy-version\":\"$version\"}}}}}"; then
    echo "STOP: Traefik's copy is updated, but restarting Traefik failed. Check the error, then run it again."; return 1
  fi
  if ! kubectl -n traefik rollout status deployment/traefik --timeout=300s; then
    echo "STOP: Traefik was restarted, but its rollout has not finished. Check the Traefik pods, then run it again."; return 1
  fi
)
update_traefik_copy new-leaf.crt
```

If it prints a `STOP` message, fix the cause and run `update_traefik_copy new-leaf.crt` again. Each
step is safe to repeat.

### Gate 2

This gate verifies the certificate Traefik actually serves. Updating the Secrets alone cannot
confirm that, so check the live connection.

```bash
kip cluster ca status
```

```
    ✔ The anchor covers what signed this cluster's certificate
    ✔ The API server has loaded that anchor
    ✔ The wire confirms the certificate in use
```

The third line is the gate. It opens a real connection on the node and checks the certificate that
comes back against the authority the cluster now says is active.

If it shows ✗, read the `Resume at` line printed underneath it. Under this ordering the usual reason
is that step 2.3 has not landed, so the authority moved and the certificate has not followed, and the
status says so directly by resuming you at 2.3. If it resumes you at gate 2 instead, then 2.3 did land
and the wire has not caught up. Check that the Traefik rollout at the end of 2.3 finished, then check
again before investigating.

**Do not go to phase 3 until that line shows ✔.** If it shows `–`, your cluster has no
gateway-fronted issuer and this gate cannot run at all. See "Does this apply to your cluster?" above
before continuing. Confirm a login as well:

```bash
kip auth verify
```

## Phase 3: narrow trust

The old authority stops being trusted and is destroyed. This is the only irreversible part, and it is
safe precisely because gate 2 proved the wire had already moved.

### 3.1 Narrow the anchor

On the node:

```bash
cp new-ca.crt /etc/rancher/k3s/kipper-hop-ca.crt
```

Then from your own machine:

```bash
kip cluster auth sync
```

This one is itself a gate. Before writing the config it opens a verifying connection to your issuer
using only the narrowed anchor, so if the wire were somehow still on the old authority it refuses
rather than handing the API server something that cannot verify your cluster.

### Gate 3

```bash
kip cluster ca status
kip auth verify
```

Both must be clean before you go any further.

### 3.2 Destroy the old authority

Only now, and only if gate 3 passed.

```bash
kubectl -n kipper-system patch secret kipper-hop-ca --type merge -p '{"data":{"previous.crt":null}}'
```

### 3.3 Confirm

```bash
kip cluster ca status
```

```
  Certificate authority — demo-cluster

    Authority        kipper-hop-ca (7d41e0b9), expires 27 Jul 2056 (29 years)
    Served cert      kipper-hop, expires 27 Jul 2036 (9 years)
    Login issuer     demo-cluster.kipper.run

    ✔ The anchor covers what signed this cluster's certificate
    ✔ The API server has loaded that anchor
    ✔ The wire confirms the certificate in use

  Everything agrees. Nothing to do.
```

The fingerprint in brackets is the new authority's, and it should be one you have not seen before on
this page. If the status instead reports that the API server is anchored on an authority this cluster
does not hold, step 3.1 did not take: the old authority is still trusted even though you deleted your
copy of it. Put the narrowed anchor back and sync again.

Log in from a machine that has never talked to this cluster, so you know you are not reading a cached
session.

Then remove your working copies and the backups holding the old private key, and rotate your backup
storage credentials if a leaked backup is why you are here. Leaving the old key in a bucket someone
else can read defeats the whole exercise.

```bash
rm -rf /root/ca-replacement
```

## If you stop halfway

Every gate is a safe place to stand. A cluster left with two trusted authorities runs correctly and
indefinitely: it trusts one authority more than it strictly needs to, which is untidy rather than
broken. There is never a reason to rush the next step. The one thing to avoid is changing the
cluster's domain before you finish, and `kip` refuses that for you.

Where you got to is written on the cluster, so ask it:

```bash
kip cluster ca status
```

| What the status reports | Where you stopped | What to do |
| --- | --- | --- |
| part-way through (staged) | after 1.3, before the anchor was widened | resume at 1.4 |
| part-way through (expanded), the API server has **not** loaded the anchor | after 1.4, before 1.5 | run 1.5 first, because going on from here is what breaks logins |
| part-way through (expanded), the API server **has** loaded it | after gate 1 | resume at 2.2 (2.1 makes no cluster change, so redo it freely) |
| part-way through (promoted), resume at 2.3 | after 2.2 | resume at 2.3 |
| part-way through (promoted), resume at gate 2 | after 2.3 | confirm gate 2, then 3.1 |
| part-way through (narrowed) | after 3.1 | confirm gate 3, then 3.2 |

### Abandoning a replacement

Up to the moment the authority is promoted at 2.2, nothing signs under the new authority and you can
walk away from it. Remove both halves in one patch, because removing one leaves half an authority behind,
which the status reports as material needing attention:

```bash
kubectl -n kipper-system patch secret kipper-hop-ca --type merge \
  -p '{"data":{"pending.crt":null,"pending.key":null}}'
```

If you widened the anchor at 1.4, narrow it back and apply that too:

```bash
cp old-ca.crt /etc/rancher/k3s/kipper-hop-ca.crt   # on the node
kip cluster auth sync                               # from your own machine
```

After 2.2 there is no abandoning: the new authority is signing. Carry on to the end, or use the
recovery at the bottom of this page.

The two `expanded` rows are the same phase and are told apart by the second tick in the status. They
matter separately: the anchor holds both authorities but the API server is still running the config it
had, so moving the signature before applying it hands the cluster a certificate the API server has
never been told to trust.

The status names the step for you in each case, so you do not have to work out which row you are on.

## If it goes wrong

Recovery restores the saved authority and signs a new certificate for the cluster's current key.
It patches the existing Secrets and preserves the certificate's private key, keeping the gateway's
public-key pin unchanged.

Keep the certificate Secret in place. If it is deleted, Kipper can generate a new key, breaking the
gateway's pin. A saved certificate may also no longer match the current key. Recovery uses the
current key's public half so the restored certificate matches it.

Use OpenSSL 3 and GNU `base64` on the node. Before updating the cluster, the function checks that the
required backups are nonempty, builds a certificate, verifies it against the saved authority and
anchor, and checks its public key. Each restore step stops on failure and reports where it stopped.
The certificate patch requires the Secret's resource version to match the version read at the start;
any intervening update stops the patch, protecting against a concurrent key change.

On the node:

```bash
cd /root/ca-replacement

update_traefik_copy() (
  local cert=$1 version live_pub cert_pub
  umask 077
  trap 'rm -f live.key hop.yaml' EXIT
  if ! kubectl -n kipper-system get secret kipper-hop-cert -o jsonpath='{.data.tls\.key}' \
      | base64 -d > live.key || [ ! -s live.key ] \
    || ! kubectl -n traefik get secret kipper-hop-cert -o jsonpath='{.data.hop\.yaml}' \
      | base64 -d > hop.yaml || [ ! -s hop.yaml ]; then
    echo "STOP: could not read the live key or Traefik's copy. If the copy is missing, run kip upgrade first."; return 1
  fi
  if ! live_pub=$(openssl pkey -in live.key -pubout) || ! cert_pub=$(openssl x509 -in "$cert" -noout -pubkey) \
    || [ -z "$live_pub" ] || [ "$live_pub" != "$cert_pub" ]; then
    echo "STOP: $cert does not match the live key. Traefik's copy is unchanged."; return 1
  fi
  if ! kubectl -n traefik create secret generic kipper-hop-cert --from-file=tls.crt="$cert" \
      --from-file=tls.key=live.key --from-file=hop.yaml=hop.yaml --dry-run=client -o yaml \
      | kubectl apply -f - > /dev/null 2>&1; then
    echo "STOP: writing Traefik's copy failed. Its output is withheld because it can contain the private key."; return 1
  fi
  if ! version=$(kubectl -n traefik get secret kipper-hop-cert -o jsonpath='{.metadata.resourceVersion}') \
    || [ -z "$version" ] \
    || ! kubectl -n traefik patch deployment traefik --type merge \
      -p "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"kipper.run/hop-copy-version\":\"$version\"}}}}}"; then
    echo "STOP: Traefik's copy is updated, but restarting Traefik failed. Check the error, then run it again."; return 1
  fi
  if ! kubectl -n traefik rollout status deployment/traefik --timeout=300s; then
    echo "STOP: Traefik was restarted, but its rollout has not finished. Check the Traefik pods, then run it again."; return 1
  fi
)

restore_authority() {
  local f SAN RV CA_B64 CAKEY_B64 LEAF_B64
  for f in backup-ca.crt backup-ca.key backup-anchor.crt; do
    if [ ! -s "$f" ]; then
      echo "STOP: $f is empty or missing. Cluster configuration is unchanged."; return 1
    fi
  done

  RV=$(kubectl -n kipper-system get secret kipper-hop-cert -o jsonpath='{.metadata.resourceVersion}')
  if [ -z "$RV" ] \
    || ! kubectl -n kipper-system get secret kipper-hop-cert -o jsonpath='{.data.tls\.crt}' \
      | base64 -d > live-leaf.crt || [ ! -s live-leaf.crt ] \
    || ! kubectl -n kipper-system get secret kipper-hop-cert -o jsonpath='{.data.tls\.key}' \
      | base64 -d | openssl pkey -pubout > live.pub 2> /dev/null || [ ! -s live.pub ]; then
    echo "STOP: could not read the stored certificate and public key. Cluster configuration is unchanged."; return 1
  fi

  SAN=$(openssl x509 -in live-leaf.crt -noout -ext subjectAltName | tail -1 | tr -d ' ')
  if [ -z "$SAN" ] || ! printf '%s\n' \
      "subjectAltName=$SAN" \
      "keyUsage=critical,digitalSignature" \
      "extendedKeyUsage=serverAuth" \
      "basicConstraints=critical,CA:FALSE" > restore.ext; then
    echo "STOP: could not read the certificate names or write restore.ext. Cluster configuration is unchanged."; return 1
  fi

  if ! openssl x509 -new -subj "/CN=kipper-hop" -force_pubkey live.pub \
      -CA backup-ca.crt -CAkey backup-ca.key -days 3650 -sha256 \
      -extfile restore.ext -out restored-leaf.crt \
    || ! openssl verify -CAfile backup-ca.crt restored-leaf.crt > /dev/null \
    || ! openssl verify -CAfile backup-anchor.crt restored-leaf.crt > /dev/null \
    || ! openssl x509 -in restored-leaf.crt -noout -pubkey | diff -q - live.pub > /dev/null; then
    echo "STOP: could not create or verify a certificate for the current key using the saved authority and anchor. Cluster configuration is unchanged."; return 1
  fi

  if ! CA_B64=$(base64 -w0 < backup-ca.crt) || [ -z "$CA_B64" ] \
    || ! CAKEY_B64=$(base64 -w0 < backup-ca.key) || [ -z "$CAKEY_B64" ] \
    || ! LEAF_B64=$(base64 -w0 < restored-leaf.crt) || [ -z "$LEAF_B64" ]; then
    echo "STOP: could not encode the authority certificate, authority key, or restored certificate. Cluster configuration is unchanged."; return 1
  fi

  if ! kubectl -n kipper-system patch secret kipper-hop-ca --type merge -p "{\"data\":{
      \"tls.crt\":\"$CA_B64\",
      \"tls.key\":\"$CAKEY_B64\",
      \"pending.crt\":null,
      \"pending.key\":null,
      \"previous.crt\":null
    }}"; then
    echo "STOP: the authority patch failed. Check the error and cluster status before retrying."; return 1
  fi
  if ! kubectl -n kipper-system patch secret kipper-hop-cert --type merge \
      -p "{\"metadata\":{\"resourceVersion\":\"$RV\"},\"data\":{\"tls.crt\":\"$LEAF_B64\"}}"; then
    echo "STOP: authority restored; certificate patch failed. Check the error for a concurrent Secret update or another failure, then retry."; return 1
  fi
  if ! update_traefik_copy restored-leaf.crt; then
    echo "STOP: authority and certificate restored. Updating Traefik's copy or completing its rollout failed. Fix the cause above, then run restore_authority again."; return 1
  fi
  if ! cp backup-anchor.crt /etc/rancher/k3s/.kipper-hop-ca.crt.restore \
    || ! mv /etc/rancher/k3s/.kipper-hop-ca.crt.restore /etc/rancher/k3s/kipper-hop-ca.crt; then
    echo "STOP: authority, certificate and Traefik's copy restored. Updating the anchor file failed. Check the error, then retry."; return 1
  fi
  echo "RESTORED"
}
restore_authority
```

`RESTORED` means the Secret patches, Traefik's rollout onto its copy and the anchor file replacement
succeeded. A `STOP` message identifies the failed step. After resolving the error, rerun the function.
It reads the current key again and issues a fresh certificate under the saved authority. In a new shell, set
`KUBECONFIG=/etc/rancher/k3s/k3s.yaml`, return to `/root/ca-replacement`, and paste both functions again.

The explicit `null` values remove the pending and previous authority entries, clearing the
unfinished replacement state.

If the API server already trusts only the new authority, logins fail once the restored certificate
is served and remain unavailable until the sync below completes.

Recovery restores the authority you intended to replace. If its key was exposed, restart Phase 1
once the cluster is healthy.

Then from your own machine:

```bash
kip cluster auth sync
kip cluster ca status
kip auth verify
```

The status should report no replacement in flight and everything agreeing. Certificate authentication
was working the whole time, so you were never locked out of the cluster itself.

If the authority Secret (`kipper-hop-ca`) is missing, recreate it, then run `restore_authority` again:

```bash
kubectl -n kipper-system create secret tls kipper-hop-ca \
  --cert=backup-ca.crt --key=backup-ca.key
```

## A note on backups

Cluster-wide backups include the namespace holding this authority's private key. If you keep backups
somewhere you would not keep a private key, that is worth knowing, and a leaked backup is the most
likely reason anyone ever reads this page.

After replacing an authority because of a leak, rotate your backup storage credentials too. Old
backups still contain the old key.

## Command reference

| Command | What it does |
| --- | --- |
| `kip cluster ca status` | Shows the authority, what the cluster serves, and whether they agree |
| `kip cluster auth sync` | Rebuilds the API server's authentication config from the anchor on the server |
| `kip auth verify` | Confirms a login actually works end to end |

`kip cluster ca status` and `kip cluster auth sync` reach your server over SSH and accept `--cluster`
and `--ssh-key`. Everything else on this page is `kubectl` and `openssl` run on the node itself.
