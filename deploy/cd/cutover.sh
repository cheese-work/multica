#!/usr/bin/env bash
set -euo pipefail

# A/B slot cutover controller (CHE-397 unit 2). Runs ON C00, under the same
# deploy lock and Docker/Compose/migration primitives deploy.sh already
# established (D2, CHE-530) — this script extends that machinery rather than
# forking a second one: it sources deploy-lib.sh for every image/tuple/
# migration helper, calls deploy/cd/quiescence.mjs for the same admission
# gates D2 uses, and delegates the actual "is this packet safe to admit" and
# "is this router config well-formed" questions to release-packet.mjs and
# router.sh respectively.
#
# What this script adds that deploy.sh's single-slot path does not need:
# picking which of two ALREADY-DEPLOYED colours is active, and switching the
# local router's generation to match — deploy.sh has exactly one backend/
# frontend pair and never needs to choose between colours.
#
# ## Forward and rollback execution (accepted architecture, verbatim mapping)
#
#   1. Acquire lock, record state           -> stage_prepare()
#   2. Gate/drain/quiesce/migrate           -> stage_quiesce_and_migrate()
#   3. Start candidate, activate router     -> stage_activate()
#   4. Rollback: quiesce candidate, restore -> cmd_rollback()
#   5. Record reconciliation, release lock  -> the `flock` fd closing on exit
#
# Only one colour is ever started at a time (see docker-compose.ab.yml's own
# comment on why: in-memory realtime hubs and backend-owned schedulers make
# a second live backend unsafe, not just redundant).

usage() {
  cat <<'EOF'
usage: cutover.sh <command> [options]

Commands:
  cutover --manifest PATH --packet PATH --compose-dir PATH --state-dir PATH
      Full forward cutover: quiesce the currently-active colour, run the
      standalone migrator against the INACTIVE colour's image, start the
      inactive colour through the migration-free path, verify /health +
      /readyz, activate its router generation, reopen admission. On any
      gate failure, leaves the previously-active colour serving and exits
      nonzero without activating the router.

  rollback --compose-dir PATH --state-dir PATH
      Gate admission, verify the retained predecessor colour against the
      CURRENT schema, start it without migrations, restore its router
      generation. Never runs a down migration or restores from backup —
      that is the separately authorized recovery path.

  status --state-dir PATH
      Print which colour is currently active (from cutover-state.json) and
      exit 0. Never touches Docker or the router.

State file: --state-dir/cutover-state.json records {active_colour,
image_tuple, migration_ledger_version, router_generation, updated_at} —
the A/B counterpart to deploy.sh's deployed-tuple.json. cutover.sh reads and
writes this file under the SAME flock deploy.sh uses (--state-dir is shared),
so a cutover.sh run and a deploy.sh run can never interleave.
EOF
}

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

command=${1:-}
shift || true

if [ "$command" = "--help" ] || [ "$command" = "-h" ] || [ -z "$command" ]; then
  usage
  exit 0
fi

manifest=""
packet=""
compose_dir=""
state_dir=""

while (($#)); do
  case "$1" in
    --manifest) manifest=${2:?}; shift 2 ;;
    --packet) packet=${2:?}; shift 2 ;;
    --compose-dir) compose_dir=${2:?}; shift 2 ;;
    --state-dir) state_dir=${2:?}; shift 2 ;;
    --help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

require() {
  if [ -z "$2" ]; then
    echo "missing $1" >&2
    usage >&2
    exit 2
  fi
}
require --state-dir "$state_dir"
if [ "$command" != "status" ]; then
  require --compose-dir "$compose_dir"
fi

mkdir -p "$state_dir"
cutover_state_file="$state_dir/cutover-state.json"
router_state_dir="$state_dir/router"
deploy_lock="$state_dir/deploy.lock"

# shellcheck source=deploy-lib.sh
source "$script_dir/deploy-lib.sh"
compose_files=("docker-compose.selfhost.yml" "deploy/cd/docker-compose.ab.yml")

other_colour() {
  case "$1" in
    blue) echo green ;;
    green) echo blue ;;
    *) echo "unknown colour: $1" >&2; return 1 ;;
  esac
}

active_colour() {
  if [ -f "$cutover_state_file" ]; then
    json_field "$cutover_state_file" active_colour
  fi
}

backend_port_for() {
  case "$1" in
    blue) echo "${BACKEND_BLUE_PORT:-18081}" ;;
    green) echo "${BACKEND_GREEN_PORT:-18082}" ;;
  esac
}

write_cutover_state() {
  local colour=$1 backend_image=$2 web_image=$3 ledger_version=$4
  cat >"$cutover_state_file" <<JSON
{
  "active_colour": "$colour",
  "image_tuple": { "backend": "$backend_image", "web": "$web_image" },
  "migration_ledger_version": "$ledger_version",
  "router_generation_colour": "$colour",
  "updated_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
JSON
}

# A second cutover/rollback invocation while one is already running must not
# interleave migration steps, colour starts, or router generation swaps with
# this one — and must not race a concurrent deploy.sh single-slot run either,
# which is exactly why this uses the SAME lock file deploy.sh takes when
# --state-dir points at the same directory.
exec 9>"$deploy_lock"
if ! flock -w 600 9; then
  echo "could not acquire deploy lock within 600s — another deploy/cutover is running" >&2
  exit 1
fi

case "$command" in
  status)
    colour="$(active_colour)"
    if [ -z "$colour" ]; then
      echo "no cutover state recorded in $state_dir" >&2
      exit 1
    fi
    echo "$colour"
    exit 0
    ;;

  cutover)
    require --manifest "$manifest"
    require --packet "$packet"

    backend_image="$(json_field "$manifest" images.backend)"
    web_image="$(json_field "$manifest" images.web)"
    source_sha="$(json_field "$manifest" source_sha)"
    if [ ! "$backend_image" ] || [ ! "$web_image" ] || [ ! "$source_sha" ]; then
      echo "manifest is missing images.backend / images.web / source_sha" >&2
      exit 1
    fi
    backend_repo="$(image_repo "$backend_image")"
    backend_digest="$(image_digest "$backend_image")"
    image_tag="sha-${source_sha}"

    # Step 4 (contract): reject a packet with checksum drift, dirty ledger,
    # invalid indexes, incomplete hooks, or a missing rollback target BEFORE
    # any mutation — the packet is the release's admission proof, not an
    # advisory document.
    echo "==> verifying release packet contract"
    if ! node "$script_dir/release-packet.mjs" verify --packet "$packet"; then
      echo "!! release packet failed contract checks; refusing to cut over" >&2
      exit 1
    fi

    from_colour="$(active_colour)"
    if [ -z "$from_colour" ]; then
      from_colour="blue" # first-ever A/B cutover on this host: treat blue as the incumbent slot
      echo "==> no prior cutover-state.json; treating '$from_colour' as the incumbent colour for this first A/B cutover"
    fi
    to_colour="$(other_colour "$from_colour")"
    echo "==> cutover plan: $from_colour (active) -> $to_colour (candidate)"

    migration_service_name="backend-${to_colour}"

    # Step 1: admission gate + quiescence preflight against the CURRENTLY
    # ACTIVE colour's database connection — coarse, cheap, denies on any
    # already-visible hard blocker before an outage is even started.
    database_url="${CUTOVER_DATABASE_URL:?CUTOVER_DATABASE_URL is required, the same DATABASE_URL the compose stacks postgres service uses}"
    echo "==> quiescence preflight"
    if ! node "$script_dir/quiescence.mjs" preflight --database-url "$database_url"; then
      echo "!! quiescence preflight denied cutover; $from_colour remains active" >&2
      exit 1
    fi

    # Step 2: final gate immediately before the first candidate mutation —
    # strict, zero-tolerance. A denial here leaves $from_colour serving,
    # untouched.
    echo "==> quiescence final-gate (pre-migration)"
    if ! node "$script_dir/quiescence.mjs" final-gate --database-url "$database_url"; then
      echo "!! quiescence final-gate denied cutover; $from_colour remains active" >&2
      exit 1
    fi

    echo "==> pulling candidate image pair (tag ${image_tag}) for colour=$to_colour"
    if ! MULTICA_BACKEND_IMAGE="$backend_repo" MULTICA_IMAGE_TAG="$image_tag" \
      compose pull "backend-${to_colour}" "frontend-${to_colour}"; then
      echo "!! image pull failed for colour=$to_colour; $from_colour remains active" >&2
      exit 1
    fi
    if ! verify_pulled_digest "$backend_repo" "$image_tag" "$backend_digest"; then
      echo "!! pulled image digest mismatch for colour=$to_colour; $from_colour remains active" >&2
      exit 1
    fi

    # Run the standalone migrator exactly once, from the candidate image,
    # against the inactive colour's (stopped) service identity — never the
    # currently-serving colour's, so the one-shot container's --no-deps
    # never risks touching a live serving container's dependency graph.
    echo "==> running migration step (one-shot, candidate image, before candidate starts)"
    if ! run_migration_step "$backend_repo" "$image_tag" up; then
      echo "!! migration step failed; $from_colour remains active, candidate not started" >&2
      echo "!! quiescent gate must be re-run before retrying — do not restart the candidate against a partially migrated schema" >&2
      exit 1
    fi

    # Step 3: start the candidate through the migration-free authorized
    # path — MULTICA_SKIP_MIGRATIONS=1, the same guard deploy.sh's
    # single-slot forward path uses, so the just-applied migration step is
    # never repeated.
    echo "==> starting candidate colour=$to_colour (MULTICA_SKIP_MIGRATIONS=1)"
    if ! MULTICA_BACKEND_IMAGE="$backend_repo" MULTICA_WEB_IMAGE="$(image_repo "$web_image")" \
      MULTICA_IMAGE_TAG="$image_tag" MULTICA_SKIP_MIGRATIONS=1 \
      compose up -d --no-deps "backend-${to_colour}" "frontend-${to_colour}"; then
      echo "!! candidate colour=$to_colour failed to start; $from_colour remains active" >&2
      exit 1
    fi

    candidate_port="$(backend_port_for "$to_colour")"
    echo "==> health-checking candidate colour=$to_colour on port $candidate_port"
    if ! wait_ready_on_port "$candidate_port" 180; then
      echo "!! candidate colour=$to_colour did not become ready within 180s; stopping it and leaving $from_colour active" >&2
      compose stop "backend-${to_colour}" "frontend-${to_colour}" >/dev/null 2>&1 || true
      exit 1
    fi

    # Require the EXACT expected /health commit — a 200 alone only proves
    # something is listening on the port, not that it is the release just
    # migrated and started (see deploy-lib.sh's verify_health_identity).
    echo "==> verifying candidate /health identity"
    if ! verify_health_identity "$candidate_port" "$source_sha"; then
      echo "!! candidate colour=$to_colour /health identity mismatch; stopping it and leaving $from_colour active" >&2
      compose stop "backend-${to_colour}" "frontend-${to_colour}" >/dev/null 2>&1 || true
      exit 1
    fi

    # Activate the complete router generation. router.sh's own `nginx -t`
    # gate means an invalid config never reaches the running router — this
    # is the "prepare, validate, atomically replace, reload" sequence from
    # the accepted architecture, delegated wholesale rather than
    # reimplemented here.
    echo "==> activating router generation for colour=$to_colour"
    if ! bash "$script_dir/router.sh" select --colour "$to_colour" --state-dir "$router_state_dir"; then
      echo "!! router generation switch failed; candidate colour=$to_colour is running but NOT receiving traffic — $from_colour's router generation is still active" >&2
      exit 1
    fi

    # Quiesce and stop the now-inactive colour AFTER traffic has moved off
    # it — draining it before the router switch would have caused an outage
    # for no benefit, since the switch itself is what stops new requests
    # from reaching it.
    echo "==> stopping now-inactive colour=$from_colour"
    compose stop "backend-${from_colour}" "frontend-${from_colour}" >/dev/null 2>&1 || true

    ledger_version="$(current_ledger_version)"
    write_cutover_state "$to_colour" "$backend_image" "$web_image" "$ledger_version"
    echo "==> cutover complete: $to_colour is now active (ledger version: $ledger_version)"
    ;;

  rollback)
    from_colour="$(active_colour)"
    if [ -z "$from_colour" ]; then
      echo "no cutover-state.json in $state_dir; nothing recorded to roll back from" >&2
      exit 1
    fi
    to_colour="$(other_colour "$from_colour")"
    echo "==> rollback plan: $from_colour (active, failing) -> $to_colour (retained predecessor)"

    database_url="${CUTOVER_DATABASE_URL:?CUTOVER_DATABASE_URL is required}"
    echo "==> quiescence final-gate (pre-rollback)"
    if ! node "$script_dir/quiescence.mjs" final-gate --database-url "$database_url"; then
      echo "!! quiescence final-gate denied rollback — gating admission but NOT restarting $to_colour blind" >&2
      exit 1
    fi

    # The retained predecessor must be verified against the CURRENT schema,
    # not assumed compatible from when it was last active — a migration run
    # by the failed candidate before this rollback fires could have moved
    # the ledger. Never run a down migration or restore from backup here;
    # that is the separately authorized recovery path (accepted
    # architecture, step 4).
    current_version="$(current_ledger_version)"
    previous_recorded_version=""
    if [ -f "$cutover_state_file" ]; then
      previous_recorded_version="$(json_field "$cutover_state_file" migration_ledger_version)"
    fi
    if [ -n "$previous_recorded_version" ] && [ "$current_version" != "$previous_recorded_version" ]; then
      echo "!! current ledger version ($current_version) does not match the predecessor's last-known-good version ($previous_recorded_version) — the retained predecessor image cannot be assumed compatible with a schema it has never served" >&2
      echo "!! this is NOT a routine rollback case; a down migration or backup restore requires separate authorization — refusing to start $to_colour blind" >&2
      exit 1
    fi

    backend_port="$(backend_port_for "$to_colour")"
    echo "==> starting retained predecessor colour=$to_colour without migrations"
    if ! MULTICA_SKIP_MIGRATIONS=1 compose up -d --no-deps "backend-${to_colour}" "frontend-${to_colour}"; then
      echo "!! failed to start retained predecessor colour=$to_colour — MANUAL INTERVENTION REQUIRED" >&2
      exit 1
    fi

    if ! wait_ready_on_port "$backend_port" 120; then
      echo "!! retained predecessor colour=$to_colour did not become healthy within 120s — MANUAL INTERVENTION REQUIRED" >&2
      exit 1
    fi

    echo "==> restoring router generation for colour=$to_colour"
    if ! bash "$script_dir/router.sh" select --colour "$to_colour" --state-dir "$router_state_dir"; then
      echo "!! router restore failed — retained predecessor colour=$to_colour is healthy but NOT receiving traffic; MANUAL INTERVENTION REQUIRED" >&2
      exit 1
    fi

    echo "==> stopping failed candidate colour=$from_colour"
    compose stop "backend-${from_colour}" "frontend-${from_colour}" >/dev/null 2>&1 || true

    backend_image="$(json_field "$cutover_state_file" image_tuple.backend 2>/dev/null || echo "")"
    web_image="$(json_field "$cutover_state_file" image_tuple.web 2>/dev/null || echo "")"
    write_cutover_state "$to_colour" "$backend_image" "$web_image" "$current_version"
    echo "==> rollback complete: $to_colour restored and healthy (all acknowledged data/uploads on shared Postgres/volume preserved — no restore performed)"
    ;;

  *)
    echo "unknown command: $command" >&2
    usage >&2
    exit 2
    ;;
esac
