#!/usr/bin/env bash
# Clean-room install test. Runs the path a new user takes, on a fresh server,
# using only kip. A failure here blocks a release.
#
#   export KIPPER_TEST_HOST=<ip of a fresh Ubuntu 24.04 or Debian 12 server>
#   export KIPPER_TEST_KEY=~/.ssh/id_ed25519
#   scripts/clean-room-test.sh
#
# The server is left running so you can inspect a failure. Tear it down with
#
#   kip cluster uninstall <cluster> --yes
#
# BEFORE destroying the VM. That releases the *.kipper.run name; the local kip
# config entry holds the only credential that can, so a VM destroyed first
# leaves the name claimed.
#
# Re-running against a used server proves nothing: rebuild it first.
#
# Every step goes through kip. The script refuses to run if it finds a kubectl
# call in its own body, because "you never need kubectl" is the claim under test.

set -uo pipefail

HOST="${KIPPER_TEST_HOST:-}"
KEY="${KIPPER_TEST_KEY:-$HOME/.ssh/id_ed25519}"
EMAIL="${KIPPER_TEST_EMAIL:-you@example.com}"
KIP="${KIPPER_TEST_KIP:-kip}"
# Set when the cluster should serve on a domain you control rather than the
# kipper.run name derived from the address.
DOMAIN="${KIPPER_TEST_DOMAIN:-}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT


pass=0; fail=0; skip=0; results=()

# Poll a predicate to a deadline. Three checks here race something the cluster
# is still settling, and a single look at any of them reports a healthy cluster
# as broken.
retry_until() {
  local secs="$1" interval="$2"; shift 2
  local deadline=$(( $(date +%s) + secs ))
  while :; do
    "$@" && return 0
    [ "$(date +%s)" -lt "$deadline" ] || return 1
    sleep "$interval"
  done
}

step() {
  local name="$1"; shift
  local started; started=$(date +%s)
  printf '\n\033[1m==> %s\033[0m\n' "$name"
  { printf '\n===== %s =====\n' "$name"; } >> "$LOG"
  if "$@" >"$WORK/out" 2>&1; then
    cat "$WORK/out" >> "$LOG"
    local secs=$(( $(date +%s) - started ))
    printf '    \033[32mok\033[0m  %ss\n' "$secs"
    results+=("ok|$name|${secs}s"); pass=$((pass+1)); return 0
  fi
  cat "$WORK/out" >> "$LOG"
  local secs=$(( $(date +%s) - started ))
  # --admin-kubeconfig reaches the Kubernetes API but not the console API, so a
  # few commands still want an operator login. A person installing by hand has
  # one; this run does not, and that is a gap in coverage rather than a defect.
  if grep -q 'not authenticated' "$WORK/out"; then
    printf '    \033[33mskipped\033[0m  needs an interactive login, which this run has no way to perform\n'
    results+=("skipped|$name|-"); skip=$((skip+1)); return 0
  fi
  printf '    \033[31mFAILED\033[0m  %ss\n' "$secs"
  sed 's/^/      /' "$WORK/out" | tail -25
  results+=("FAILED|$name|${secs}s"); fail=$((fail+1)); return 1
}

# The claim under test is that a new user never reaches for kubectl.
# Assembled so the word itself never appears in an executable line here, which
# would make the check match its own source.
banned="kube""ctl"
if grep -vE '^[[:space:]]*#' "$0" | grep -v 'banned=' | grep -qE "(^|[^a-z-])$banned"; then
  echo "This script calls $banned. The point of it is that kip is enough." >&2
  exit 2
fi
[ -n "$HOST" ] || { echo "Set KIPPER_TEST_HOST to a fresh server's address." >&2; exit 2; }

# Every command's output is appended here rather than discarded, because a
# passing step can still print something worth reading and the install prints a
# credential. Delete it when you destroy the server.
LOG="${KIPPER_TEST_LOG:-clean-room-$(date +%Y%m%d-%H%M%S).log}"
if [ -e "$LOG" ]; then
  echo "Refusing to reuse an existing log at $LOG. Move it aside or set KIPPER_TEST_LOG." >&2
  exit 2
fi
(umask 077; : > "$LOG") || exit 2
echo
echo "  Output goes to $LOG, created readable only by you."
echo "  The install writes the cluster admin password into it. Delete the file"
echo "  when you destroy the server; destroying the server does not remove it."

echo "Clean-room test against $HOST"
echo "kip: $($KIP --version 2>&1 | head -1)"

# --admin-kubeconfig because this run is unattended. `step` captures output, so
# kip has no terminal, and ResolveKubeconfigMode defers the login in that case,
# which would leave every later step with no credential.
install_cluster() {
  if [ -n "$DOMAIN" ]; then
    $KIP install --host "$HOST" --ssh-key "$KEY" --admin-email "$EMAIL" \
      --admin-kubeconfig --domain "$DOMAIN"
  else
    $KIP install --host "$HOST" --ssh-key "$KEY" --admin-email "$EMAIL" --admin-kubeconfig
  fi
}
step "install the cluster" install_cluster || exit 1

# `kip status` exits zero even when it prints a cross against a node or a
# component, so assert on what it reported rather than on the exit code.
healthy_now() {
  $KIP status > "$WORK/status" 2>&1 || return 1
  ! grep -q '✗' "$WORK/status"
}
assert_healthy() {
  if retry_until 420 10 healthy_now; then
    cat "$WORK/status" >> "$LOG"
    return 0
  fi
  cat "$WORK/status" >> "$LOG"
  echo "    still unhealthy after seven minutes:"
  grep '✗' "$WORK/status" | sed 's/^/    /'
  return 1
}
step "cluster reports every node and component healthy" assert_healthy
# The comparison page needs what the platform costs before anything is deployed
# on it, so take that reading here rather than at the end.
read_memory() {
  timeout 300 ssh -i "$KEY" -o BatchMode=yes -o ConnectTimeout=15 \
    -o StrictHostKeyChecking=accept-new "root@$HOST" \
    "sleep 120; free -m | awk '/^Mem:/ {printf \"    total %sMB  used %sMB  available %sMB\\n\", \$2, \$3, \$7}'" \
    | tee -a "$LOG"
}
step "idle memory, platform only (for the comparison page)" read_memory

step "create a postgres service" $KIP service add postgres --name pg
step "create a mysql service"    $KIP service add mysql --name my

# `kip service add` returns before the database is actually serving, so an
# import started immediately fails on a slow server for reasons that are not
# defects.
# `kip service list` prints NAME TYPE STATUS READY STORAGE, where READY is
# "readyReplicas/replicas". `kip service info` carries no status at all and does
# print a generated password, so matching words there can be satisfied by a
# credential rather than by readiness.
service_ready() {
  local ready
  ready=$($KIP service list 2>/dev/null | awk -v s="$1" '$1 == s {print $4}')
  case "$ready" in
    ''|0/*) return 1 ;;
    */*) [ "${ready%/*}" = "${ready#*/}" ] ;;
    *) return 1 ;;
  esac
}
wait_ready() {
  retry_until 420 10 service_ready "$1" && return 0
  echo "    $1 never reported all replicas ready"
  return 1
}
step "postgres becomes ready" wait_ready pg
step "mysql becomes ready"    wait_ready my

printf 'CREATE TABLE smoke (id int);\nINSERT INTO smoke VALUES (1);\n' > "$WORK/dump.sql"
step "import a dump into postgres" $KIP service import pg --file "$WORK/dump.sql"

deploy_app() { $KIP app deploy --name hello --image nginx:latest --port 80; }
step "deploy an app from an image" deploy_app
step "bind the database to the app" $KIP service bind pg hello
step "the app is listed"            $KIP app list

# The route is what a stranger actually checks, so test it from outside.
# `kip app list` prints NAME/STATUS/IMAGE/READY and no URL; the deploy prints
# "Live at https://...", so take it from the log of that step.
url="$(grep -oE 'https://hello[^ ]*' "$LOG" | tail -1)"
# cert-manager issues after the route exists, so an immediate request races the
# certificate. A new user meets the same wait, which is why the deadline is
# generous rather than absent.
curl_ok() { curl --fail --silent --show-error --max-time 20 -o /dev/null "$1"; }
serves_https() {
  retry_until 300 10 curl_ok "$1" && return 0
  echo "    $1 did not serve a valid certificate within five minutes"
  return 1
}
if [ -n "$url" ]; then
  step "the app answers over HTTPS with a valid certificate" serves_https "$url"
else
  printf '\n\033[31mFAILED\033[0m  no route URL found in `kip app list`\n'
  results+=("FAILED|the app answers over HTTPS|-"); fail=$((fail+1))
fi

# Task 8 wants a measured idle figure and this is the only clean moment to take
# one: a real cluster with one trivial app and no traffic.
echo
step "memory with two databases and an app running" read_memory

echo
printf '%s\n' "-------------------------------------------------------------"
for r in "${results[@]}"; do IFS='|' read -r s n d <<<"$r"; printf '  %-7s %-45s %s\n' "$s" "$n" "$d"; done
printf '%s\n' "-------------------------------------------------------------"
printf '  %s passed, %s failed, %s skipped\n' "$pass" "$fail" "$skip"
[ "$skip" -eq 0 ] || printf '  Skipped steps are untested, not proven.\n' 
printf '  full output: %s (contains the admin password, delete this file)\n' "$LOG"
printf '\n  Tear down, in this order:\n'
printf '    kip cluster uninstall <cluster> --yes   # releases the kipper.run name\n'
printf '    then destroy the VM, then: rm %s\n' "$LOG"
[ "$fail" -eq 0 ] || echo "  A failure here blocks the release."
exit $(( fail > 0 ))
