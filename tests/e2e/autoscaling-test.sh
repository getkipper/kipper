#!/usr/bin/env bash
# End-to-end test of app capacity: desired, minimum and maximum as guardrails,
# target tracking, switching off and on, stop and start, and the upgrade check.
# It uses kip alone. Run it against a test cluster that runs this branch's
# console images (see docs/en/contributing.md, "Testing a branch on a test
# cluster"), with KIP pointing at a kip built from the same commit.
set -uo pipefail

banned=kubectl
if grep -vE '^[[:space:]]*#' "$0" | grep -v 'banned=' | grep -qE "(^|[^a-z-])$banned"; then
  echo "This script calls $banned. The point of it is that kip is enough." >&2
  exit 2
fi

KIP="${KIP:-./kip/kip}"
APP=web
SCOPE=(--project autoscale-e2e --environment test)
WORK="$(mktemp -d "${TMPDIR:-.}/autoscaling.XXXXXX")"

RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'
pass=0
fail=0

ok() { echo -e "  ${GREEN}✔${NC}  $1"; pass=$((pass + 1)); }
bad() { echo -e "  ${RED}✗${NC}  $1"; fail=$((fail + 1)); }

# Renders the manifest: $1 replicas line (or empty), $2 enabled, $3 min, $4 max.
manifest() {
  REPLICAS="$1" ENABLED="$2" MIN="$3" MAX="$4" \
    envsubst < tests/e2e/autoscaling/kipper.yaml.tmpl > "$WORK/kipper.yaml"
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

status() { $KIP app autoscale "$APP" "${SCOPE[@]}" --status 2>&1; }

echo ""
echo "App capacity test"
echo ""

echo "Setup"
manifest "    replicas: 2" true 2 4
$KIP apply -f "$WORK/kipper.yaml" > "$WORK/apply.out" 2>&1 && ok "manifest applied" || { bad "manifest applied"; cat "$WORK/apply.out"; exit 1; }
wait_ready "2/2" 900 && ok "app running at its minimum of two" || { bad "app running at its minimum of two"; exit 1; }

echo ""
echo "Case: the policy is visible from kip"
sleep 20
status > "$WORK/status1.out"
grep -q "Autoscaling: on (CPU 80%)" "$WORK/status1.out" && ok "--status shows target tracking on" || { bad "--status shows target tracking on"; cat "$WORK/status1.out"; }
grep -q "(set by autoscaling)" "$WORK/status1.out" && grep -q "Min: 2   Max: 4" "$WORK/status1.out" && ok "desired is set by autoscaling within 2 to 4" || { bad "desired is set by autoscaling within 2 to 4"; cat "$WORK/status1.out"; }
! grep -q "not ready" "$WORK/status1.out" && ok "no autoscaling problem is reported" || { bad "no autoscaling problem is reported"; cat "$WORK/status1.out"; }

echo ""
echo "Case: a flag that is not given keeps the stored value"
$KIP app autoscale "$APP" "${SCOPE[@]}" --max 3 > "$WORK/max.out" 2>&1 && ok "kip app autoscale --max 3" || { bad "kip app autoscale --max 3"; cat "$WORK/max.out"; }
status > "$WORK/status2.out"
grep -q "Min: 2   Max: 3" "$WORK/status2.out" && grep -q "CPU 80%" "$WORK/status2.out" && ok "minimum and CPU target kept, maximum now 3" || { bad "minimum and CPU target kept, maximum now 3"; cat "$WORK/status2.out"; }

echo ""
echo "Case: a fixed count is refused while autoscaling is on"
if $KIP app scale "$APP" "${SCOPE[@]}" --replicas 3 > "$WORK/scale-on.out" 2>&1; then bad "kip app scale is refused while autoscaling is on"; else ok "kip app scale is refused while autoscaling is on"; fi

echo ""
echo "Case: switching off keeps the running count and the bounds"
$KIP app autoscale "$APP" "${SCOPE[@]}" --off > "$WORK/off.out" 2>&1 && ok "kip app autoscale --off" || { bad "kip app autoscale --off"; cat "$WORK/off.out"; }
grep -q "keeps running 2 replicas" "$WORK/off.out" && ok "it says the app keeps running 2 replicas" || { bad "it says the app keeps running 2 replicas"; cat "$WORK/off.out"; }
status > "$WORK/status3.out"
grep -q "Autoscaling: off" "$WORK/status3.out" && grep -q "Min: 2   Max: 3 (the bounds apply to the desired count)" "$WORK/status3.out" && ok "the bounds stay and apply to the desired count" || { bad "the bounds stay and apply to the desired count"; cat "$WORK/status3.out"; }
wait_ready "2/2" 120 && ok "still two pods" || bad "still two pods"

echo ""
echo "Case: the bounds hold for a fixed count"
if $KIP app scale "$APP" "${SCOPE[@]}" --replicas 5 > "$WORK/scale5.out" 2>&1; then bad "a count above the maximum is refused"; else grep -q "between 2 and 3" "$WORK/scale5.out" && ok "a count above the maximum is refused, naming the bounds" || { bad "a count above the maximum is refused, naming the bounds"; cat "$WORK/scale5.out"; }; fi
if $KIP app scale "$APP" "${SCOPE[@]}" --replicas 0 > "$WORK/scale0.out" 2>&1; then bad "zero is refused while bounds exist"; else ok "zero is refused while bounds exist"; fi
$KIP app scale "$APP" "${SCOPE[@]}" --replicas 3 > "$WORK/scale3.out" 2>&1 && ok "a count within the bounds is accepted" || { bad "a count within the bounds is accepted"; cat "$WORK/scale3.out"; }
wait_ready "3/3" 300 && ok "three pods" || bad "three pods"

echo ""
echo "Case: an omitted count stays within the manifest's bounds"
manifest "" false 2 3
$KIP apply -f "$WORK/kipper.yaml" > "$WORK/apply-omit.out" 2>&1 && ok "kip apply without replicas is accepted" || { bad "kip apply without replicas is accepted"; cat "$WORK/apply-omit.out"; }
wait_ready "3/3" 60 && ok "the stored count of three is kept" || bad "the stored count of three is kept"

echo ""
echo "Case: impossible bounds are refused before anything is written"
manifest "    replicas: 3" false 4 3
if $KIP apply -f "$WORK/kipper.yaml" > "$WORK/apply-bad.out" 2>&1; then bad "kip apply refuses min above max"; else grep -q "must not exceed maxReplicas" "$WORK/apply-bad.out" && ok "kip apply refuses min above max" || { bad "kip apply refuses min above max"; cat "$WORK/apply-bad.out"; }; fi

echo ""
echo "Case: stop and start with autoscaling on"
$KIP app autoscale "$APP" "${SCOPE[@]}" --cpu 80 > "$WORK/on.out" 2>&1 && ok "autoscaling switched back on" || { bad "autoscaling switched back on"; cat "$WORK/on.out"; }
$KIP app stop "$APP" "${SCOPE[@]}" --reason "e2e capacity" > "$WORK/stop.out" 2>&1 && ok "kip app stop" || { bad "kip app stop"; cat "$WORK/stop.out"; }
wait_ready "0/" 300 && ok "no pods while stopped" || bad "no pods while stopped"
$KIP app start "$APP" "${SCOPE[@]}" > "$WORK/start.out" 2>&1 && ok "kip app start" || { bad "kip app start"; cat "$WORK/start.out"; }
wait_ready "2/2" 600 && ok "started at the minimum of two" || bad "started at the minimum of two"

echo ""
echo "Case: removing the bounds frees the count"
$KIP app autoscale "$APP" "${SCOPE[@]}" --off > "$WORK/off2.out" 2>&1 && ok "switched off again" || { bad "switched off again"; cat "$WORK/off2.out"; }
$KIP app autoscale "$APP" "${SCOPE[@]}" --remove > "$WORK/remove.out" 2>&1 && ok "kip app autoscale --remove" || { bad "kip app autoscale --remove"; cat "$WORK/remove.out"; }
$KIP app scale "$APP" "${SCOPE[@]}" --replicas 1 > "$WORK/scale1.out" 2>&1 && ok "a count below the old minimum is accepted" || { bad "a count below the old minimum is accepted"; cat "$WORK/scale1.out"; }
wait_ready "1/1" 300 && ok "one pod" || bad "one pod"

echo ""
echo "Case: autoscaling starts an app that sits at zero"
$KIP app scale "$APP" "${SCOPE[@]}" --replicas 0 > "$WORK/scale-zero.out" 2>&1 && ok "scaled to zero without bounds" || { bad "scaled to zero without bounds"; cat "$WORK/scale-zero.out"; }
wait_ready "0/0" 300 && ok "no pods" || bad "no pods"
$KIP app autoscale "$APP" "${SCOPE[@]}" --min 2 --max 3 --cpu 80 > "$WORK/enable-zero.out" 2>&1 && ok "autoscaling switched on with a minimum of two" || { bad "autoscaling switched on with a minimum of two"; cat "$WORK/enable-zero.out"; }
grep -q "moved from 0 to 2" "$WORK/enable-zero.out" && ok "the stored count moved into the bounds" || { bad "the stored count moved into the bounds"; cat "$WORK/enable-zero.out"; }
wait_ready "2/2" 600 && ok "the app runs at its minimum" || bad "the app runs at its minimum"

echo ""
echo "Case: the upgrade check finds nothing to fix"
$KIP upgrade --check > "$WORK/check.out" 2>&1 && grep -q "nothing for the upgrade to fix" "$WORK/check.out" && ok "kip upgrade --check is clean" || { bad "kip upgrade --check is clean"; cat "$WORK/check.out"; }

echo ""
echo "Logs are in $WORK"
echo "Remove the test project with:"
echo "  $KIP project delete autoscale-e2e"
echo -e "Passed: ${GREEN}$pass${NC}  Failed: ${RED}$fail${NC}"
[ "$fail" -eq 0 ]
