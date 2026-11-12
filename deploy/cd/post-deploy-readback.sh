#!/usr/bin/env bash
# Post-deploy readback (CHE-4582, CHE-2365, CHE-5175). Runs on the C00 runner
# after a successful deploy and releases it within WAIT_SECONDS. Fails, and so
# suspends automatic deploys, unless, within that wait:
#   - the public route and the router-selected upstream turn healthy
#     (ab-health-check.sh, read-only, one sample per try); and
#   - C00 runs the deployed commit, or a descendant of it (a newer deploy may
#     have landed; then this readback leaves the journal and the digests to
#     that deploy and passes without arming); and
#   - when C00 runs exactly the deployed commit, the native health observer is
#     armed for the whole 15-minute window and its timer has sampled once
#     (health-window.mjs arm, which re-checks the live commit under its lock;
#     install-health-observer.sh installs the timer on C00); and
#   - the live image digests equal the admitted manifest's.
# The 15-minute watch is not here: the observer's timer samples C00 on the host
# and post-deploy-confirm.sh verifies its journal after an environment wait
# timer, so no runner is held while time passes.
# usage: post-deploy-readback.sh SOURCE_SHA WAIT_SECONDS [MANIFEST_JSON]
# Needs C00_COMPOSE_DIR, C00_STATE_DIR, C00_CUTOVER_STATE_DIR, C00_ROUTER_STATE_DIR
# and OBSERVATION_ID (names this deploy's journal). On success the arm step writes
# observation and t0 to GITHUB_OUTPUT; a descendant writes superseded=true instead.
# READBACK_RETRY_SECONDS (default 5) is the pause between health tries;
# READBACK_ARM_VERIFY_SECONDS (default 30) bounds the wait for the timer's first sample.
set -euo pipefail

sha=${1:?source sha is required}
window=${2:?wait seconds is required}
manifest=${3:-}
[[ "$sha" =~ ^[0-9a-f]{40}$ ]] || { echo "::error::source sha must be a full 40-hex commit SHA" >&2; exit 2; }
[[ "$window" =~ ^[0-9]+$ ]] || { echo "::error::window must be whole seconds" >&2; exit 2; }
: "${C00_ROUTER_STATE_DIR:?C00_ROUTER_STATE_DIR is required}"
: "${C00_CUTOVER_STATE_DIR:?C00_CUTOVER_STATE_DIR is required}"
: "${C00_COMPOSE_DIR:?C00_COMPOSE_DIR is required}"
: "${C00_STATE_DIR:?C00_STATE_DIR is required}"
: "${OBSERVATION_ID:?OBSERVATION_ID is required}"

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
field() {
  node -e 'const v=process.argv[2].split(".").reduce((a,k)=>a?.[k],JSON.parse(require("node:fs").readFileSync(process.argv[1],"utf8")));process.stdout.write(v==null?"":String(v))' "$1" "$2"
}
# Read the live commit, then decide: a newer deploy that already landed owns the
# journal, so this readback must not arm (a delayed older readback would
# otherwise supersede the newer deploy's journal). Returns 0 and prints nothing
# when C00 runs exactly $sha; exits 0 for a descendant, 1 for anything else.
read_live() {
  bash deploy/cd/capture-remote-baseline.sh "$live"
  running="$(field "$live" application_sha)"
  [ "$running" = "$sha" ] && return 0
  if git merge-base --is-ancestor "$sha" "$running" 2>/dev/null; then
    echo "C00 runs ${running}, a descendant of the deployed ${sha}; its own deploy arms the journal and checks the digests."
    # No journal was armed for this run: tell the workflow to skip the delay and the confirm.
    [ -z "${GITHUB_OUTPUT:-}" ] || echo "superseded=true" >> "$GITHUB_OUTPUT"
    exit 0
  fi
  echo "::error title=C00 readback::C00 runs ${running:-<unknown>}, not ${sha} or a descendant" >&2
  exit 1
}
read_live

# Arm only while C00 still runs $sha. health-window.mjs re-checks that under its
# lock, in the same step that switches the journal (exit 3: a newer deploy landed
# since the read above).
arm_status=0
node deploy/cd/health-window.mjs arm --dir "${C00_STATE_DIR%/}/health-observer" --id "$OBSERVATION_ID" --sha "$sha" \
  --window 900 --interval 10 --verify-seconds "${READBACK_ARM_VERIFY_SECONDS:-30}" \
  --compose-dir "$C00_COMPOSE_DIR" --cutover-state-dir "$C00_CUTOVER_STATE_DIR" \
  --router-state-dir "${C00_ROUTER_STATE_DIR%/}" --health-script deploy/cd/ab-health-check.sh \
  --live-sha-script deploy/cd/live-application-sha.sh || arm_status=$?
if [ "$arm_status" -eq 3 ]; then
  read_live  # a descendant exits 0 here; anything else exits 1
  echo "::error title=C00 readback::C00 changed during arming but runs ${running} again; refusing to pass" >&2
  exit 1
elif [ "$arm_status" -ne 0 ]; then
  echo "::error title=C00 readback::the health observer is not armed and sampling (see health-window output above)" >&2
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
