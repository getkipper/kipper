#!/usr/bin/env bash
# Reports every reference to a container-image organisation across a cluster's
# workload specs, not just its running pods.
#
# Running pods are the wrong thing to check before retiring a registry: a
# Deployment whose pods are up but whose template still names the old org
# breaks on its next rollout, and a CronJob breaks on its next schedule, hours
# after anyone was watching. This walks the specs that decide what gets pulled
# next, including the Kipper custom resources that controllers materialise
# those specs from.
#
# Usage:
#   scripts/check-image-org.sh oldorg            # current KUBECONFIG
#   scripts/check-image-org.sh oldorg ~/.kip/clusters/example.yaml
#
# Exit 0 when nothing that will pull again names the org, 1 when something
# does, 2 when the check could not run. The distinction is the point: this
# gates the deletion of a registry, so "I could not check" must never read as
# "all clear".
#
# Known limits, both erring towards reporting too much rather than too little,
# because a false blocker is visible and a false clean deletes a registry:
#
#   - A reference reached through valueFrom (a ConfigMap or Secret key rather
#     than a literal) cannot be resolved from the object alone and is not
#     followed. Retiring a registry that a ConfigMap names is not covered here.
#   - An untagged path such as "<org>/name" under a key that is not a known path
#     field is reported, because it is grammatically identical to an untagged
#     Docker Hub reference. Keys that can only hold a filesystem path, and paths
#     ending in a recognised file extension, are excluded; what remains is
#     reported so a human decides rather than skipped so nobody does.
#   - A reference assembled at runtime from parts ("ghcr.io/" + org + "/name")
#     is invisible to any check that reads strings, including this one. The
#     repository test in kip/internal/installer guards the shipped constants,
#     which is where such a reference would have to be built.
set -euo pipefail

ORG="${1:-}"
if [[ -z "$ORG" ]]; then
  echo "usage: $(basename "$0") <image-org> [kubeconfig]" >&2
  exit 2
fi
[[ -n "${2:-}" ]] && export KUBECONFIG="$2"

if ! kubectl version -o json >/dev/null 2>&1; then
  echo "  ✘  cannot reach the cluster; refusing to report a clean result" >&2
  exit 2
fi

SCAN_PY="$(mktemp)"
cat > "$SCAN_PY" <<'PYEOF'
import json
import re
import sys

org = sys.argv[1]

# A string is split into tokens and each is judged on its own, because an image
# reference reaches a container inside a command line as readily as on its own
# ("--image=org/app:v1", "docker pull ghcr.io/org/tool:v2"). Anything that cannot
# appear inside a reference is a delimiter, so a reference followed by prose
# punctuation still yields a clean token.
TOKEN_SPLIT = re.compile(r"[^A-Za-z0-9._:/@+\[\]-]+")

# The org must be a whole path segment: a plain substring test would report every
# ghcr.io/getkipper reference when asked about "kipper", and would report a
# getkipper-old reference when asked about "getkipper".
#
# Any number of segments may precede the org, because a registry host is only one
# of them: GitLab, Harbor and Artifact Registry all nest an organisation under a
# group or project (registry.gitlab.com/group/<org>/app:v1).
IMAGE_REF = re.compile(
    # Registry host and/or groups. The first alternative carries the tail of a
    # bracketed IPv6 literal, whose opening bracket is stripped with the token.
    r"^(?:(?:[A-Fa-f0-9:]+\]|[A-Za-z0-9][A-Za-z0-9._-]*)(?::\d+)?/)*"
    + re.escape(org) + r"/"                            # the org, whole segment
    r"[A-Za-z0-9][A-Za-z0-9._-]*"                      # repository
    r"(?:/[A-Za-z0-9][A-Za-z0-9._-]*)*"                # nested repository path
    r"(?::[A-Za-z0-9._-]+)?"                           # optional tag
    r"(?:@[A-Za-z0-9][A-Za-z0-9._+-]*:[A-Fa-f0-9]{32,})?$"  # optional digest, any algorithm
)

# Hosts that serve source, not images. `github.com/org/repo` is a valid image
# reference by grammar alone, so without this a README URL or a SOURCE_URL env
# value raises a blocker that repointing every image cannot clear. Anything not
# on this list is still reported, so an unknown host fails closed.
NON_REGISTRY_HOSTS = {
    "github.com", "www.github.com",
    "gitlab.com", "www.gitlab.com",
    "bitbucket.org", "www.bitbucket.org",
}

# A relative file path under a directory that happens to share the org's name is
# a valid reference by grammar alone ("kipper/setup.sh" as workingDir or an arg).
# An untagged reference whose last segment carries one of these extensions is
# treated as a path. A reference with a tag or digest is never excluded this way,
# because no file path carries one.
FILE_EXTENSIONS = {
    "sh", "bash", "md", "yaml", "yml", "json", "txt", "py", "go", "js", "ts",
    "conf", "cfg", "ini", "xml", "html", "css", "sql", "log", "pem", "crt",
    "key", "toml", "lock", "gz", "tar", "zip", "tpl", "env",
}

def looks_like_a_path(token):
    if ":" in token or "@" in token:      # a tag or digest; no file path has one
        return False
    _, _, last = token.rpartition("/")
    _, dot, ext = last.rpartition(".")
    return bool(dot) and ext.lower() in FILE_EXTENSIONS

# Schemes that name an image to pull, as `skopeo` and `crane` take them. The
# scheme is stripped and what remains is judged as a reference, because
# "docker://ghcr.io/org/tool:v2" in a CronJob's args pulls exactly as an image
# field does. Every other scheme names a source, not an image.
# Only the transports that name a remote registry. dir:, oci-archive:,
# docker-archive:, docker-daemon: and containers-storage: read a local directory,
# archive or store, so a reference behind one of them does not contact the
# registry being retired and must not hold the gate shut. They are left
# unstripped, which is enough: the leading "dir:" keeps the token from matching.
IMAGE_TRANSPORTS = ("docker://", "oci://")

# Transports that read a local directory, archive or daemon store. These are
# rejected outright rather than left to fail the grammar: "dir:8080/<org>/x"
# would otherwise match, reading "dir" as a host and "8080" as its port, and
# hold the gate shut over something that never contacts the registry.
LOCAL_TRANSPORTS = ("dir:", "oci-archive:", "docker-archive:", "docker-daemon:",
                    "containers-storage:", "tarball:", "sif:", "ostree:")

def image_ref_in(text):
    """The image references in one string, ignoring things that merely share the
    org's path."""
    for token in TOKEN_SPLIT.split(text):
        if not token:
            continue
        # The transport comes off before the brackets, so a reference that has
        # both (docker://[2001:db8::1]:5000/org/app) survives each step.
        if token.lower().startswith(LOCAL_TRANSPORTS):
            continue
        for transport in IMAGE_TRANSPORTS:
            if token.lower().startswith(transport):
                token = token[len(transport):].lstrip("/")
                break
        token = token.strip(".-/[")
        if not token or "://" in token or token.startswith("git@"):
            continue
        if token.split("/", 1)[0].lower() in NON_REGISTRY_HOSTS:
            continue
        if looks_like_a_path(token):
            continue
        if IMAGE_REF.match(token):
            yield token

try:
    items = json.load(sys.stdin).get("items", [])
except Exception as exc:                      # a parse failure is not "nothing found"
    print("PARSE_ERROR " + str(exc), file=sys.stderr)
    sys.exit(3)

# managedFields and the applied-configuration annotation carry a copy of the
# whole spec, so matching inside them reports a reference a second time under a
# key that pulls nothing. Only those two are skipped: annotations at large are
# controller input and can name an image an admission webhook will inject, so
# skipping the whole map would hide desired state that does pull.
SKIP_META_KEYS = {"managedFields"}
SKIP_ANNOTATIONS = {
    "kubectl.kubernetes.io/last-applied-configuration",
}

# Keys whose value is a filesystem path by schema. A directory under a path that
# happens to share the org's name ("<org>/config") is grammatically identical to
# an untagged image reference, and no file extension gives it away. Skipping the
# keys that can only hold a path removes that whole class without weakening the
# fields an image can actually reach a container through.
PATH_KEYS = {"mountPath", "subPath", "subPathExpr", "workingDir", "hostPath",
             "path", "mountPropagation", "defaultMode"}

def refs(node, out, in_meta=False, in_annotations=False):
    if isinstance(node, dict):
        for key, value in node.items():
            if in_meta and key in SKIP_META_KEYS:
                continue
            if in_annotations and key in SKIP_ANNOTATIONS:
                continue
            if key in PATH_KEYS and isinstance(value, str):
                continue
            refs(value, out,
                 in_meta or key == "metadata",
                 in_annotations or (in_meta and key == "annotations"))
    elif isinstance(node, list):
        for value in node:
            refs(value, out, in_meta, in_annotations)
    elif isinstance(node, str):
        for found in image_ref_in(node):
            out.add(found)

for item in items:
    meta = item.get("metadata", {})
    where = "{}/{}".format(meta.get("namespace", "-"), meta.get("name", "?"))
    found = set()
    refs(item, found)
    for ref in sorted(found):
        print("    {}  {}".format(where, ref))
PYEOF

# The resource types this cluster actually serves. A Kipper CR kind is absent on
# a plain Kubernetes cluster, and that absence is conclusive — a type the API
# does not serve can hold no instances — so it is skipped rather than treated as
# a failed check. Anything else that goes wrong still fails closed.
if ! SERVED_KINDS="$(kubectl api-resources -o name 2>/dev/null)"; then
  echo "  ✘  could not list the cluster's resource types; refusing to report a clean result" >&2
  exit 2
fi

qualified_kinds() {  # qualified_kinds <kind>; every served resource with that name
  # api-resources prints group-qualified names ("deployments.apps"), while the
  # kind lists here use the short form for core kinds and the qualified form for
  # the CRs. Every match is printed, not just the first: when two API groups
  # serve the same name, `kubectl get <short>` silently resolves to the
  # preferred one and the other's objects would never be scanned. Getting this
  # wrong skips a kind in silence, which is the one outcome this script must
  # never produce.
  local escaped
  escaped="$(printf '%s' "$1" | sed 's/[.]/\\./g')"
  printf '%s\n' "$SERVED_KINDS" | grep -E "^${escaped}(\.|$)"
}

# Every object this run actually looked at, by UID. A child is only derived if
# its controller is in here: an owner that was deleted with an orphaning policy
# leaves the child running with nothing above it to rewrite the reference, and
# its kind alone cannot tell you that.
SCANNED_UIDS="$(mktemp)"
trap 'rm -f "$SCAN_PY" "$SCANNED_UIDS"' EXIT

record_uids() {  # record_uids <json>
  printf '%s' "$1" | python3 -c '
import json, sys
for item in json.load(sys.stdin).get("items", []):
    uid = item.get("metadata", {}).get("uid")
    if uid:
        print(uid)
' >> "$SCANNED_UIDS" 2>/dev/null || true
}

scan_kind() {  # scan_kind <kind>; prints hits, returns non-zero only on error
  local kind="$1" json hits
  if ! json="$(kubectl get "$kind" -A -o json 2>/dev/null)"; then
    echo "  ✘  could not list $kind; refusing to report a clean result" >&2
    return 2
  fi
  record_uids "$json"
  if ! hits="$(printf '%s' "$json" | python3 "$SCAN_PY" "$ORG")"; then
    echo "  ✘  scan of $kind failed; refusing to report a clean result" >&2
    return 2
  fi
  printf '%s' "$hits"
}

# Desired state: what a controller will pull next. These block retirement.
# The Kipper CRs are here because they are the durable desired state — a
# controller rebuilds the Deployment from the App, so a clean Deployment over a
# stale App is a reference waiting to come back.
BLOCKING_KINDS=(deployments statefulsets daemonsets cronjobs replicationcontrollers
                apps.kipper.run functions.kipper.run jobs.kipper.run)
# Nothing is derived by virtue of its kind. A ReplicaSet created by a Deployment
# is rewritten when that Deployment rolls, but one created directly is desired
# state that will recreate its pods; the same distinction decides a Pod. Both are
# split by ownership below rather than listed here, because assuming a kind is
# always derived is how a live reference gets reported as history.

found=0
skipped=()
for kind in "${BLOCKING_KINDS[@]}"; do
  mapfile -t qualified < <(qualified_kinds "$kind")
  if [[ "${#qualified[@]}" -eq 0 ]]; then
    skipped+=("$kind")
    continue
  fi
  for served in "${qualified[@]}"; do
    hits="$(scan_kind "$served")" || exit 2
    if [[ -n "$hits" ]]; then
      count="$(printf '%s\n' "$hits" | grep -c .)"
      echo "  $served ($count)"
      printf '%s\n' "$hits"
      found=$((found + count))
    fi
  done
done

# Batch Jobs split by whether they can still pull. A Job that has not finished
# will start a pod; one that has completed is history its CronJob's TTL clears.
# Group-qualified: this cluster serves both jobs.batch and jobs.kipper.run, and
# an unqualified `kubectl get jobs` resolves to whichever group is preferred.
if ! jobs_json="$(kubectl get jobs.batch -A -o json 2>/dev/null)"; then
  echo "  ✘  could not list jobs.batch; refusing to report a clean result" >&2
  exit 2
fi
record_uids "$jobs_json"
for state in active complete; do
  if ! hits="$(printf '%s' "$jobs_json" \
      | python3 -c '
import json, sys
state = sys.argv[1]
data = json.load(sys.stdin)
keep = []
for item in data.get("items", []):
    conds = item.get("status", {}).get("conditions") or []
    done = any(c.get("type") in ("Complete", "Failed") and c.get("status") == "True"
               for c in conds)
    if (state == "complete") == done:
        keep.append(item)
print(json.dumps({"items": keep}))
' "$state" | python3 "$SCAN_PY" "$ORG")"; then
    echo "  ✘  scan of jobs ($state) failed; refusing to report a clean result" >&2
    exit 2
  fi
  [[ -z "$hits" ]] && continue
  count="$(printf '%s\n' "$hits" | grep -c .)"
  if [[ "$state" == "active" ]]; then
    echo "  jobs, unfinished ($count)"
    printf '%s\n' "$hits"
    found=$((found + count))
  else
    echo "  jobs, completed ($count) — history, will not pull again"
    printf '%s\n' "$hits"
  fi
done

# Pods and ReplicaSets split by whether something that gets scanned will rewrite
# them. Being owned is not enough on its own: a pod owned by a standalone
# ReplicaSet, or by a Node as a static mirror pod, has no scanned desired state
# above it to correct the reference, so it is blocking like anything else.
owner_filter='
import json, sys

want_derived = sys.argv[1] == "derived"
with open(sys.argv[2]) as fh:
    scanned = {line.strip() for line in fh if line.strip()}

data = json.load(sys.stdin)
keep = []
for item in data.get("items", []):
    owners = item.get("metadata", {}).get("ownerReferences") or []
    # Only a controller owner rewrites its child. A plain owner reference is a
    # garbage-collection link: it deletes the child with the parent but never
    # updates it, so it cannot make a stale reference someone else problem.
    controller = next((o for o in owners if o.get("controller")), None)
    # And the controller has to still exist. A parent deleted with an orphaning
    # policy leaves the child running with its reference frozen, which is the
    # case that looks derived and is not.
    derived = controller is not None and controller.get("uid") in scanned
    if derived == want_derived:
        keep.append(item)
print(json.dumps({"items": keep}))
'

split_scan() {  # split_scan <kind> <label>; blocking half counted, derived half tallied
  local kind="$1" label="$2" json blocking derived_hits
  if ! json="$(kubectl get "$kind" -A -o json 2>/dev/null)"; then
    echo "  ✘  could not list $kind; refusing to report a clean result" >&2
    exit 2
  fi
  # Recorded before the split so a pod owned by one of these ReplicaSets can see
  # its owner. The order of the two split_scan calls is what makes that hold.
  record_uids "$json"
  if ! blocking="$(printf '%s' "$json" | python3 -c "$owner_filter" standalone "$SCANNED_UIDS" | python3 "$SCAN_PY" "$ORG")"; then
    echo "  ✘  scan of standalone $kind failed; refusing to report a clean result" >&2
    exit 2
  fi
  if [[ -n "$blocking" ]]; then
    local count
    count="$(printf '%s\n' "$blocking" | grep -c .)"
    echo "  $label ($count) — nothing scanned will replace these"
    printf '%s\n' "$blocking"
    found=$((found + count))
  fi
  if ! derived_hits="$(printf '%s' "$json" | python3 -c "$owner_filter" derived "$SCANNED_UIDS" | python3 "$SCAN_PY" "$ORG")"; then
    echo "  ✘  scan of owned $kind failed; refusing to report a clean result" >&2
    exit 2
  fi
  [[ -n "$derived_hits" ]] && derived=$((derived + $(printf '%s\n' "$derived_hits" | grep -c .)))
  return 0
}

derived=0
split_scan replicasets "replicasets with no scanned owner"
split_scan pods "pods with no scanned owner"

echo
if [[ "${#skipped[@]}" -gt 0 ]]; then
  echo "  ·  not served by this cluster, so nothing of that kind exists: ${skipped[*]}"
fi
if [[ "$derived" -gt 0 ]]; then
  echo "  ·  $derived derived reference(s) in replicasets/pods, which clear as their owners roll"
  echo "     (an old ReplicaSet still names it, so a rollback onto that revision would"
  echo "      fail to pull once the registry is gone)"
fi
if [[ "$found" -eq 0 ]]; then
  echo "  ✔  nothing that will pull again references '$ORG'"
  exit 0
fi
echo "  ✘  $found spec reference(s) to '$ORG' remain — do not retire it yet"
exit 1
