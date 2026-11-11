#!/usr/bin/env bash
# Post-deploy confirm (CHE-5175). Runs on the C00 runner about 15 minutes after
# the readback, so the runner is held for seconds, not for the window. It reads
# the window from history instead of watching it. Fails, and so suspends
# automatic deploys and starts the rollback, unless:
#   - C00 runs the deployed commit, or a descendant of it (a newer deploy landed;
#     the window then belongs to that deploy and is left to its own confirm);
#   - the live image digests equal the admitted manifest's;
#   - `docker events` since T0 shows no health_status: unhealthy, die, oom or
#     restart for the active colour's backend and frontend; and
#   - the restart count did not grow since the readback recorded it.
# usage: post-deploy-confirm.sh check SOURCE_SHA T0_EPOCH RESTART_BASELINE [MANIFEST_JSON]
# Needs C00_COMPOSE_DIR, C00_STATE_DIR, C00_CUTOVER_STATE_DIR.
set -euo pipefail

mode=${1:-}
[ "$mode" = check ] || { echo "usage: post-deploy-confirm.sh check SOURCE_SHA T0_EPOCH RESTART_BASELINE [MANIFEST_JSON]" >&2; exit 2; }
sha=${2:?source sha is required}
t0=${3:?T0 (epoch seconds) is required}
baseline=${4:?restart baseline is required}
manifest=${5:-}
[[ "$sha" =~ ^[0-9a-f]{40}$ ]] || { echo "::error::source sha must be a full 40-hex commit SHA" >&2; exit 2; }
[[ "$t0" =~ ^[0-9]+$ ]] || { echo "::error::T0 must be whole epoch seconds" >&2; exit 2; }
[[ "$baseline" =~ ^[0-9]+$ ]] || { echo "::error::restart baseline must be a whole number" >&2; exit 2; }
: "${C00_CUTOVER_STATE_DIR:?C00_CUTOVER_STATE_DIR is required}"

field() {
  node -e 'const v=process.argv[2].split(".").reduce((a,k)=>a?.[k],JSON.parse(require("node:fs").readFileSync(process.argv[1],"utf8")));process.stdout.write(v==null?"":String(v))' "$1" "$2"
}
live="$(mktemp)"
trap 'rm -f "$live"' EXIT
bash deploy/cd/capture-remote-baseline.sh "$live"
running="$(field "$live" application_sha)"
if [ "$running" != "$sha" ]; then
  if git merge-base --is-ancestor "$sha" "$running" 2>/dev/null; then
    echo "C00 runs ${running}, a descendant of the deployed ${sha}; the window belongs to that deploy."
    exit 0
  fi
  echo "::error title=C00 confirm::C00 runs ${running:-<unknown>}, not ${sha} or a descendant" >&2
  exit 1
fi
if [ -n "$manifest" ]; then
  for image in backend web; do
    want="$(field "$manifest" "image_digests.${image}")"
    have="$(field "$live" "images.${image}.digest")"
    if [ "$have" != "$want" ]; then
      echo "::error title=C00 confirm::${image} digest ${have:-<none>} differs from the admitted ${want}" >&2
      exit 1
    fi
  done
fi

colour="$(field "${C00_CUTOVER_STATE_DIR%/}/cutover-state.json" active_colour)"
[[ "$colour" =~ ^(blue|green)$ ]] || { echo "::error title=C00 confirm::no active colour in the cutover state" >&2; exit 1; }
until_epoch="$(date +%s)"
# Only the active colour: the other one drains after a cutover and may die by design.
bad="$(docker events --since "$t0" --until "$until_epoch" --filter type=container --format '{{json .}}' |
  node -e '
    const colour = process.argv[1];
    const watched = new Set([`backend-${colour}`, `frontend-${colour}`]);
    const bad = ["health_status: unhealthy", "die", "oom", "restart"];
    const hits = require("node:fs").readFileSync(0, "utf8").split("\n").filter(Boolean).flatMap((line) => {
      const e = JSON.parse(line);
      const service = e.Actor?.Attributes?.["com.docker.compose.service"];
      return watched.has(service) && bad.includes(e.Action) ? [`${service}: ${e.Action}`] : [];
    });
    process.stdout.write([...new Set(hits)].join("\n"));
  ' "$colour")"
if [ -n "$bad" ]; then
  echo "::error title=C00 confirm::container events between T0 ${t0} and ${until_epoch}:" >&2
  echo "$bad" >&2
  exit 1
fi
now_restarts="$(bash deploy/cd/restart-count.sh)"
if [ "$now_restarts" -gt "$baseline" ]; then
  echo "::error title=C00 confirm::restart count grew from ${baseline} to ${now_restarts}" >&2
  exit 1
fi
echo "C00 confirm ok: ${running} quiet from ${t0} to ${until_epoch}, restarts ${now_restarts}${manifest:+, image digests match the admitted manifest}."
