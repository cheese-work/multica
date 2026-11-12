#!/usr/bin/env bash
# Post-deploy confirm (CHE-5175). Runs on the C00 runner about 15 minutes after
# the readback, so the runner is held for seconds, not for the window. It does not
# watch the window: the native C00 health observer (health-window.mjs, run by a
# systemd timer, armed by the readback) sampled it every 10 s into a durable
# journal, and this job verifies that journal. Fails, and so suspends automatic
# deploys and starts the rollback, unless:
#   - the 15-minute window has elapsed since T0 (CONFIRM_WINDOW_SECONDS, default
#     900), so an early or mis-timed run cannot pass an unobserved window;
#   - C00 runs the deployed commit, or a descendant of it (a newer deploy landed;
#     the window then belongs to that deploy's own journal and confirm, and is not
#     judged here);
#   - the live image digests equal the admitted manifest's;
#   - the journal of OBSERVATION_ID is complete and healthy: one passing sample per
#     10 s slot from T0 to T0+900 including the last, taken by the timer, with no
#     failed, missed, truncated or replaced-container sample (a failed sample stays
#     failed after recovery, so a no-traffic outage or a /health 500 cannot hide);
#   - the public route and the router-selected upstream answer now
#     (ab-health-check.sh, one read-only sample);
#   - `docker events` since T0 shows no health_status: unhealthy, die, oom or
#     restart for the active colour's backend and frontend; and
#   - the restart count did not grow since the readback recorded it.
# usage: post-deploy-confirm.sh check SOURCE_SHA T0_EPOCH RESTART_BASELINE OBSERVATION_ID [MANIFEST_JSON]
# Needs C00_COMPOSE_DIR, C00_STATE_DIR, C00_CUTOVER_STATE_DIR, C00_ROUTER_STATE_DIR.
set -euo pipefail

mode=${1:-}
[ "$mode" = check ] || { echo "usage: post-deploy-confirm.sh check SOURCE_SHA T0_EPOCH RESTART_BASELINE OBSERVATION_ID [MANIFEST_JSON]" >&2; exit 2; }
sha=${2:?source sha is required}
t0=${3:?T0 (epoch seconds) is required}
baseline=${4:?restart baseline is required}
observation=${5:?observation id is required}
manifest=${6:-}
[[ "$sha" =~ ^[0-9a-f]{40}$ ]] || { echo "::error::source sha must be a full 40-hex commit SHA" >&2; exit 2; }
[[ "$t0" =~ ^[0-9]+$ ]] || { echo "::error::T0 must be whole epoch seconds" >&2; exit 2; }
[[ "$observation" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ ]] || { echo "::error::observation id must be 1-64 letters, digits, dot, dash or underscore" >&2; exit 2; }
[[ "$baseline" =~ ^[0-9]+$ ]] || { echo "::error::restart baseline must be a whole number" >&2; exit 2; }
: "${C00_CUTOVER_STATE_DIR:?C00_CUTOVER_STATE_DIR is required}"
: "${C00_ROUTER_STATE_DIR:?C00_ROUTER_STATE_DIR is required}"
: "${C00_STATE_DIR:?C00_STATE_DIR is required}"
window="${CONFIRM_WINDOW_SECONDS:-900}"
[[ "$window" =~ ^[0-9]+$ ]] || { echo "::error::CONFIRM_WINDOW_SECONDS must be whole seconds" >&2; exit 2; }
elapsed=$(($(date +%s) - t0))
if [ "$elapsed" -lt "$window" ]; then
  echo "::error title=C00 confirm::the ${window}s window after T0 ${t0} has not elapsed (${elapsed}s)" >&2
  exit 1
fi

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

# The window: every 10 s slot sampled by the native timer, none failed. Verified after the
# descendant check so a newer deploy is never rolled back for an older window.
node deploy/cd/health-window.mjs verify --dir "${C00_STATE_DIR%/}/health-observer" --id "$observation" --sha "$sha" || {
  echo "::error title=C00 confirm::the health journal ${observation} does not show a complete healthy window" >&2
  exit 1
}

# Final HTTP sample: the route answers now. Containers define no
# healthcheck, so events alone cannot show a hung application or a failed router.
bash deploy/cd/ab-health-check.sh --router-state-dir "${C00_ROUTER_STATE_DIR%/}" \
  --cutover-state-dir "$C00_CUTOVER_STATE_DIR" --window 0 || {
  echo "::error title=C00 confirm::route health check failed at confirmation" >&2
  exit 1
}

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
echo "C00 confirm ok: ${running} journal ${observation} healthy and events quiet from ${t0} to ${until_epoch}, restarts ${now_restarts}${manifest:+, image digests match the admitted manifest}."
