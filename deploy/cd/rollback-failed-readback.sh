#!/usr/bin/env bash
# Roll C00 back to the retained predecessor after a failed readback (CHE-4582,
# CHE-2365: "on failure roll back and suspend"). The suspension is separate: the
# failed readback job already suspends automatic deploys.
#
# Runs cutover.sh `rollback` from this checkout (the admitted ref, the same
# controller the deploy ran): drain the failing colour, quiescence gate, check
# the ledger against minimum_rollback_version, start the predecessor without
# migrations, restore its router generation. Never a down migration or restore.
# Skips, with exit 0, when C00 no longer runs SOURCE_SHA: a newer deploy owns it.
# Fails, without rolling back, when the live commit cannot be identified.
# usage: rollback-failed-readback.sh SOURCE_SHA   (runs inside the cd-deploy-c00 lock)
# Needs C00_COMPOSE_DIR, C00_CUTOVER_STATE_DIR, C00_ROUTER_STATE_DIR, GHCR_PULL_TOKEN.
set -euo pipefail

sha=${1:?source sha is required}
[[ "$sha" =~ ^[0-9a-f]{40}$ ]] || { echo "::error::source sha must be a full 40-hex commit SHA" >&2; exit 2; }
: "${C00_COMPOSE_DIR:?C00_COMPOSE_DIR is required}"
: "${C00_CUTOVER_STATE_DIR:?C00_CUTOVER_STATE_DIR is required}"
: "${C00_ROUTER_STATE_DIR:?C00_ROUTER_STATE_DIR is required}"
case "$C00_ROUTER_STATE_DIR" in
  /*) : ;;
  *) echo "::error::C00_ROUTER_STATE_DIR must be an absolute path" >&2; exit 1 ;;
esac
router_state_dir="${C00_ROUTER_STATE_DIR%/}"

# Identity only: the failed colour may be stopped, and the full baseline capture
# needs running services. A commit that cannot be identified is not rolled back.
running="$(bash deploy/cd/live-application-sha.sh)"
if [ -z "$running" ]; then
  echo "::error::cannot identify the commit C00's active colour runs; not rolling back a colour that is not proven to be ${sha}" >&2
  exit 1
fi
if [ "$running" != "$sha" ]; then
  echo "C00 runs ${running}, no longer the deployed ${sha}; its colour is not rolled back here."
  exit 0
fi

echo "::warning title=C00 rollback::readback of ${sha} failed; rolling back to the retained predecessor."
controller="$(cd deploy/cd && pwd)/cutover.sh"
bash deploy/cd/render-cutover-remote-script.sh \
  --router-state-dir "$router_state_dir" \
  -- bash -c "cd '$C00_COMPOSE_DIR' && . ./.env && export CUTOVER_DATABASE_URL=\"postgres://\${POSTGRES_USER:-multica}:\${POSTGRES_PASSWORD:-multica}@postgres:5432/\${POSTGRES_DB:-multica}?sslmode=disable\" && bash '$controller' rollback --compose-dir '$C00_COMPOSE_DIR' --state-dir '$C00_CUTOVER_STATE_DIR'" \
  | env -i HOME="$HOME" PATH="$PATH" bash -s

live="$(mktemp)"
trap 'rm -f "$live"' EXIT
bash deploy/cd/ab-health-check.sh --router-state-dir "$router_state_dir" --cutover-state-dir "$C00_CUTOVER_STATE_DIR" --window 60
bash deploy/cd/capture-remote-baseline.sh "$live"
echo "C00 rolled back from ${sha}; now running $(node -e 'process.stdout.write(String(JSON.parse(require("node:fs").readFileSync(process.argv[1],"utf8")).application_sha))' "$live")."
