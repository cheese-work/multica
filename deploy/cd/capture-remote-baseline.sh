#!/usr/bin/env bash
# Capture C00's live baseline tuple on the local C00 runner (CHE-1923).
#
# The deploy runner IS the target host, so nothing crosses a network boundary:
# the runner reads C00's router state, Compose services and image labels
# directly. Used for both the pre-admission baseline and the live baseline the
# deploy step re-reads before it mutates anything (CHE-2146).
set -euo pipefail

output=${1:?baseline output path is required}
: "${C00_COMPOSE_DIR:?C00_COMPOSE_DIR is required}"
: "${C00_STATE_DIR:?C00_STATE_DIR is required}"
: "${C00_CUTOVER_STATE_DIR:?C00_CUTOVER_STATE_DIR is required}"

remote_dir="${C00_STATE_DIR%/}/cd-baseline-$(date +%s)-$$"
trap 'rm -rf "$remote_dir" || true' EXIT
mkdir -p "$remote_dir"
cp deploy/cd/capture-tuple.sh deploy/cd/tuple-snapshot.mjs "$remote_dir/"

# The live C00 stack is A/B: the base backend/frontend services are stopped and
# the router points at one colour-specific pair. Read the router's active
# generation and ask capture-tuple.sh for that exact Compose service pair;
# inspecting the stopped base service yields no container id and therefore no
# config-hash.
active_colour="$(node -e 'const fs=require("node:fs"); const p=process.argv[1]; const c=JSON.parse(fs.readFileSync(p,"utf8")).active_colour; if(!/^(blue|green)$/.test(c)) process.exit(1); process.stdout.write(c)' "${C00_CUTOVER_STATE_DIR%/}/cutover-state.json" | tr -d '[:space:]')"
[[ "$active_colour" =~ ^(blue|green)$ ]]
echo "C00 router reports active colour ${active_colour}; capturing backend-${active_colour}/frontend-${active_colour}"

# Resolve the deployed commit HERE, on the local runner, and pass it in only
# when the image cannot speak for itself. Precedence, highest first:
#   1. the image's own org.opencontainers.image.revision label (what the image
#      was actually built from, recorded at build time by D1)
#   2. tag-based git resolution (C00 may still run images not built by D1,
#      which carry only a short SHA tag)
# Passing --application-sha would override the label, so it is passed only when
# the label is absent. Never substitute this workflow's own commit: that would
# record a baseline claiming C00 runs something it does not.
baseline_sha=""
compose=(docker compose -f docker-compose.selfhost.yml -f deploy/cd/docker-compose.ab.yml)
deployed_label="$(docker inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$(cd "$C00_COMPOSE_DIR" && "${compose[@]}" ps -q "backend-${active_colour}")" 2>/dev/null | tr -d '[:space:]' || true)"

if [[ "$deployed_label" =~ ^[a-f0-9]{40}$ ]]; then
  echo "C00's backend image carries revision label ${deployed_label}; deferring to it (no --application-sha)"
else
  deployed_tag="$( (cd "$C00_COMPOSE_DIR" && "${compose[@]}" images "backend-${active_colour}" --format json) 2>/dev/null \
    | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>{try{const r=JSON.parse(d||"[]");const x=Array.isArray(r)?r[0]:r;process.stdout.write(x&&x.Tag?String(x.Tag):"")}catch{process.stdout.write("")}})' || true)"
  candidate="${deployed_tag#sha-}"
  if [[ "$candidate" =~ ^[a-f0-9]{7,40}$ ]]; then
    baseline_sha="$(git rev-parse --verify --quiet "${candidate}^{commit}" 2>/dev/null || true)"
  fi
fi

capture_args=(--compose-dir "$C00_COMPOSE_DIR" --output "${remote_dir}/baseline-tuple.json" --compose-service-suffix "$active_colour")
if [[ "$baseline_sha" =~ ^[a-f0-9]{40}$ ]]; then
  echo "no usable revision label; resolved C00's deployed image tag '${deployed_tag}' to commit ${baseline_sha}"
  capture_args+=(--application-sha "$baseline_sha")
elif ! [[ "$deployed_label" =~ ^[a-f0-9]{40}$ ]]; then
  echo "could not resolve C00's deployed tag '${deployed_tag:-<none>}' to a commit; capture-tuple.sh will fail loudly with its own diagnostic"
fi

bash "${remote_dir}/capture-tuple.sh" "${capture_args[@]}"
cp "${remote_dir}/baseline-tuple.json" "$output"
