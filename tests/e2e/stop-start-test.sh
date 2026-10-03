#!/usr/bin/env bash
set -euo pipefail

# End-to-end test: a stopped app runs no pods, its route answers with the
# stopped page behind the route's own gates, the database it is bound to keeps
# running, and a start brings back its replica count and autoscaling.
#
# Usage:
#   KIPPER_TEST_ROUTE_HOST=stopstart.example.com ./tests/e2e/stop-start-test.sh
#
# Run manually against the cluster selected by kip. The route host must resolve
# to that cluster. Management operations use kip; HTTP checks use curl.

banned="kube""ctl"
if grep -vE '^[[:space:]]*#' "$0" | grep -v 'banned=' | grep -qE "(^|[^a-z-])$banned"; then
  echo "This script calls $banned. The point of it is that kip is enough." >&2
  exit 2
fi

ROUTE_HOST="${KIPPER_TEST_ROUTE_HOST:?Set KIPPER_TEST_ROUTE_HOST to a hostname that resolves to the cluster}"
KIP="${KIP:-./kip/kip}"
APP=web
SCOPE=(--project stop-start --environment test)
WORK="$(mktemp -d "${TMPDIR:-.}/stop-start.XXXXXX")"

RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'
pass=0
fail=0

ok() { echo -e "  ${GREEN}✔${NC}  $1"; pass=$((pass + 1)); }
bad() { echo -e "  ${RED}✗${NC}  $1"; fail=$((fail + 1)); }

# Renders the manifest. $1 is requireApiKey (true or false), $2 a stopped
# block or nothing.
manifest() {
  ROUTE_HOST="$ROUTE_HOST" API_KEY="$1" STOPPED="$2" \
    envsubst < tests/e2e/stopstart/kipper.yaml.tmpl > "$WORK/kipper.yaml"
}

# Waits until kip app list shows the app with the given ready count.
wait_ready() {
  local want="$1" deadline=$((SECONDS + ${2:-600}))
  while [ $SECONDS -lt $deadline ]; do
    if $KIP app list "${SCOPE[@]}" 2>&1 | grep -qE "^  $APP .* $want"; then
      return 0
    fi
    sleep 5
  done
  $KIP app list "${SCOPE[@]}" || true
  return 1
}

# Waits until kip service list shows the database running.
wait_db() {
  local deadline=$((SECONDS + ${1:-600}))
  while [ $SECONDS -lt $deadline ]; do
    if $KIP service list "${SCOPE[@]}" 2>&1 | grep -qE "^  db +postgres +running"; then
      return 0
    fi
    sleep 5
  done
  $KIP service list "${SCOPE[@]}" || true
  return 1
}

# Prints the HTTP status code and saves the response body in $WORK/body.
fetch() {
  curl -s --max-time 10 -o "$WORK/body" -w '%{http_code}' "$@" "https://$ROUTE_HOST/" || true
}

echo ""
echo "Stop and start test against $ROUTE_HOST"
echo ""

echo "Setup"
manifest false ""
$KIP apply -f "$WORK/kipper.yaml" > "$WORK/apply.out" 2>&1 && ok "manifest applied" || { bad "manifest applied"; cat "$WORK/apply.out"; exit 1; }
wait_ready "2/2" 900 && ok "app running with two replicas" || { bad "app running with two replicas"; exit 1; }
wait_db 600 && ok "the database is running" || { bad "the database is running"; exit 1; }
[ "$(fetch)" = "200" ] && ok "the route serves the app" || bad "the route serves the app"

echo ""
echo "Case: stop"
$KIP app stop "$APP" "${SCOPE[@]}" --reason "e2e stop" > "$WORK/stop.out" 2>&1 && ok "kip app stop" || { bad "kip app stop"; cat "$WORK/stop.out"; }
wait_ready "0/2" 300 && ok "no pods left, two replicas remembered" || bad "no pods left, two replicas remembered"
$KIP app list "${SCOPE[@]}" 2>&1 | grep -q "$APP is stopped since .* e2e stop" && ok "kip app list says who stopped it and why" || bad "kip app list says who stopped it and why"
status="$(fetch -H 'Accept: text/html')"
[ "$status" = "503" ] && grep -q "This app is stopped" "$WORK/body" && ok "the route answers the stopped page" || bad "the route answers the stopped page (got $status)"
status="$(fetch -H 'Accept: application/json')"
[ "$status" = "503" ] && grep -q '"app_stopped"' "$WORK/body" && ok "an API client gets the JSON answer" || bad "an API client gets the JSON answer (got $status)"
wait_db 30 && ok "the bound database keeps running" || bad "the bound database keeps running"
if $KIP app restart "$APP" "${SCOPE[@]}" > "$WORK/restart.out" 2>&1; then bad "a restart of a stopped app is refused"; else ok "a restart of a stopped app is refused"; fi
if $KIP app scale "$APP" "${SCOPE[@]}" --replicas 3 > "$WORK/scale.out" 2>&1; then bad "scaling an autoscaled app is refused while stopped, as while running"; else ok "scaling an autoscaled app is refused while stopped, as while running"; fi

echo ""
echo "Case: an apply without the stop does not start the app"
manifest false ""
if $KIP apply -f "$WORK/kipper.yaml" > "$WORK/apply-clear.out" 2>&1; then bad "kip apply without the stop is refused"; else ok "kip apply without the stop is refused"; fi
wait_ready "0/2" 30 && ok "the app is still stopped" || bad "the app is still stopped"

# The API-key gate stands in for every gate here: it answers 401 without any
# credentials set up, which kip cannot do for basic auth.
echo ""
echo "Case: the route's gates answer before the stopped page"
manifest true "    stopped:
      reason: e2e stop"
$KIP apply -f "$WORK/kipper.yaml" > "$WORK/apply-gate.out" 2>&1 && ok "the API-key gate switched on" || { bad "the API-key gate switched on"; cat "$WORK/apply-gate.out"; }
sleep 10
status="$(fetch)"
[ "$status" = "401" ] && ! grep -q "This app is stopped" "$WORK/body" && ok "a visitor without a key gets 401, not the page" || bad "a visitor without a key gets 401, not the page (got $status)"
# Switching the gate off clears a field, which apply refuses without --force.
manifest false "    stopped:
      reason: e2e stop"
$KIP apply -f "$WORK/kipper.yaml" --force > "$WORK/apply-nogate.out" 2>&1 && ok "the API-key gate switched off" || bad "the API-key gate switched off"

echo ""
echo "Case: start"
$KIP app start "$APP" "${SCOPE[@]}" > "$WORK/start.out" 2>&1 && ok "kip app start" || { bad "kip app start"; cat "$WORK/start.out"; }
wait_ready "2/2" 600 && ok "back at the autoscaler's minimum of two" || bad "back at the autoscaler's minimum of two"
sleep 10
[ "$(fetch)" = "200" ] && ok "the route serves the app again" || bad "the route serves the app again"

echo ""
echo "Logs and responses are in $WORK"
echo "Remove the test app and its database with:"
echo "  $KIP app delete $APP ${SCOPE[*]}"
echo "  $KIP service delete db --delete-data ${SCOPE[*]}"
echo -e "Passed: ${GREEN}$pass${NC}  Failed: ${RED}$fail${NC}"
[ "$fail" -eq 0 ]
