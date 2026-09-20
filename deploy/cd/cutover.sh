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
image_tuple, migration_ledger_version, minimum_rollback_version,
router_generation, updated_at}. migration_ledger_version is the ACTIVE
colour's own (post-migration) ledger; minimum_rollback_version is copied
straight from the admitted release packet's own field — the compatibility
floor a future rollback verifies the current ledger sits AT OR AFTER
(order-aware, not strict equality — see cutover.sh's rollback branch) before
restarting the retained predecessor. This is the A/B counterpart to
deploy.sh's deployed-tuple.json. cutover.sh
reads and writes this file under the SAME flock deploy.sh uses (--state-dir is shared),
so a cutover.sh run and a deploy.sh run can never interleave.

Environment:
  GHCR_PULL_TOKEN     Optional read:packages GHCR PAT (CHE-549). When set,
                      the `cutover` command logs in to ghcr.io immediately
                      before pulling the candidate colour's image pair and
                      logs out on exit. Unset skips both silently — same
                      contract as deploy.sh's own GHCR_PULL_TOKEN.
  CUTOVER_DATABASE_URL   Required by `cutover`/`rollback`: the DATABASE_URL
                      the compose stack's postgres service uses.
  ROUTER_STATE_DIR    Overrides router.sh's state directory (test isolation
                      only — production uses the canonical repo-relative path).
  CUTOVER_LOCK_WAIT_SECONDS   Overrides the 600s deploy-lock wait bound
                      (test isolation only).
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

# router_state_dir MUST be the exact directory router/docker-compose.router.yml
# bind-mounts into the running router container (./state relative to that
# compose file, i.e. deploy/cd/router/state) — NOT a path derived from this
# invocation's own --state-dir, which is caller-supplied and arbitrary (a
# test's mktemp dir, an operator's chosen state root, ...). router.sh
# defaults to exactly this same canonical location when no --state-dir is
# given it; pointing cutover.sh's own call at a different, cutover-scoped
# directory (the original bug an independent review caught) would make
# `select` render and validate a generation the running container's bind
# mount can never see, so a cutover would report success while the router
# kept serving the previous generation. ROUTER_STATE_DIR exists solely for
# test-cutover.sh to point at an isolated scratch directory instead of the
# real repo-relative path.
router_state_dir="${ROUTER_STATE_DIR:-$root_dir/deploy/cd/router/state}"
deploy_lock="$state_dir/deploy.lock"

# shellcheck source=deploy-lib.sh
source "$script_dir/deploy-lib.sh"
compose_files=("docker-compose.selfhost.yml" "deploy/cd/docker-compose.ab.yml")

# ghcr_login/ghcr_logout (deploy-lib.sh, CHE-549) — C00 has no ambient GHCR
# credential, so an unauthenticated pull of the private multica-backend/
# multica-web packages fails with 403 the same way deploy.sh's single-slot
# pull did before CHE-549. Registered on EXIT immediately after sourcing
# deploy-lib.sh, before login ever runs, so logout fires on every exit path
# of every command (cutover, rollback, status, or an early require/usage
# exit) — not just the cutover pull's own success path.
trap ghcr_logout EXIT

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
  local colour=$1 backend_image=$2 web_image=$3 ledger_version=$4 minimum_rollback_version=$5
  cat >"$cutover_state_file" <<JSON
{
  "active_colour": "$colour",
  "image_tuple": { "backend": "$backend_image", "web": "$web_image" },
  "migration_ledger_version": "$ledger_version",
  "minimum_rollback_version": "$minimum_rollback_version",
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
#
# CUTOVER_LOCK_WAIT_SECONDS defaults to 600s (matching deploy.sh's own
# bound) — overridable solely so test-cutover.sh's "lost controller
# connection" scenario can prove the actual refusal path (message + no
# Docker calls) in a few seconds instead of waiting out the real 600s
# production bound.
lock_wait_seconds="${CUTOVER_LOCK_WAIT_SECONDS:-600}"
exec 9>"$deploy_lock"
if ! flock -w "$lock_wait_seconds" 9; then
  echo "could not acquire deploy lock within ${lock_wait_seconds}s — another deploy/cutover is running" >&2
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

    # Port-collision preflight: the base (un-scaled) "frontend" service
    # publishes 127.0.0.1:${FRONTEND_PORT:-3000}, the same host port the
    # router's own frontend listener binds (docker-compose.router.yml,
    # network_mode: host). docker-compose.ab.yml's own comment says the
    # base backend/frontend services must be scaled to zero once A/B is
    # adopted, but nothing enforced that — this is the documented
    # prerequisite from an independent review finding made into an actual
    # preflight check rather than an assumption. Refuse loudly here rather
    # than let the router container fail to bind at reload time, deep into
    # the cutover.
    base_running="$(compose ps --status running --format '{{.Service}}' backend frontend 2>/dev/null || true)"
    if [ -n "$base_running" ]; then
      echo "!! base (pre-A/B) service(s) still running: $base_running — these must be scaled to zero before an A/B cutover (docker compose ... up -d --scale backend=0 --scale frontend=0 <router-adopting compose invocation>), or the router's own 127.0.0.1:3000 listener collides with the un-scaled frontend service's published port" >&2
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

    # quiescence.mjs has no host `psql` binary to fall back to on C00 —
    # docker exec into the already-running postgres service container
    # (deploy-lib.sh's postgres_container_id) rather than quiescence.mjs's
    # own --psql-via-docker-network mode, which starts a SECOND, independent
    # postgres:16-alpine container per query: ~450-550ms of container-start
    # overhead against quiescence.mjs's own <=500ms DEFAULT_QUERY_TIMEOUT_MS
    # statement budget, which made every gate check below fail closed on a
    # timeout indistinguishable from real contention (CHE-655). `docker exec`
    # into the container already running measures ~100-150ms.
    postgres_container="$(postgres_container_id)" || exit 1

    # Step 1: admission gate + quiescence preflight against the CURRENTLY
    # ACTIVE colour's database connection — coarse, cheap, denies on any
    # already-visible hard blocker before an outage is even started.
    database_url="${CUTOVER_DATABASE_URL:?CUTOVER_DATABASE_URL is required, the same DATABASE_URL the compose stacks postgres service uses}"
    echo "==> quiescence preflight"
    if ! node "$script_dir/quiescence.mjs" preflight --database-url "$database_url" --psql-via-docker-exec "$postgres_container"; then
      echo "!! quiescence preflight denied cutover; $from_colour remains active" >&2
      exit 1
    fi

    if ! ghcr_login; then
      echo "!! ghcr.io login failed; $from_colour remains active" >&2
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

    # The release packet's OWN minimum_rollback_version is the rollback
    # compatibility anchor for THIS cutover — not a value cutover.sh derives
    # itself from the ledger. release-packet.mjs already validated it is a
    # real, known migration version (see that file's own checks), so
    # reading it straight from the admitted packet is the source of truth,
    # not a second, independently-computed guess. Read it now, before the
    # migration step runs, so a migration failure below never lands a
    # partially-written cutover-state.json.
    packet_minimum_rollback_version="$(json_field "$packet" minimum_rollback_version)"
    if [ -z "$packet_minimum_rollback_version" ]; then
      echo "!! release packet is missing minimum_rollback_version; refusing to cut over without a rollback compatibility anchor" >&2
      exit 1
    fi

    # Step 2: drain and stop the incumbent colour BEFORE the strict
    # quiescence final-gate runs and before the migrator touches the schema
    # — matching the accepted architecture's own step 2 ("drain and shut
    # down the old backend before verifying quiescence and running the
    # migrator") and this file's own header comment ("Gate/drain/quiesce/
    # migrate"). The final-gate demands ZERO foreign sessions/locks
    # (evaluateFinalGate, quiescence.mjs); the incumbent backend's own
    # connection pool holds idle sessions for as long as it keeps running,
    # so the gate could never admit while $from_colour still serves
    # (CHE-655, found live on C00: every cutover and rollback refused here,
    # unconditionally, purely because of this ordering bug — nothing was
    # ever actually contending). Everything from this point on runs with
    # NEITHER colour serving until the candidate is up and the router
    # switches — an unavoidable outage window for the quiesce+migrate phase,
    # not a regression: a live schema migration cannot safely run against a
    # schema a serving backend still holds connections against.
    echo "==> draining and stopping current colour=$from_colour (required before the quiescence final-gate can ever admit)"
    compose stop "backend-${from_colour}" "frontend-${from_colour}" >/dev/null 2>&1 || true

    # Step 3: final gate immediately before the first candidate mutation —
    # strict, zero-tolerance. A denial here means $from_colour is ALREADY
    # stopped (see above): the API is down and needs manual intervention to
    # restart it, not the "colour remains active" framing that was true
    # before this ordering fix.
    echo "==> quiescence final-gate (pre-migration)"
    if ! node "$script_dir/quiescence.mjs" final-gate --database-url "$database_url" --psql-via-docker-exec "$postgres_container"; then
      echo "!! quiescence final-gate denied cutover after draining $from_colour — the API is DOWN; MANUAL INTERVENTION REQUIRED to restart $from_colour" >&2
      exit 1
    fi

    # Run the standalone migrator exactly once, from the candidate image,
    # against the inactive colour's (stopped) service identity — never the
    # currently-serving colour's, so the one-shot container's --no-deps
    # never risks touching a live serving container's dependency graph.
    echo "==> running migration step (one-shot, candidate image, before candidate starts)"
    if ! run_migration_step "$backend_repo" "$image_tag" up; then
      echo "!! migration step failed after draining $from_colour — the API is DOWN; candidate not started; MANUAL INTERVENTION REQUIRED to restart $from_colour" >&2
      echo "!! quiescent gate must be re-run before retrying — do not restart the candidate against a partially migrated schema" >&2
      exit 1
    fi

    # Step 4: start the candidate through the migration-free authorized
    # path — MULTICA_SKIP_MIGRATIONS=1, the same guard deploy.sh's
    # single-slot forward path uses, so the just-applied migration step is
    # never repeated.
    echo "==> starting candidate colour=$to_colour (MULTICA_SKIP_MIGRATIONS=1)"
    if ! MULTICA_BACKEND_IMAGE="$backend_repo" MULTICA_WEB_IMAGE="$(image_repo "$web_image")" \
      MULTICA_IMAGE_TAG="$image_tag" MULTICA_SKIP_MIGRATIONS=1 \
      compose up -d --no-deps "backend-${to_colour}" "frontend-${to_colour}"; then
      echo "!! candidate colour=$to_colour failed to start after draining $from_colour — the API is DOWN; MANUAL INTERVENTION REQUIRED" >&2
      exit 1
    fi

    candidate_port="$(backend_port_for "$to_colour")"
    echo "==> health-checking candidate colour=$to_colour on port $candidate_port"
    if ! wait_ready_on_port "$candidate_port" 180; then
      echo "!! candidate colour=$to_colour did not become ready within 180s; stopping it — the API is DOWN ($from_colour already drained); MANUAL INTERVENTION REQUIRED" >&2
      compose stop "backend-${to_colour}" "frontend-${to_colour}" >/dev/null 2>&1 || true
      exit 1
    fi

    # Require the EXACT expected /health commit — a 200 alone only proves
    # something is listening on the port, not that it is the release just
    # migrated and started (see deploy-lib.sh's verify_health_identity).
    echo "==> verifying candidate /health identity"
    if ! verify_health_identity "$candidate_port" "$source_sha"; then
      echo "!! candidate colour=$to_colour /health identity mismatch; stopping it — the API is DOWN ($from_colour already drained); MANUAL INTERVENTION REQUIRED" >&2
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
      echo "!! router generation switch failed; candidate colour=$to_colour is running but NOT receiving traffic — the API is DOWN ($from_colour already drained and the router still is not pointed at $to_colour); MANUAL INTERVENTION REQUIRED" >&2
      exit 1
    fi

    ledger_version="$(current_ledger_version)"
    write_cutover_state "$to_colour" "$backend_image" "$web_image" "$ledger_version" "$packet_minimum_rollback_version"
    echo "==> cutover complete: $to_colour is now active (ledger version: $ledger_version; rollback compatibility floor: $packet_minimum_rollback_version)"
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
    # quiescence.mjs has no host `psql` binary to fall back to on C00 —
    # docker exec into the already-running postgres service container
    # rather than quiescence.mjs's own --psql-via-docker-network mode (see
    # the matching comment in the `cutover)` branch above for the measured
    # overhead difference, CHE-655).
    postgres_container="$(postgres_container_id)" || exit 1

    # Drain and stop the failing candidate BEFORE the strict quiescence
    # final-gate runs — same root cause and same fix as the `cutover)`
    # branch above (CHE-655): $from_colour is the failing candidate here,
    # and its own connection pool holds idle sessions for as long as it
    # keeps running, so the gate could never admit while it still serves.
    # Without this, the automated rollback refuses unconditionally and
    # leaves the ALREADY-FAILING candidate as the only thing "active" —
    # exactly the outage rollback exists to end.
    echo "==> draining and stopping failing candidate colour=$from_colour (required before the quiescence final-gate can ever admit)"
    compose stop "backend-${from_colour}" "frontend-${from_colour}" >/dev/null 2>&1 || true

    echo "==> quiescence final-gate (pre-rollback)"
    if ! node "$script_dir/quiescence.mjs" final-gate --database-url "$database_url" --psql-via-docker-exec "$postgres_container"; then
      echo "!! quiescence final-gate denied rollback after draining $from_colour — the API is DOWN; MANUAL INTERVENTION REQUIRED" >&2
      exit 1
    fi

    # The retained predecessor must be verified against the CURRENT schema,
    # not assumed compatible from when it was last active — a migration run
    # by the failed candidate before this rollback fires could have moved
    # the ledger. Never run a down migration or restore from backup here;
    # that is the separately authorized recovery path (accepted
    # architecture, step 4).
    #
    # The comparison anchor is minimum_rollback_version, straight from the
    # release packet admitted for the cutover that made $from_colour active
    # (see the "cutover)" branch above) — the actual proof mechanism the
    # accepted architecture and release-packet.mjs already define, not a
    # value cutover.sh derives on its own. The check is ORDER-AWARE
    # (ledger_at_or_after, deploy-lib.sh), not strict equality: under
    # expand/contract a retained predecessor is REQUIRED to serve the
    # expanded (post-migration) schema — that is the normal, expected
    # rollback condition, not an unsafe one, so the live ledger sitting AT
    # OR AFTER the floor is exactly the case this guard must admit. What it
    # refuses is the ledger sitting BEHIND that floor — e.g. an out-of-band
    # down migration — which is genuinely incompatible and not routine.
    # (Strict equality against the pre-migration ledger was CHE-608's
    # independent-review finding 1's first fix attempt; the reviewer's
    # follow-up correctly flagged that as over-corrected: it refused every
    # migration-inclusive cutover's own rollback unconditionally, which is
    # exactly the case A/B rollback exists for.)
    current_version="$(current_ledger_version)"
    minimum_rollback_version=""
    if [ -f "$cutover_state_file" ]; then
      minimum_rollback_version="$(json_field "$cutover_state_file" minimum_rollback_version)"
    fi
    if [ -z "$minimum_rollback_version" ]; then
      echo "!! no minimum_rollback_version recorded in $cutover_state_file — cannot verify $to_colour is compatible with the current schema" >&2
      echo "!! this is NOT a routine rollback case; refusing to start $to_colour blind — $from_colour is already drained, so the API is DOWN; MANUAL INTERVENTION REQUIRED" >&2
      exit 1
    fi
    if ! ledger_at_or_after "$current_version" "$minimum_rollback_version"; then
      echo "!! current ledger version ($current_version) is not at or after the rollback compatibility floor ($minimum_rollback_version) — the retained predecessor image cannot be assumed compatible with this schema" >&2
      echo "!! this is NOT a routine rollback case; a down migration or backup restore requires separate authorization — refusing to start $to_colour blind — $from_colour is already drained, so the API is DOWN; MANUAL INTERVENTION REQUIRED" >&2
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

    backend_image="$(json_field "$cutover_state_file" image_tuple.backend 2>/dev/null || echo "")"
    web_image="$(json_field "$cutover_state_file" image_tuple.web 2>/dev/null || echo "")"
    # minimum_rollback_version carries forward unchanged: no migration ran
    # during this rollback (see the guard above), so the same compatibility
    # floor a future rollback attempt would need to verify against is still
    # exactly right — nothing has moved the schema since it was last
    # recorded and just re-verified.
    write_cutover_state "$to_colour" "$backend_image" "$web_image" "$current_version" "$minimum_rollback_version"
    echo "==> rollback complete: $to_colour restored and healthy (all acknowledged data/uploads on shared Postgres/volume preserved — no restore performed)"
    ;;

  *)
    echo "unknown command: $command" >&2
    usage >&2
    exit 2
    ;;
esac
