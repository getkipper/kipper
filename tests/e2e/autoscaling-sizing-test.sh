#!/usr/bin/env bash
# The one-hour sizing observation: two apps with automatic sizing, one tracking
# CPU and one tracking memory, under steady load for DURATION seconds. The
# targets are set low so both autoscalers act: the CPU-tracked app settles
# between its bounds, and the memory-tracked app climbs to its maximum, because
# memory per pod does not fall as pods are added. Every
# minute it records each app's ready count and current utilisation, and at the
# end it counts how often the replica count changed direction. Compare each
# app's requests in the console's Resources tab at the start and the end: the
# CPU-tracked app must keep its CPU request, the memory-tracked app its memory
# request. It uses kip alone.
set -uo pipefail

banned=kubectl
if grep -vE '^[[:space:]]*#' "$0" | grep -v 'banned=' | grep -qE "(^|[^a-z-])$banned"; then
  echo "This script calls $banned. The point of it is that kip is enough." >&2
  exit 2
fi

KIP="${KIP:-./kip/kip}"
PROJECT="${PROJECT:-autoscale-sizing}"
SCOPE=(--project "$PROJECT" --environment test)
DURATION="${DURATION:-3600}"
WORK="$(mktemp -d "${TMPDIR:-.}/autoscaling-sizing.XXXXXX")"
SAMPLES="$WORK/samples.csv"

RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'
pass=0
fail=0
ok() { echo -e "  ${GREEN}✔${NC}  $1"; pass=$((pass + 1)); }
bad() { echo -e "  ${RED}✗${NC}  $1"; fail=$((fail + 1)); }
note() { echo "  $(date +%H:%M:%S)  $1"; }

ready_of() { $KIP app list "${SCOPE[@]}" 2>&1 | grep -E "^  $1 " | awk '{print $NF}'; }
metric_of() { $KIP app autoscale "$1" "${SCOPE[@]}" --status 2>&1 | grep -o "$2: target [0-9]*%, current [0-9a-z%]*" | sed 's/.*current //'; }

# kip job run splits --command on whitespace, so the loop uses ${IFS} for its
# spaces and stays one argument to sh -c. The pause keeps the load moderate.
load_command() {
  echo 'sh -c while(true);do(wget${IFS}-q${IFS}-O${IFS}/dev/null${IFS}http://'"$1"'/)>/dev/null${IFS}2>&1;usleep${IFS}20000;done'
}

# kip job run names each run after the job with a timestamp suffix, so the
# runs are deleted by the names kip job list reports.
stop_load() {
  for j in $($KIP job list "${SCOPE[@]}" 2>/dev/null | awk 'NR>1 && $1 ~ /^load-/ {print $1}'); do
    $KIP job delete "$j" "${SCOPE[@]}" > /dev/null 2>&1 || true
  done
}
trap stop_load EXIT

# Counts how often a series of ready counts changes direction.
reversals() {
  awk -F, -v col="$1" 'NR>1 { split($col, a, "/"); n=a[2]+0;
    if (seen && n != last) { d = (n > last) ? 1 : -1; if (dir && d != dir) r++; dir = d }
    last = n; seen = 1 } END { print r+0 }' "$SAMPLES"
}

echo ""
echo "One-hour sizing observation (${DURATION}s)"
echo ""
APPS="  cpu-app:
    image: nginx:1.27-alpine
    port: 80
    autoscale:
      enabled: true
      minReplicas: 1
      maxReplicas: 5
      cpuTarget: 10
  mem-app:
    image: nginx:1.27-alpine
    port: 80
    autoscale:
      enabled: true
      minReplicas: 1
      maxReplicas: 5
      memoryTarget: 2" PROJECT="$PROJECT" envsubst < tests/e2e/autoscaling/load.yaml.tmpl > "$WORK/kipper.yaml"
$KIP apply -f "$WORK/kipper.yaml" > "$WORK/apply.out" 2>&1 && ok "two apps applied with automatic sizing" || { bad "two apps applied"; cat "$WORK/apply.out"; exit 1; }
deadline=$((SECONDS + 600))
until [ "$(ready_of cpu-app)" = "1/1" ] && [ "$(ready_of mem-app)" = "1/1" ]; do
  [ $SECONDS -gt $deadline ] && { bad "both apps running"; exit 1; }
  sleep 10
done
ok "both apps running"
note "note each app's CPU and memory requests in the console's Resources tab now"

$KIP job run --name load-cpu-1 --image busybox:1.36 --command "$(load_command cpu-app)" "${SCOPE[@]}" > /dev/null 2>&1
$KIP job run --name load-cpu-2 --image busybox:1.36 --command "$(load_command cpu-app)" "${SCOPE[@]}" > /dev/null 2>&1
$KIP job run --name load-mem-1 --image busybox:1.36 --command "$(load_command mem-app)" "${SCOPE[@]}" > /dev/null 2>&1
$KIP job run --name load-mem-2 --image busybox:1.36 --command "$(load_command mem-app)" "${SCOPE[@]}" > /dev/null 2>&1
ok "steady load started on both apps"

echo "time,cpu_ready,cpu_util,mem_ready,mem_util" > "$SAMPLES"
end=$((SECONDS + DURATION))
while [ $SECONDS -lt $end ]; do
  row="$(date +%H:%M:%S),$(ready_of cpu-app),$(metric_of cpu-app cpu),$(ready_of mem-app),$(metric_of mem-app memory)"
  echo "$row" >> "$SAMPLES"
  note "$row"
  sleep 60
done
stop_load
ok "load stopped after ${DURATION}s"

cpu_rev="$(reversals 2)"
mem_rev="$(reversals 4)"
[ "$cpu_rev" -le 2 ] && ok "the CPU-tracked app's replica count changed direction $cpu_rev times" || bad "the CPU-tracked app's replica count changed direction $cpu_rev times (oscillation)"
[ "$mem_rev" -le 2 ] && ok "the memory-tracked app's replica count changed direction $mem_rev times" || bad "the memory-tracked app's replica count changed direction $mem_rev times (oscillation)"
note "compare each app's requests in the Resources tab with the start: the CPU-tracked app's CPU request and the memory-tracked app's memory request must be unchanged"
note "the console bell should hold scale entries for both apps and one 'autoscaling at maximum' warning for mem-app"

echo ""
echo "Samples are in $SAMPLES"
echo "Remove the test project with:"
echo "  $KIP project delete $PROJECT"
echo -e "Passed: ${GREEN}$pass${NC}  Failed: ${RED}$fail${NC}"
[ "$fail" -eq 0 ]
