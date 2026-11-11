#!/usr/bin/env bash
# Post-deploy readback (CHE-4582, CHE-2365, CHE-5175). Runs on the C00 runner
# after a successful deploy and releases it within WAIT_SECONDS. Fails, and so
# suspends automatic deploys, unless, within that wait:
#   - the public route and the router-selected upstream turn healthy
#     (ab-health-check.sh, read-only, one sample per try); and
#   - C00 then runs the deployed commit, or a descendant of it (a newer deploy
#     may have landed), and, when it runs exactly that commit, the live image
#     digests equal the admitted manifest's.
# The 15-minute watch is not here: post-deploy-confirm.sh judges it from history
# after an environment wait timer, so no runner is held while time passes.
# usage: post-deploy-readback.sh SOURCE_SHA WAIT_SECONDS [MANIFEST_JSON]
# Needs C00_COMPOSE_DIR, C00_STATE_DIR, C00_CUTOVER_STATE_DIR, C00_ROUTER_STATE_DIR.
# READBACK_RETRY_SECONDS (default 5) is the pause between health tries.
set -euo pipefail

sha=${1:?source sha is required}
window=${2:?wait seconds is required}
manifest=${3:-}
[[ "$sha" =~ ^[0-9a-f]{40}$ ]] || { echo "::error::source sha must be a full 40-hex commit SHA" >&2; exit 2; }
[[ "$window" =~ ^[0-9]+$ ]] || { echo "::error::window must be whole seconds" >&2; exit 2; }
: "${C00_ROUTER_STATE_DIR:?C00_ROUTER_STATE_DIR is required}"
: "${C00_CUTOVER_STATE_DIR:?C00_CUTOVER_STATE_DIR is required}"

# Wait for health, bounded: one sample per try, so the wait ends at the first
# healthy sample or at the deadline, whichever comes first.
retry="${READBACK_RETRY_SECONDS:-5}"
[[ "$retry" =~ ^[0-9]+$ ]] || { echo "::error::READBACK_RETRY_SECONDS must be whole seconds" >&2; exit 2; }
deadline=$((SECONDS + window))
until bash deploy/cd/ab-health-check.sh --router-state-dir "${C00_ROUTER_STATE_DIR%/}" \
  --cutover-state-dir "$C00_CUTOVER_STATE_DIR" --window 0; do
  if ((SECONDS + retry > deadline)); then
    echo "::error title=C00 readback::C00 not healthy within ${window}s" >&2
    exit 1
  fi
  sleep "$retry"
done

live="$(mktemp)"
trap 'rm -f "$live"' EXIT
bash deploy/cd/capture-remote-baseline.sh "$live"
field() {
  node -e 'const v=process.argv[2].split(".").reduce((a,k)=>a?.[k],JSON.parse(require("node:fs").readFileSync(process.argv[1],"utf8")));process.stdout.write(v==null?"":String(v))' "$1" "$2"
}
running="$(field "$live" application_sha)"
if [ "$running" != "$sha" ]; then
  if git merge-base --is-ancestor "$sha" "$running" 2>/dev/null; then
    echo "C00 runs ${running}, a descendant of the deployed ${sha}; digest check left to that deploy."
    exit 0
  fi
  echo "::error title=C00 readback::C00 runs ${running:-<unknown>}, not ${sha} or a descendant" >&2
  exit 1
fi
if [ -n "$manifest" ]; then
  for image in backend web; do
    want="$(field "$manifest" "image_digests.${image}")"
    have="$(field "$live" "images.${image}.digest")"
    if [ "$have" != "$want" ]; then
      echo "::error title=C00 readback::${image} digest ${have:-<none>} differs from the admitted ${want}" >&2
      exit 1
    fi
  done
fi
echo "C00 readback ok: ${running} healthy within ${window}s${manifest:+, image digests match the admitted manifest}."
