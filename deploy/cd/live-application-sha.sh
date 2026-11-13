#!/usr/bin/env bash
# Print the commit C00's active colour was built from, read from the backend
# CONTAINER of that colour whether it is running or stopped (CHE-4582).
#
# capture-remote-baseline.sh needs running services (compose config-hash) and
# fails when the failed colour has stopped. A rollback must still run then, so
# this reads only identity: the image's revision label, else its tag resolved in
# this checkout's git history. Prints nothing when neither names a commit.
# Needs C00_COMPOSE_DIR and C00_CUTOVER_STATE_DIR; run from the repository root.
set -euo pipefail

: "${C00_COMPOSE_DIR:?C00_COMPOSE_DIR is required}"
: "${C00_CUTOVER_STATE_DIR:?C00_CUTOVER_STATE_DIR is required}"

colour="$(node -e 'const c=JSON.parse(require("node:fs").readFileSync(process.argv[1],"utf8")).active_colour;if(!/^(blue|green)$/.test(c))process.exit(1);process.stdout.write(c)' "${C00_CUTOVER_STATE_DIR%/}/cutover-state.json")"
compose=(docker compose -f docker-compose.selfhost.yml -f deploy/cd/docker-compose.ab.yml)

container="$(cd "$C00_COMPOSE_DIR" && "${compose[@]}" ps -aq "backend-${colour}" | head -n 1)"
sha=""
if [ -n "$container" ]; then
  sha="$(docker inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$container" 2>/dev/null | tr -d '[:space:]' || true)"
fi
if ! [[ "$sha" =~ ^[a-f0-9]{40}$ ]]; then
  tag="$( (cd "$C00_COMPOSE_DIR" && "${compose[@]}" images "backend-${colour}" --format json) 2>/dev/null \
    | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>{try{const r=JSON.parse(d||"[]");const x=Array.isArray(r)?r[0]:r;process.stdout.write(x&&x.Tag?String(x.Tag):"")}catch{}})' || true)"
  tag="${tag#sha-}"
  sha=""
  if [[ "$tag" =~ ^[a-f0-9]{7,40}$ ]]; then
    sha="$(git rev-parse --verify --quiet "${tag}^{commit}" 2>/dev/null || true)"
  fi
fi
[[ "$sha" =~ ^[a-f0-9]{40}$ ]] && printf '%s' "$sha"
exit 0
