#!/usr/bin/env bash
set -euo pipefail

# Bounded A/B health alarm (CHE-773). Runs ON C00 — after a cutover (CD's
# post-deploy observation window) or from an operator's timer. Read-only:
# never starts, stops, or reroutes anything; it only reports. Automatic
# failover is deliberately absent — restarting a colour needs the schema
# compatibility proof cutover.sh's recovery performs, and running both
# backends at once is never safe (docker-compose.ab.yml).
#
# Checks, each sample:
#   - the router's active generation names a colour and its ports;
#   - that colour's backend answers /readyz on the port the router forwards to;
#   - the public listeners (router :8081 /health, :3000 /) answer 2xx.
# Healthy = every sample in --window passes. Any failing sample prints one
# `::error` line per failure (a GitHub annotation under CD, plain text on a
# terminal) plus the A/B state an operator needs, and exits 1.

usage() {
  echo "usage: ab-health-check.sh --router-state-dir PATH [--cutover-state-dir PATH] [--window SECONDS] [--interval SECONDS]" >&2
}

router_state_dir=""
cutover_state_dir=""
window=0
interval=10
while (($#)); do
  case "$1" in
    --router-state-dir) router_state_dir=${2:?}; shift 2 ;;
    --cutover-state-dir) cutover_state_dir=${2:?}; shift 2 ;;
    --window) window=${2:?}; shift 2 ;;
    --interval) interval=${2:?}; shift 2 ;;
    *) usage; exit 2 ;;
  esac
done
[ -n "$router_state_dir" ] || { usage; exit 2; }

field() {
  node -e 'const v=JSON.parse(require("fs").readFileSync(process.argv[1],"utf8"))[process.argv[2]];process.stdout.write(v==null?"":String(v))' "$1" "$2" 2>/dev/null
}

http_code() {
  curl --silent --output /dev/null --max-time 5 --write-out '%{http_code}' "$1" 2>/dev/null || true
}

sample() {
  local json="$router_state_dir/active.json" colour backend_port code failed=0
  colour="$(field "$json" colour)"
  backend_port="$(field "$json" backend_port)"
  if [ -z "$colour" ] || [ -z "$backend_port" ]; then
    echo "::error title=Multica A/B health::router has no readable active generation at $json"
    return 1
  fi
  code="$(http_code "http://127.0.0.1:${backend_port}/readyz")"
  if [ "$code" != 200 ]; then
    echo "::error title=Multica A/B health::router-selected upstream backend-$colour on :$backend_port /readyz returned ${code:-000}"
    failed=1
  fi
  code="$(http_code "http://127.0.0.1:8081/health")"
  if [[ "$code" != 2* ]]; then
    echo "::error title=Multica A/B health::public API (router :8081 /health) returned ${code:-000}"
    failed=1
  fi
  code="$(http_code "http://127.0.0.1:3000/")"
  if [[ "$code" != 2* ]]; then
    echo "::error title=Multica A/B health::public web (router :3000 /) returned ${code:-000}"
    failed=1
  fi
  return "$failed"
}

report_state() {
  echo "router active generation: $(cat "$router_state_dir/active.json" 2>/dev/null || echo '<missing>')"
  if [ -n "$cutover_state_dir" ]; then
    echo "cutover-state.json: $(cat "$cutover_state_dir/cutover-state.json" 2>/dev/null || echo '<missing>')"
    [ -f "$cutover_state_dir/outage-alert.json" ] && echo "last cutover outage alert: $(cat "$cutover_state_dir/outage-alert.json")"
  fi
  echo "runbook: deploy/cd/README.md, \"A/B outage runbook\""
}

elapsed=0
while :; do
  if ! sample; then
    report_state
    exit 1
  fi
  [ "$elapsed" -ge "$window" ] && break
  sleep "$interval"
  elapsed=$((elapsed + interval))
done
echo "==> A/B health ok: $(field "$router_state_dir/active.json" colour) served for ${window}s"
