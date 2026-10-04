#!/usr/bin/env bash
# Drives an autoscaled app to its maximum with load from in-cluster jobs, holds
# it there long enough to look at the console's "At maximum" badge, then stops
# the load and waits for the scale-in. It uses kip alone; run it against a test
# cluster that runs this branch's console images.
set -uo pipefail

banned=kubectl
if grep -vE '^[[:space:]]*#' "$0" | grep -v 'banned=' | grep -qE "(^|[^a-z-])$banned"; then
  echo "This script calls $banned. The point of it is that kip is enough." >&2
  exit 2
fi

KIP="${KIP:-./kip/kip}"
PROJECT="${PROJECT:-autoscale-load}"
APP=web
SCOPE=(--project "$PROJECT" --environment test)
LOADERS="${LOADERS:-4}"
HOLD="${HOLD:-180}"
WORK="$(mktemp -d "${TMPDIR:-.}/autoscaling-load.XXXXXX")"

RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'
pass=0
fail=0
ok() { echo -e "  ${GREEN}✔${NC}  $1"; pass=$((pass + 1)); }
bad() { echo -e "  ${RED}✗${NC}  $1"; fail=$((fail + 1)); }
note() { echo "  $(date +%H:%M:%S)  $1"; }

wait_ready() {
  local want="$1" deadline=$((SECONDS + ${2:-600}))
  while [ $SECONDS -lt $deadline ]; do
    $KIP app list "${SCOPE[@]}" 2>&1 | grep -qE "^  $APP .* $want" && return 0
    sleep 10
  done
  return 1
}

# kip job run splits --command on whitespace, so the loop uses ${IFS} for its
# spaces and stays one argument to sh -c.
load_command() {
  echo 'sh -c while(true);do(wget${IFS}-q${IFS}-O${IFS}/dev/null${IFS}http://'"$APP"'/)>/dev/null${IFS}2>&1;done'
}

# kip job run names each run after the job with a timestamp suffix, so the
# runs are deleted by the names kip job list reports.
stop_load() {
  for j in $($KIP job list "${SCOPE[@]}" 2>/dev/null | awk 'NR>1 && $1 ~ /^load-/ {print $1}'); do
    $KIP job delete "$j" "${SCOPE[@]}" > /dev/null 2>&1 || true
  done
}
trap stop_load EXIT

echo ""
echo "Load to the maximum"
echo ""
APPS="  $APP:
    image: nginx:1.27-alpine
    port: 80
    resources:
      profile: standard
    autoscale:
      enabled: true
      minReplicas: 1
      maxReplicas: 3
      cpuTarget: 10" PROJECT="$PROJECT" envsubst < tests/e2e/autoscaling/load.yaml.tmpl > "$WORK/kipper.yaml"
$KIP apply -f "$WORK/kipper.yaml" > "$WORK/apply.out" 2>&1 && ok "app applied with autoscaling 1 to 3 at 10% CPU" || { bad "app applied"; cat "$WORK/apply.out"; exit 1; }
wait_ready "1/1" 600 && ok "running at its minimum of one" || { bad "running at its minimum of one"; exit 1; }

for i in $(seq 1 "$LOADERS"); do
  $KIP job run --name "load-$i" --image busybox:1.36 --command "$(load_command)" "${SCOPE[@]}" > "$WORK/job-$i.out" 2>&1 || { bad "load job $i started"; cat "$WORK/job-$i.out"; }
done
ok "$LOADERS load jobs started"
note "load running; watching for the scale-out"

deadline=$((SECONDS + 900))
reached=0
while [ $SECONDS -lt $deadline ]; do
  line="$($KIP app list "${SCOPE[@]}" 2>&1 | grep -E "^  $APP ")"
  cpu="$($KIP app autoscale "$APP" "${SCOPE[@]}" --status 2>&1 | grep -o 'cpu: target 10%, current [0-9a-z%]*' | sed 's/.*current //')"
  note "ready ${line##* }, cpu $cpu"
  if echo "$line" | grep -qE " 3/3$"; then reached=1; break; fi
  sleep 20
done
[ "$reached" = 1 ] && ok "scaled out to the maximum of three" || bad "scaled out to the maximum of three within 15 minutes"

$KIP app autoscale "$APP" "${SCOPE[@]}" --status > "$WORK/status-max.out" 2>&1
grep -q "Desired: 3 (set by autoscaling)" "$WORK/status-max.out" && ok "--status shows desired 3, set by autoscaling" || { bad "--status shows desired 3, set by autoscaling"; cat "$WORK/status-max.out"; }
note "holding at the maximum for ${HOLD}s: open the app's Scale tab now and look for the At maximum badge"
held=1
end=$((SECONDS + HOLD))
while [ $SECONDS -lt $end ]; do
  $KIP app list "${SCOPE[@]}" 2>&1 | grep -qE "^  $APP .* 3/3$" || held=0
  sleep 20
done
[ "$held" = 1 ] && ok "stayed at three, never above the maximum" || bad "stayed at three, never above the maximum"

stop_load
if $KIP job list "${SCOPE[@]}" 2>&1 | awk 'NR>1 && $1 ~ /^load-/' | grep -q .; then bad "every load job deleted"; else ok "every load job deleted"; fi
note "waiting for the scale-in (the autoscaler waits five minutes before scaling down)"
wait_ready "1/1" 900 && ok "scaled back in to the minimum of one" || bad "scaled back in to the minimum of one within 15 minutes"

echo ""
echo "Logs are in $WORK"
echo "Remove the test project with:"
echo "  $KIP project delete $PROJECT"
echo -e "Passed: ${GREEN}$pass${NC}  Failed: ${RED}$fail${NC}"
[ "$fail" -eq 0 ]
