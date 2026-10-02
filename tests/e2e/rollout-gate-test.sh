#!/usr/bin/env bash
set -euo pipefail

# End-to-end test: rollouts wait for new pods to pass their health check, so a
# slow-starting app answers every request while it is replaced.
#
# Usage:
#   KIPPER_TEST_ROUTE_HOST=slowstart.example.com ./tests/e2e/rollout-gate-test.sh
#
# Runs against the cluster kip currently points at. The route host must resolve
# to that cluster. The app is built in the cluster from tests/e2e/slowstart on
# REPO_BRANCH, so that branch has to be pushed first.
#
# Optional:
#   REPO_URL               default https://github.com/getkipper/kipper
#   REPO_BRANCH            default the current git branch
#   KIPPER_TEST_UPGRADE=1  also run the upgrade case; the cluster must be on the
#                          previous release, with this app deployed by it
#   KIPPER_TEST_BIG_CPU    a CPU request no node can place (e.g. 64); runs the
#                          capacity case when set
#
# Meant to be run by hand before a release. Nothing here calls the cluster
# except through kip.

banned="kube""ctl"
if grep -vE '^[[:space:]]*#' "$0" | grep -v 'banned=' | grep -qE "(^|[^a-z-])$banned"; then
  echo "This script calls $banned. The point of it is that kip is enough." >&2
  exit 2
fi

ROUTE_HOST="${KIPPER_TEST_ROUTE_HOST:?Set KIPPER_TEST_ROUTE_HOST to a hostname that resolves to the cluster}"
REPO_URL="${REPO_URL:-https://github.com/getkipper/kipper}"
REPO_BRANCH="${REPO_BRANCH:-$(git rev-parse --abbrev-ref HEAD)}"
KIP="${KIP:-./kip/kip}"
APP=slowstart
SCOPE=(--project rollout-gate --environment test)
WORK="$(mktemp -d "${TMPDIR:-.}/rollout-gate.XXXXXX")"
LOAD_PID=""

RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'
pass=0
fail=0

ok() { echo -e "  ${GREEN}✔${NC}  $1"; pass=$((pass + 1)); }
bad() { echo -e "  ${RED}✗${NC}  $1"; fail=$((fail + 1)); }

stop_load() {
  if [ -n "$LOAD_PID" ]; then
    kill "$LOAD_PID" 2>/dev/null || true
    wait "$LOAD_PID" 2>/dev/null || true
    LOAD_PID=""
  fi
}
trap stop_load EXIT

# Requests the route repeatedly, pausing 100ms between requests. Record the
# HTTP status and instance ID; 000 indicates that no HTTP response was received.
start_load() {
  local out="$1"
  : > "$out"
  (
    while true; do
      curl -s -o /dev/null --max-time 5 -w '%{http_code} %header{x-instance-id}\n' "https://$ROUTE_HOST/" >> "$out" || true
      sleep 0.1
    done
  ) &
  LOAD_PID=$!
}

failures() { grep -cvE '^2[0-9][0-9] ' "$1" || true; }
requests() { wc -l < "$1" | tr -d ' '; }

# Waits until the app has every replica ready and no rollout note.
wait_settled() {
  local deadline=$((SECONDS + ${1:-900}))
  while [ $SECONDS -lt $deadline ]; do
    local list
    list="$($KIP app list "${SCOPE[@]}" 2>&1)" || true
    if echo "$list" | grep -qE "^  $APP .* 2/2" && ! echo "$list" | grep -q "$APP is still rolling out"; then
      return 0
    fi
    sleep 5
  done
  echo "$list"
  return 1
}

# Waits for a rollout to show in kip app list, so a following wait_settled
# does not accept the state from before the change. A rollout that finishes
# within the window may never show; the replacement check in under_load
# covers that case.
wait_started() {
  local deadline=$((SECONDS + 60))
  while [ $SECONDS -lt $deadline ]; do
    $KIP app list "${SCOPE[@]}" 2>&1 | grep -q "$APP is still rolling out" && return 0
    sleep 2
  done
  return 0
}

# The built app answers "slowstart"; the placeholder shown until the first
# build answers a page of its own. Every case needs the real app, or it proves
# nothing about a slow start.
require_built() {
  local deadline=$((SECONDS + ${1:-60}))
  while [ $SECONDS -lt $deadline ]; do
    [ "$(curl -s --max-time 5 "https://$ROUTE_HOST/")" = "slowstart" ] && return 0
    sleep 10
  done
  return 1
}

# The instance IDs in the given lines of a request log.
ids() { awk '{print $2}' | grep -v '^$' | sort -u || true; }

# Runs one action under load and prints how many requests failed, or why the
# case proves nothing: the action failed, the rollout never finished, or an
# instance that answered before the action still answers at the end, so the
# pods were not all replaced. It runs in a command substitution, so it reports
# rather than counting.
under_load() {
  local name="$1"
  shift
  local log="$WORK/$name.log"
  start_load "$log"
  sleep 5
  # Record the instances observed before the action for comparison afterwards.
  local before_lines
  before_lines="$(requests "$log")"
  if ! "$@" > "$WORK/$name.out" 2>&1; then
    stop_load
    echo "action-failed"
    return 0
  fi
  wait_started
  if ! wait_settled > "$WORK/$name.settle" 2>&1; then
    stop_load
    echo "unsettled"
    return 0
  fi
  sleep 5
  stop_load
  local before after
  before="$(head -n "$before_lines" "$log" | ids)"
  after="$(tail -n 20 "$log" | ids)"
  if [ -z "$before" ] || [ -z "$after" ] || [ -n "$(comm -12 <(echo "$before") <(echo "$after"))" ]; then
    echo "no-replacement"
    return 0
  fi
  echo "$(failures "$log") of $(requests "$log")"
}

echo ""
echo "Rollout gate test against $ROUTE_HOST, building $REPO_BRANCH"
echo ""

echo "Setup"
REPO_URL="$REPO_URL" REPO_BRANCH="$REPO_BRANCH" ROUTE_HOST="$ROUTE_HOST" \
  envsubst < tests/e2e/slowstart/kipper.yaml.tmpl > "$WORK/kipper.yaml"
if [ "${KIPPER_TEST_UPGRADE:-}" != "1" ]; then
  $KIP apply -f "$WORK/kipper.yaml" > "$WORK/apply.out" 2>&1 && ok "manifest applied" || { bad "manifest applied"; cat "$WORK/apply.out"; exit 1; }
  wait_settled 1800 && ok "app deployed" || { bad "app deployed"; exit 1; }
fi
require_built 1800 && ok "the built app answers, not the placeholder" || { bad "the built app answers, not the placeholder"; exit 1; }

if [ "${KIPPER_TEST_UPGRADE:-}" = "1" ]; then
  echo ""
  echo "Case: an upgrade restarts nothing"
  wait_settled
  log="$WORK/upgrade.log"
  start_load "$log"
  sleep 3
  before="$(awk '{print $2}' "$log" | sort -u | grep -v '^$' || true)"
  [ -n "$before" ] || bad "no instance ID answered before the upgrade, so the comparison below proves nothing"
  $KIP upgrade > "$WORK/upgrade.out" 2>&1 && ok "kip upgrade ran" || bad "kip upgrade ran"
  sleep 30
  stop_load
  after="$(tail -n 100 "$log" | awk '{print $2}' | sort -u | grep -v '^$' || true)"
  if [ -n "$before" ] && [ "$before" = "$after" ]; then ok "the same pods serve before and after"; else bad "pods changed: $(echo "$before" | tr '\n' ' ') -> $(echo "$after" | tr '\n' ' ')"; fi
  [ "$(failures "$log")" = "0" ] && ok "no failed request during the upgrade" || bad "$(failures "$log") failed requests during the upgrade"
  # The first rollout after an upgrade is gated by the new pods' check, but
  # its old pods stop without the drain, so failures here are recorded only.
  result="$(under_load adoption-restart "$KIP" app restart "$APP" "${SCOPE[@]}")"
  echo "      first rollout after the upgrade: $result requests failed (information only)"
fi

echo ""
echo "Case: a declared check"
$KIP app update "$APP" "${SCOPE[@]}" --health tcp > "$WORK/declare.out" 2>&1 && ok "declared a tcp check" || bad "declared a tcp check"
wait_started
wait_settled || bad "the declaring rollout settled"
require_built || bad "the built app still answers"
result="$(under_load declared-restart "$KIP" app restart "$APP" "${SCOPE[@]}")"
[ "${result%% *}" = "0" ] && ok "restart: $result requests failed" || bad "restart: $result requests failed"
"$KIP" app env set "$APP" "${SCOPE[@]}" E2E_MARK="$RANDOM" > "$WORK/env.out" 2>&1 || bad "env change accepted"
result="$(under_load declared-env-restart "$KIP" app restart "$APP" "${SCOPE[@]}")"
[ "${result%% *}" = "0" ] && ok "env change and restart: $result requests failed" || bad "env change and restart: $result requests failed"

echo ""
echo "Case: an inferred check"
$KIP app update "$APP" "${SCOPE[@]}" --health auto > "$WORK/auto.out" 2>&1 && ok "back to automatic" || bad "back to automatic"
wait_started
wait_settled || bad "the automatic rollout settled"
result="$(under_load inferred-restart "$KIP" app restart "$APP" "${SCOPE[@]}")"
[ "${result%% *}" = "0" ] && ok "restart: $result requests failed" || bad "restart: $result requests failed"

echo ""
echo "Case: no check, for comparison"
$KIP app update "$APP" "${SCOPE[@]}" --health none > "$WORK/none.out" 2>&1 || bad "switched the check off"
wait_started
wait_settled || bad "the unchecked rollout settled"
result="$(under_load control-restart "$KIP" app restart "$APP" "${SCOPE[@]}")"
echo "      without a check, $result requests failed (expected above zero)"
# Without a check the slow start has to show as failures. If it does not, the
# cases above passed for some other reason and prove nothing.
case "$result" in
  [1-9]*) ok "the control shows the gap the check closes" ;;
  *) bad "the control showed no failures ($result), so this run cannot show that the check makes a difference" ;;
esac
$KIP app update "$APP" "${SCOPE[@]}" --health auto > /dev/null 2>&1 || bad "back to automatic after the control"
wait_started
wait_settled || bad "the app settled after the control"

if [ -n "${KIPPER_TEST_BIG_CPU:-}" ]; then
  echo ""
  echo "Case: a new pod that cannot be placed"
  log="$WORK/capacity.log"
  start_load "$log"
  $KIP app update "$APP" "${SCOPE[@]}" --cpu-request "$KIPPER_TEST_BIG_CPU" > "$WORK/capacity.out" 2>&1 || true
  sleep 90
  list="$($KIP app list "${SCOPE[@]}" 2>&1 || true)"
  stop_load
  echo "$list" | grep -q "cannot be placed" && ok "kip app list says the new pod cannot be placed" || bad "kip app list says the new pod cannot be placed"
  [ "$(failures "$log")" = "0" ] && ok "the old pods kept serving" || bad "$(failures "$log") failed requests while the rollout waited"
  # An explicit small size: automatic sizing would keep the live, unplaceable
  # request.
  $KIP app update "$APP" "${SCOPE[@]}" --cpu 100m > /dev/null 2>&1 || bad "lowered the request again"
  wait_started
  wait_settled || bad "the app recovered after the request was lowered"
fi

echo ""
echo "Logs and request records are in $WORK"
echo -e "Passed: ${GREEN}$pass${NC}  Failed: ${RED}$fail${NC}"
[ "$fail" -eq 0 ]
