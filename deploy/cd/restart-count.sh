#!/usr/bin/env bash
# Print the summed Docker restart count of C00's active backend and frontend
# containers (CHE-5175). Read-only; runs on the C00 runner. The readback job
# records the number, readback-confirm compares it 15 minutes later.
# Needs C00_COMPOSE_DIR and C00_CUTOVER_STATE_DIR.
set -euo pipefail

: "${C00_COMPOSE_DIR:?C00_COMPOSE_DIR is required}"
: "${C00_CUTOVER_STATE_DIR:?C00_CUTOVER_STATE_DIR is required}"

colour="$(node -e 'const c=JSON.parse(require("node:fs").readFileSync(process.argv[1],"utf8")).active_colour;if(!/^(blue|green)$/.test(c))process.exit(1);process.stdout.write(c)' "${C00_CUTOVER_STATE_DIR%/}/cutover-state.json")"
compose=(docker compose -f docker-compose.selfhost.yml -f deploy/cd/docker-compose.ab.yml)
ids="$(cd "$C00_COMPOSE_DIR" && "${compose[@]}" ps -q "backend-${colour}" "frontend-${colour}")"
[ -n "$ids" ] || { echo "::error title=C00 restart count::no running backend-${colour}/frontend-${colour} container" >&2; exit 1; }
total=0
for id in $ids; do
  count="$(docker inspect --format '{{.RestartCount}}' "$id")"
  [[ "$count" =~ ^[0-9]+$ ]] || { echo "::error title=C00 restart count::no restart count for container ${id}" >&2; exit 1; }
  total=$((total + count))
done
echo "$total"
