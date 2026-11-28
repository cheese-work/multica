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
      With a contract migration applied, first proves the exact stopped
      containers (image holds the migration, pinned reference matches) and
      starts them with `compose start`; any doubt exits 3, before the drain.

  contract-check --compose-dir PATH --state-dir PATH (--colour blue|green | --image REF)
      Read-only. Read the applied contract migrations from the database and
      prove that the colour's backend container image (running or stopped), or
      the image REF, holds every one of them. Exit 0 proven or none applied,
      3 refused (a migration file is missing), 1 uncertain (nothing started or
      changed either way). Never takes the deploy lock.

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
check_colour=""
check_image=""

while (($#)); do
  case "$1" in
    --manifest) manifest=${2:?}; shift 2 ;;
    --packet) packet=${2:?}; shift 2 ;;
    --compose-dir) compose_dir=${2:?}; shift 2 ;;
    --state-dir) state_dir=${2:?}; shift 2 ;;
    --colour) check_colour=${2:?}; shift 2 ;;
    --image) check_image=${2:?}; shift 2 ;;
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
# shellcheck source=contract-lib.sh
source "$script_dir/contract-lib.sh"
compose_files=("docker-compose.selfhost.yml" "$script_dir/docker-compose.ab.yml")
flags_file=""

flags_overlay_prepare() {
  local flags flags_var=MULTICA_FEATURE_FLAGS_FILE
  flags="$(env_setting "$flags_var")"
  if [ -z "$flags" ]; then
    flags_var=MULTICA_JEV_FLAGS_FILE
    flags="$(env_setting "$flags_var")"
  fi
  if [ -z "$flags" ]; then
    local colour cid mounted
    colour="$(active_colour)"
    colour="${colour:-blue}"
    if ! cid="$(compose ps --all -q "backend-${colour}")"; then
      echo "!! flags continuity: cannot resolve previous active backend; refusing before any change" >&2
      exit 1
    fi
    [ -n "$cid" ] || return 0
    if ! mounted="$(docker inspect --format '{{json .Mounts}}' "$cid" | node -e '
      let input = "";
      process.stdin.on("data", (chunk) => input += chunk).on("end", () => {
        const mounts = JSON.parse(input);
        if (!Array.isArray(mounts)) process.exit(1);
        console.log(mounts.some((mount) => mount.Destination === "/app/feature-flags.yaml"));
      });
    ')"; then
      echo "!! flags continuity: cannot inspect previous active backend flags mount; refusing before any change" >&2
      exit 1
    fi
    if [ "$mounted" = true ]; then
      echo "!! flags continuity: MULTICA_FEATURE_FLAGS_FILE and MULTICA_JEV_FLAGS_FILE are unset but previous active backend has a flags mount; refusing before any change" >&2
      exit 1
    fi
    return 0
  fi
  flags_refuse() {
    echo "!! flags overlay: $flags_var $1; refusing before any change" >&2
    exit 1
  }
  case "$flags" in
    /*) [ -f "$flags" ] && [ -r "$flags" ] && [ -s "$flags" ] || flags_refuse "is not a readable, non-empty file"
      node "$script_dir/flagfile-check.mjs" "$flags" || flags_refuse "is not a valid feature-flag file (see the line above)" ;;
    *) flags_refuse "is unset or not an absolute path" ;;
  esac
  compose_files+=("$script_dir/docker-compose.flags.yml")
  flags_file="$flags"
  echo "==> flags overlay enabled (flag file verified)"
}

frontend_flags_match() {
  [ -n "$flags_file" ] || return 0
  curl --fail --silent --show-error --max-time 10 "http://127.0.0.1:$1/api/config" |
    node "$script_dir/flagfile-check.mjs" "$flags_file" --config-stdin
}

# Scoped Jev runtime overlay (CHE-1854). Opt-in per installation: the compose
# dir's .env sets MULTICA_JEV_GATEWAY_NETWORK.
# Once set, ALL prerequisites must hold (key, flag file, network);
# any gap refuses here, before the first drain, pull, migration or start, and
# names the variable but never its value. Unset = Jev overlay not loaded.
jev_overlay_prepare() {
  local flags network key
  network="$(env_setting MULTICA_JEV_GATEWAY_NETWORK)"
  [ -n "$network" ] || return 0
  flags="$(env_setting MULTICA_FEATURE_FLAGS_FILE)"
  [ -n "$flags" ] || flags="$(env_setting MULTICA_JEV_FLAGS_FILE)"
  key="$(env_setting MULTICA_JEV_SECRET_KEY)"
  jev_refuse() {
    echo "!! jev overlay: $1 $2; refusing before any change" >&2
    exit 1
  }
  [ -n "$flags" ] || jev_refuse MULTICA_JEV_FLAGS_FILE "is unset (or set MULTICA_FEATURE_FLAGS_FILE)"
  docker network inspect "$network" >/dev/null 2>&1 || jev_refuse MULTICA_JEV_GATEWAY_NETWORK "names a network that does not exist"
  # Same contract as the server's credential.DecodeKey: standard base64 of 32 bytes.
  printf '%s' "$key" | node -e '
    let s = "";
    process.stdin.on("data", (c) => (s += c)).on("end", () => {
      const b = Buffer.from(s, "base64");
      process.exit(b.length === 32 && b.toString("base64") === s ? 0 : 1);
    });
  ' || jev_refuse MULTICA_JEV_SECRET_KEY "is unset or not base64 of 32 bytes"
  compose_files+=("$script_dir/docker-compose.jev.yml")
  echo "==> jev overlay enabled (flag file, deployment key and gateway network verified)"
}

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

# Ports (CHE-773). Every slot port comes from `docker compose config` —
# the same .env-aware render Compose publishes from — and is exported under
# the names router.sh reads, so controller, Compose and router can never
# hold different interpretations. The previous `${BACKEND_GREEN_PORT:-18082}`
# fallback silently disagreed with Compose whenever .env set a port the
# calling shell had sourced but not exported (`. ./.env` in CD's `bash -c`):
# the controller health-checked 18082 while green listened on 18092, then
# stopped green with blue already drained.
resolve_slot_ports() {
  local colour role target var port
  for colour in blue green; do
    for role in backend frontend; do
      target=8080
      [ "$role" = frontend ] && target=3000
      var="$(printf '%s_%s_PORT' "$role" "$colour" | tr '[:lower:]' '[:upper:]')"
      if ! port="$(compose_rendered_port "${role}-${colour}" "$target")"; then
        echo "!! could not resolve the host port Compose renders for ${role}-${colour}:${target}" >&2
        return 1
      fi
      export "$var=$port"
    done
  done
}

slot_port() {
  local var
  var="$(printf '%s_%s_PORT' "$1" "$2" | tr '[:lower:]' '[:upper:]')"
  printf '%s' "${!var}"
}

# The router's own listeners (router/nginx.conf.template) — what Tailscale
# Serve forwards public traffic to.
router_api_port=8081
router_web_port=3000

# published_port_matches: the RUNNING container's actual published binding
# (`docker compose port`) equals the rendered one.
published_port_matches() {
  local service=$1 target=$2 want=$3 got
  got="$(service_published_port "$service" "$target")"
  if [ "$got" != "$want" ]; then
    echo "!! ${service}:${target} is published on '${got:-<none>}', Compose renders '$want'" >&2
    return 1
  fi
}

# conf_upstreams prints "<listen port>=<upstream port>" for each server
# block of an Nginx config read on stdin (router/nginx.conf.template shape:
# one listen + one proxy_pass per server).
conf_upstreams() {
  awk '
    /listen 127\.0\.0\.1:[0-9]+/ { match($0, /:[0-9]+/); listen = substr($0, RSTART + 1, RLENGTH - 1) }
    /proxy_pass http:\/\/127\.0\.0\.1:[0-9]+/ {
      match($0, /127\.0\.0\.1:[0-9]+/)
      route = listen "=" substr($0, RSTART + 10, RLENGTH - 10)
      if (!seen[route]++) print route
    }
  '
}

# router_selects: the router forwards to exactly this colour's rendered
# ports — checked in all three places that could disagree: the generation
# metadata (active.json), the generation file Nginx includes (active.conf),
# and the config the RUNNING router container actually loads (`nginx -T`
# inside it, which also proves its bind mount sees this generation).
router_selects() {
  local colour=$1 json="$router_state_dir/active.json" c b f want got
  if [ ! -f "$json" ]; then
    echo "!! no active router generation at $json" >&2
    return 1
  fi
  c="$(json_field "$json" colour)"
  b="$(json_field "$json" backend_port)"
  f="$(json_field "$json" frontend_port)"
  if [ "$c" != "$colour" ] || [ "$b" != "$(slot_port backend "$colour")" ] || [ "$f" != "$(slot_port frontend "$colour")" ]; then
    echo "!! router metadata selects colour=$c backend=$b frontend=$f; expected colour=$colour backend=$(slot_port backend "$colour") frontend=$(slot_port frontend "$colour")" >&2
    return 1
  fi
  want="$(printf '%s=%s\n%s=%s' "$router_api_port" "$(slot_port backend "$colour")" "$router_web_port" "$(slot_port frontend "$colour")")"
  got="$(conf_upstreams <"$router_state_dir/active.conf" 2>/dev/null || true)"
  if [ "$got" != "$want" ]; then
    echo "!! router active.conf upstreams [${got//$'\n'/ }] differ from expected [${want//$'\n'/ }]" >&2
    return 1
  fi
  got="$(docker exec "${ROUTER_CONTAINER_NAME:-multica-ab-router}" nginx -T 2>/dev/null | conf_upstreams || true)"
  if [ "$got" != "$want" ]; then
    echo "!! running router's effective config (nginx -T) upstreams [${got//$'\n'/ }] differ from expected [${want//$'\n'/ }]" >&2
    return 1
  fi
}

health_commit() {
  curl --fail --silent --show-error "http://127.0.0.1:$1/health" 2>/dev/null | json_field /dev/stdin commit 2>/dev/null
}

health_process() {
  curl --fail --silent --show-error "http://127.0.0.1:$1/health" 2>/dev/null |
    node -e 'let d="";process.stdin.on("data",c=>d+=c).on("end",()=>{try{const h=JSON.parse(d);if(h.commit&&h.pid)process.stdout.write(`${h.commit}/${h.pid}/${h.started_at??""}`)}catch{}})'
}

# public_route_serves: the router's public API listener reaches the SAME
# backend process as the slot port (commit + pid + started_at from /health —
# a commit alone cannot tell two slots running the same release apart), it
# reports the expected commit, and the public web listener answers.
public_route_serves() {
  local backend_port=$1 commit=$2 via_router direct
  via_router="$(health_process "$router_api_port")"
  direct="$(health_process "$backend_port")"
  if [ -z "$direct" ] || [ "$via_router" != "$direct" ] || [ "${direct%%/*}" != "$commit" ]; then
    echo "!! public API :$router_api_port reaches process '${via_router:-<none>}', slot :$backend_port is '${direct:-<none>}', expected commit $commit" >&2
    return 1
  fi
  curl --fail --silent --show-error "http://127.0.0.1:${router_web_port}/" >/dev/null 2>&1
}

# stop_confirmed stops services and proves they are down: Compose's own exit
# status AND `compose ps --status running` listing none of them. The single-
# active-backend invariant rests on this — a stop that failed or left a
# container running must never be followed by a migration or by starting
# the other colour.
stop_confirmed() {
  local still
  if ! compose stop "$@" >/dev/null 2>&1; then
    echo "!! docker compose stop $* failed" >&2
    return 1
  fi
  if ! still="$(compose ps --status running --format '{{.Service}}' "$@" 2>/dev/null)"; then
    echo "!! could not confirm $* stopped (docker compose ps failed)" >&2
    return 1
  fi
  if [ -n "$still" ]; then
    echo "!! still running after stop: ${still//$'\n'/ }" >&2
    return 1
  fi
}

# container_image_id: the image ID the service's RUNNING container was
# created from — not what its tag points at now, which a stale container
# would not reflect.
container_image_id() {
  local cid
  cid="$(compose ps -q "$1" 2>/dev/null | head -1)"
  [ -n "$cid" ] || return 1
  docker inspect --format '{{.Image}}' "$cid" 2>/dev/null
}

# runs_verified_image: service's running container was created from exactly
# the image id verified_image_id returned for the manifest digest. The
# reference is not read again here: it may point elsewhere by now.
runs_verified_image() {
  local service=$1 want=$2 running
  running="$(container_image_id "$service" || true)"
  if [ -z "$want" ] || [ "$running" != "$want" ]; then
    echo "!! $service runs image '${running:-<none>}', verified image id is '${want:-<none>}'" >&2
    return 1
  fi
}

alert_outage() {
  local reason=$1
  echo "::error title=Multica A/B cutover outage::$reason" >&2
  echo "!! OUTAGE: $reason — MANUAL INTERVENTION REQUIRED (runbook: deploy/cd/README.md, \"A/B outage runbook\")" >&2
  cat >"$state_dir/outage-alert.json" <<JSON
{"at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)", "reason": "$reason"}
JSON
}

# recover_incumbent restarts the drained incumbent after a post-drain
# failure — only when compatibility is proven, never blind:
#   - the candidate is stopped first (one backend/scheduler at a time);
#   - the live ledger is unchanged since the incumbent last served it, or
#     sits within [packet minimum_rollback_version, packet final version] —
#     a schema the incumbent is required to serve under expand/contract (no
#     down migration, no restore);
#   - `compose start` reuses the incumbent's own stopped containers, and the
#     restarted image must equal the one recorded before the drain;
#   - its /health commit, its router generation and the public route must
#     all read back as the pre-drain incumbent.
# Any unproven step raises alert_outage instead. Always exits nonzero: the
# cutover itself failed.
recover_incumbent() {
  local reason=$1 ledger final minimum
  echo "!! $reason" >&2
  echo "==> post-drain failure; attempting compatibility-gated recovery of incumbent colour=$from_colour" >&2
  if ! stop_confirmed "backend-${to_colour}" "frontend-${to_colour}"; then
    alert_outage "$reason; candidate $to_colour shutdown NOT confirmed, so $from_colour was NOT started (never two backends on one DB); stop $to_colour by hand, then follow the runbook"
    exit 1
  fi
  if [ -z "$incumbent_commit" ] || [ -z "$incumbent_backend_image" ] || [ -z "$incumbent_web_image" ] ||
    [ -z "$incumbent_backend_image_id" ] || [ -z "$incumbent_web_image_id" ] || [ -z "$incumbent_ledger" ]; then
    alert_outage "$reason; pre-drain incumbent identity is unproven, so $from_colour was NOT restarted"
    exit 1
  fi

  ledger="$(current_ledger_version)" || ledger=""
  minimum="$incumbent_ledger"
  final="$incumbent_ledger"
  if [ "$command" = cutover ]; then
    minimum="$packet_minimum_rollback_version"
    final="$(node -e 'const p=JSON.parse(require("fs").readFileSync(process.argv[1],"utf8"));process.stdout.write(p.ordered_migrations.at(-1)?.version??"")' "$packet")"
  fi
  if [ -z "$ledger" ] || { [ "$ledger" != "$incumbent_ledger" ] && {
    ! ledger_at_or_after "$ledger" "$minimum" "$root_dir/server/migrations" ||
      ! ledger_at_or_after "$final" "$ledger" "$root_dir/server/migrations"; }; }; then
    alert_outage "$reason; ledger '${ledger:-<unreadable>}' is neither the pre-drain ledger ($incumbent_ledger) nor within [$minimum, $final], so $from_colour is not proven schema-compatible and was NOT restarted; both colours are stopped"
    exit 1
  fi
  # CHE-7486 N5b-1: with a contract migration applied, the incumbent image
  # must hold its migration file. The applied set is read again here, after
  # the drain, never taken from the pre-drain read or the state file.
  if ! contract_applied_read; then
    alert_outage "$reason; contract state unreadable ($contract_read_error), so $from_colour was NOT restarted; both colours are stopped; $(contract_recovery_text)"
    exit 1
  fi
  contract_proof_rc=0
  contract_image_proof "$incumbent_backend_image_id" || contract_proof_rc=$?
  case "$contract_proof_rc" in
    0) ;;
    3)
      alert_outage "$reason; contract migration $(contract_applied_versions) applied (attempt $(contract_applied_attempts)) and the $from_colour image $incumbent_backend_image_id does not hold it and was NOT restarted; both colours are stopped; $(contract_recovery_text)"
      exit 1
      ;;
    *)
      alert_outage "$reason; contract state unreadable (the image proof on $incumbent_backend_image_id could not run), so $from_colour was NOT restarted; both colours are stopped; $(contract_recovery_text)"
      exit 1
      ;;
  esac
  if ! compose start "backend-${from_colour}" "frontend-${from_colour}" >/dev/null 2>&1; then
    alert_outage "$reason; restarting $from_colour failed; both colours are stopped"
    exit 1
  fi
  if [ "$(container_image_id "backend-${from_colour}" || true)" != "$incumbent_backend_image_id" ] ||
    [ "$(container_image_id "frontend-${from_colour}" || true)" != "$incumbent_web_image_id" ]; then
    stop_confirmed "backend-${from_colour}" "frontend-${from_colour}" || true
    alert_outage "$reason; restarted $from_colour container image differs from the pre-drain incumbent ($incumbent_backend_image_id / $incumbent_web_image_id); stopping it again was attempted"
    exit 1
  fi
  if ! published_port_matches "backend-${from_colour}" 8080 "$(slot_port backend "$from_colour")" ||
    ! published_port_matches "frontend-${from_colour}" 3000 "$(slot_port frontend "$from_colour")" ||
    ! wait_ready_on_port "$(slot_port backend "$from_colour")" 120 ||
    ! verify_health_identity "$(slot_port backend "$from_colour")" "$incumbent_commit"; then
    alert_outage "$reason; $from_colour was restarted but did not come back healthy as commit $incumbent_commit"
    exit 1
  fi
  if ! bash "$script_dir/router.sh" select --colour "$from_colour" --state-dir "$router_state_dir" ||
    ! router_selects "$from_colour" || ! public_route_serves "$(slot_port backend "$from_colour")" "$incumbent_commit"; then
    alert_outage "$reason; $from_colour is running and healthy but the public route does not read back as $from_colour"
    exit 1
  fi
  echo "!! RECOVERED: cutover to $to_colour failed ($reason); $from_colour restarted (image $incumbent_backend_image, commit $incumbent_commit, ledger $ledger) and the public route reads back healthy. Cutover state unchanged; investigate before retrying." >&2
  exit 1
}

# rollback_contract_binding (CHE-7486 N5b-2): with a contract migration
# applied, `rollback` starts only the exact stopped containers whose image id
# it has proven, never what a tag or the state file names. Runs BEFORE the
# drain; every doubt is exit 3 "rollback refused: <reason>" with nothing
# stopped, started or created. Sets rollback_bound=1 and the proven ids when a
# contract is applied; with none applied it changes nothing (today's path).
#   1. read the applied set (N5b-1); unreadable refuses
#   2. exactly one stopped backend-<to> and frontend-<to> container each, and
#      the image id each was created from
#   3. the backend image id holds every applied migration file
#   4. the pinned reference (`compose config` under the caller's
#      MULTICA_BACKEND_IMAGE / MULTICA_WEB_IMAGE / MULTICA_IMAGE_TAG) of each
#      service resolves to that container's own image id
rollback_refuse() {
  echo "rollback refused: $1" >&2
  exit 3
}

rollback_bound=0
rollback_proven_backend_id=""
rollback_proven_web_id=""
rollback_contract_binding() {
  local service cids count cid image_id reference config resolved proof_rc=0
  rollback_bound=0
  contract_applied_read || rollback_refuse "contract state unreadable ($contract_read_error)"
  [ -n "$contract_applied" ] || return 0

  for service in backend frontend; do
    cids="$(compose ps --all -q "${service}-${to_colour}" 2>/dev/null)" || rollback_refuse "cannot list the ${service}-${to_colour} containers"
    count="$(printf '%s' "$cids" | grep -c . || true)"
    if [ "$count" -eq 0 ]; then
      rollback_refuse "${service}-${to_colour} has no stopped container (want exactly one)"
    elif [ "$count" -ne 1 ]; then
      rollback_refuse "${service}-${to_colour} has $count containers (want exactly one)"
    fi
    cid="$cids"
    image_id="$(docker inspect --format '{{.Image}}' "$cid" 2>/dev/null | tr -d '[:space:]')" || image_id=""
    [ -n "$image_id" ] || rollback_refuse "cannot read the image id of the ${service}-${to_colour} container"
    if [ "$service" = backend ]; then rollback_proven_backend_id="$image_id"; else rollback_proven_web_id="$image_id"; fi
  done

  contract_image_proof "$rollback_proven_backend_id" || proof_rc=$?
  case "$proof_rc" in
    0) ;;
    3) rollback_refuse "backend-${to_colour} image $rollback_proven_backend_id does not hold contract migration $(contract_applied_versions) (attempt $(contract_applied_attempts)); $(contract_recovery_text)" ;;
    *) rollback_refuse "contract state unreadable (the image proof on $rollback_proven_backend_id could not run)" ;;
  esac

  config="$(compose config --format json 2>/dev/null)" || rollback_refuse "cannot render the pinned references (docker compose config failed)"
  for service in backend frontend; do
    reference="$(printf '%s' "$config" | node -e '
      const c = JSON.parse(require("node:fs").readFileSync(0, "utf8"));
      const image = c.services?.[process.argv[1]]?.image;
      if (typeof image !== "string" || image === "") process.exit(1);
      process.stdout.write(image);
    ' "${service}-${to_colour}" 2>/dev/null)" || rollback_refuse "cannot read the pinned reference of ${service}-${to_colour}"
    resolved="$(docker image inspect --format '{{.Id}}' "$reference" 2>/dev/null | tr -d '[:space:]')" || resolved=""
    if [ "$service" = backend ]; then image_id="$rollback_proven_backend_id"; else image_id="$rollback_proven_web_id"; fi
    [ "$resolved" = "$image_id" ] ||
      rollback_refuse "${service}-${to_colour} pinned reference $reference resolves to ${resolved:-<nothing>}, but the stopped container holds $image_id"
  done
  rollback_bound=1
}

# Contract snapshot gate (CHE-7486 N5a). Runs after the quiescence final-gate
# and before the migrator, only when the packet lists a contract migration.
# It snapshots each listed table with COPY, checks the copy against its
# manifest, writes the attempt record, and sets contract_attempt_id for the
# migrator. Any failure restarts the incumbent: no migration has run yet.
contract_psql() {
  compose exec -T postgres psql -U "${POSTGRES_USER:-multica}" -d "${POSTGRES_DB:-multica}" -tA -v ON_ERROR_STOP=1 -c "$1" </dev/null
}

# contract_scalar returns one query answer; COPY output is streamed with
# contract_psql, because command substitution would drop its trailing bytes.
contract_scalar() {
  local out
  out="$(contract_psql "$1")" || return 1
  printf '%s' "$out"
}

contract_fail() {
  recover_incumbent "contract snapshot or attempt record failed (no migration ran): $1"
}

contract_snapshot_gate() {
  local list version tables restore table present count rows columns copy entry
  local -a absent_flag
  local snapshots_dir attempts_dir attempt_dir entries versions_csv manifest_sha ledger_latest ledger_rows
  local -A copied=()
  contract_attempt_id=""
  unset MULTICA_CONTRACT_ATTEMPT
  list="$(node "$script_dir/contract-snapshot.mjs" list --packet "$packet")" || contract_fail "the packet's contract list is unreadable"
  [ -n "$list" ] || return 0

  while IFS=$'\t' read -r version tables restore; do
    count="$(contract_scalar "SELECT count(*) FROM schema_migrations WHERE version = '$version'")" || contract_fail "cannot read the ledger for $version"
    [[ "$count" =~ ^[0-9]+$ ]] || contract_fail "unreadable ledger answer for $version"
    [ "$count" -eq 0 ] || recover_incumbent "contract migration $version already has a ledger row, so no snapshot can precede it (no migration ran)"
    present="$(contract_scalar "SELECT to_regclass('schema_contracts') IS NOT NULL")" || contract_fail "cannot look up schema_contracts"
    case "$present" in
      t)
        count="$(contract_scalar "SELECT count(*) FROM schema_contracts WHERE version = '$version'")" || contract_fail "cannot read schema_contracts for $version"
        [[ "$count" =~ ^[0-9]+$ ]] || contract_fail "unreadable schema_contracts answer for $version"
        [ "$count" -eq 0 ] || recover_incumbent "contract migration $version already has a schema_contracts row (no migration ran)"
        ;;
      f) ;;
      *) contract_fail "unreadable schema_contracts lookup" ;;
    esac
  done <<<"$list"

  contract_attempt_id="$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  if ! [[ "$contract_attempt_id" =~ ^[0-9a-f]{32}$ ]]; then
    contract_attempt_id=""
    contract_fail "no attempt id from /dev/urandom"
  fi
  snapshots_dir="$state_dir/contract-snapshots"
  attempts_dir="$state_dir/contract-attempts"
  attempt_dir="$snapshots_dir/$contract_attempt_id"
  (umask 077 && mkdir -p "$snapshots_dir" "$attempts_dir" && mkdir "$attempt_dir") || contract_fail "cannot create $attempt_dir"

  entries=""
  versions_csv=""
  while IFS=$'\t' read -r version tables restore; do
    versions_csv="${versions_csv:+$versions_csv,}$version"
    for table in ${tables//,/ }; do
      copy="$attempt_dir/$table.copy"
      present="$(contract_scalar "SELECT to_regclass('$table') IS NOT NULL")" || contract_fail "cannot look up table $table"
      case "$present" in t | f) ;; *) contract_fail "unreadable lookup for table $table" ;; esac
      if [ "$present" = f ]; then
        [ -n "${copied[$table]:-}" ] || (umask 077 && set -o noclobber && : >"$copy") || contract_fail "cannot create $copy"
        rows=0
        columns=""
      else
        [ -n "${copied[$table]:-}" ] || (umask 077 && set -o noclobber && contract_psql "COPY $table TO STDOUT" >"$copy") || contract_fail "COPY of $table failed"
        rows="$(contract_scalar "SELECT count(*) FROM $table")" || contract_fail "cannot count the rows of $table"
        [[ "$rows" =~ ^[0-9]+$ ]] || contract_fail "unreadable row count of $table"
        columns="$(contract_scalar "SELECT column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = '$table' ORDER BY ordinal_position")" ||
          contract_fail "cannot read the columns of $table"
      fi
      copied[$table]=1
      absent_flag=()
      [ "$present" != f ] || absent_flag=(--absent)
      entry="$(node "$script_dir/contract-snapshot.mjs" entry --version "$version" --table "$table" --rows "$rows" --copy "$copy" \
        --columns-file /dev/stdin ${absent_flag[@]+"${absent_flag[@]}"} <<<"$columns")" || contract_fail "cannot describe the copy of $table"
      entries+="$entry"$'\n'
    done
  done <<<"$list"
  ledger_latest="$(current_ledger_version)" || contract_fail "cannot read the ledger version"
  ledger_rows="$(contract_scalar "SELECT count(*) FROM schema_migrations")" || contract_fail "cannot count the ledger"
  printf '%s' "$entries" | (umask 077 && node "$script_dir/contract-snapshot.mjs" manifest --out "$attempt_dir/manifest.json" \
    --attempt-id "$contract_attempt_id" --source-sha "$source_sha" --taken-at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --ledger-latest "$ledger_latest" --ledger-rows "$ledger_rows") || contract_fail "cannot write the manifest"
  node "$script_dir/contract-snapshot.mjs" verify --manifest "$attempt_dir/manifest.json" || contract_fail "a copy does not match its manifest"
  manifest_sha="sha256:$(sha256sum "$attempt_dir/manifest.json" | cut -d' ' -f1)"

  (umask 077 && node "$script_dir/contract-snapshot.mjs" record --out "$attempts_dir/$contract_attempt_id.json.tmp" \
    --attempt-id "$contract_attempt_id" --source-sha "$source_sha" --versions "$versions_csv" --snapshot-dir "$attempt_dir" \
    --manifest-sha256 "$manifest_sha" --incumbent-image-id "$incumbent_backend_image_id" \
    --incumbent-digest "${incumbent_backend_image##*@}" --candidate-digest "$backend_digest" \
    --written-at "$(date -u +%Y-%m-%dT%H:%M:%SZ)") || contract_fail "cannot write the attempt record"
  # Flush only this attempt's files and directories; a host-wide sync can
  # stall for minutes on a busy disk while the incumbent is down.
  sync "$attempt_dir"/* "$attempt_dir" "$attempts_dir/$contract_attempt_id.json.tmp" || contract_fail "sync failed"
  mv "$attempts_dir/$contract_attempt_id.json.tmp" "$attempts_dir/$contract_attempt_id.json" || contract_fail "cannot rename the attempt record"
  sync "$attempts_dir" "$snapshots_dir" || contract_fail "sync failed"
  echo "==> contract snapshot written: attempt $contract_attempt_id ($versions_csv)"
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
if [ "$command" != contract-check ]; then
  exec 9>"$deploy_lock"
fi
if [ "$command" != contract-check ] && ! flock -w "$lock_wait_seconds" 9; then
  echo "could not acquire deploy lock within ${lock_wait_seconds}s — another deploy/cutover is running" >&2
  exit 1
fi

if [ "$command" != status ] && [ "$command" != contract-check ]; then
  flags_overlay_prepare
  jev_overlay_prepare
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

  contract-check)
    if [ -n "$check_colour" ] && [ -n "$check_image" ] || [ -z "$check_colour$check_image" ]; then
      echo "contract-check needs exactly one of --colour blue|green or --image REF" >&2
      usage >&2
      exit 2
    fi
    case "$check_colour" in blue | green | "") ;; *) echo "contract-check --colour must be blue or green" >&2; exit 2 ;; esac
    if [ -n "$check_colour" ]; then
      check_label="backend-${check_colour}"
      check_cid="$(compose ps --all -q "$check_label" 2>/dev/null | head -1 || true)"
      if [ -z "$check_cid" ]; then
        echo "contract-check: no $check_label container, running or stopped, to read an image id from (uncertain)" >&2
        exit 1
      fi
      check_image_id="$(docker inspect --format '{{.Image}}' "$check_cid" 2>/dev/null || true)"
    else
      check_label="$check_image"
      check_image_id="$(docker image inspect --format '{{.Id}}' "$check_image" 2>/dev/null || true)"
    fi
    if [ -z "$check_image_id" ]; then
      echo "contract-check: cannot resolve $check_label to an image id (uncertain)" >&2
      exit 1
    fi
    if ! contract_applied_read; then
      echo "contract-check: contract state unreadable: $contract_read_error (uncertain)" >&2
      exit 1
    fi
    if [ -z "$contract_applied" ]; then
      echo "contract-check: no contract migration applied; $check_label image $check_image_id needs no proof"
      exit 0
    fi
    while IFS='|' read -r check_version check_attempt check_restore; do
      echo "applied: $check_version (attempt ${check_attempt:-none}, restore $check_restore)"
    done <<<"$contract_applied"
    contract_proof_rc=0
    contract_image_proof "$check_image_id" || contract_proof_rc=$?
    case "$contract_proof_rc" in
      0) echo "$check_label image $check_image_id: proven, it holds every applied contract migration"; exit 0 ;;
      3) echo "$check_label image $check_image_id: refused, it does not hold every applied contract migration; $(contract_recovery_text)"; exit 3 ;;
      *) echo "contract-check: the image proof on $check_image_id could not run (uncertain)" >&2; exit 1 ;;
    esac
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
    web_repo="$(image_repo "$web_image")"
    web_digest="$(image_digest "$web_image")"
    image_tag="sha-${source_sha}"

    # Step 4 (contract): reject a packet with checksum drift, dirty ledger,
    # invalid indexes, incomplete hooks, or a missing rollback target BEFORE
    # any mutation — the packet is the release's admission proof, not an
    # advisory document.
    echo "==> verifying release packet contract"
    if ! node "$script_dir/release-packet.mjs" verify --packet "$packet" --manifest "$manifest" \
      --migrations-dir "$root_dir/server/migrations" --repo-root "$root_dir"; then
      echo "!! release packet failed contract checks; refusing to cut over" >&2
      exit 1
    fi

    # Released compose files (CHE-2396): the deployed copies are replaced by
    # the verified bundle's before anything renders, so a stale compose dir
    # can never configure the candidate. Still under the deploy lock.
    echo "==> syncing released compose files into $compose_dir"
    if ! node "$script_dir/drift-receipt.mjs" sync --source-root "$root_dir" --compose-dir "$compose_dir"; then
      echo "!! could not sync the released compose files; refusing to cut over" >&2
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

    if ! resolve_slot_ports; then
      echo "!! slot ports unresolved; $from_colour remains active" >&2
      exit 1
    fi
    echo "==> rendered slot ports: backend blue=$BACKEND_BLUE_PORT green=$BACKEND_GREEN_PORT, frontend blue=$FRONTEND_BLUE_PORT green=$FRONTEND_GREEN_PORT"

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
    # CHE-9367 (CHE-9223): exit 75 only for a healthy deferral — admit=false
    # and decision="deferred_contention", before anything is drained. Every
    # other denial (observation_failed, a crash, no or bad JSON, any other
    # decision) and every later gate (final-gate, CHE-6850) exits 1.
    preflight_out=""
    if ! preflight_out="$(node "$script_dir/quiescence.mjs" preflight --database-url "$database_url" --psql-via-docker-exec "$postgres_container")"; then
      printf '%s\n' "$preflight_out"
      echo "!! quiescence preflight denied cutover; $from_colour remains active" >&2
      if printf '%s\n' "$preflight_out" | node -e '
        const lines = require("node:fs").readFileSync(0, "utf8").trim().split("\n");
        let r;
        try { r = JSON.parse(lines[lines.length - 1]); } catch { process.exit(1); }
        process.exit(r !== null && typeof r === "object" && !Array.isArray(r)
          && r.admit === false && r.decision === "deferred_contention" ? 0 : 1);
      ' 2>/dev/null; then
        exit 75
      fi
      exit 1
    fi
    printf '%s\n' "$preflight_out"

    if ! ghcr_login; then
      echo "!! ghcr.io login failed; $from_colour remains active" >&2
      exit 1
    fi

    echo "==> pulling candidate image pair (tag ${image_tag}) for colour=$to_colour"
    # Bounded retry (CHE-823): one GHCR connection reset failed a whole
    # admitted deploy. Nothing is drained yet, so retrying is safe; the
    # digest checks below still pin exactly what was pulled.
    pull_attempt=1
    until MULTICA_BACKEND_IMAGE="$backend_repo" MULTICA_WEB_IMAGE="$web_repo" MULTICA_IMAGE_TAG="$image_tag" \
      compose pull "backend-${to_colour}" "frontend-${to_colour}"; do
      if [ "$pull_attempt" -ge 3 ]; then
        echo "!! image pull failed for colour=$to_colour; $from_colour remains active" >&2
        exit 1
      fi
      echo "!! image pull attempt $pull_attempt/3 failed; retrying in $((pull_attempt * 5))s" >&2
      sleep $((pull_attempt * 5))
      pull_attempt=$((pull_attempt + 1))
    done
    if ! candidate_backend_id="$(verified_image_id "$backend_repo" "$image_tag" "$backend_digest")"; then
      echo "!! pulled backend image digest mismatch for colour=$to_colour; $from_colour remains active" >&2
      exit 1
    fi
    if ! candidate_web_id="$(verified_image_id "$web_repo" "$image_tag" "$web_digest")"; then
      echo "!! pulled web image digest mismatch for colour=$to_colour; $from_colour remains active" >&2
      exit 1
    fi
    echo "==> candidate image ids: backend $candidate_backend_id, web $candidate_web_id (digest-verified)"

    # CHE-7486 N5b-1: a contract migration may already be applied (an earlier
    # attempt, or an out-of-band run). The candidate image id must hold its
    # migration file before anything drains, or the candidate would start on
    # a schema its tree predates. An uncertain read refuses.
    if ! contract_applied_read; then
      echo "!! contract state unreadable ($contract_read_error); refusing to cut over; $from_colour remains active" >&2
      exit 1
    fi
    contract_proof_rc=0
    contract_image_proof "$candidate_backend_id" || contract_proof_rc=$?
    case "$contract_proof_rc" in
      0) ;;
      3)
        echo "!! candidate backend image $candidate_backend_id does not hold contract migration $(contract_applied_versions) (attempt $(contract_applied_attempts)); $from_colour remains active; $(contract_recovery_text)" >&2
        exit 1
        ;;
      *)
        echo "!! contract state unreadable (the image proof on candidate $candidate_backend_id could not run); refusing to cut over; $from_colour remains active" >&2
        exit 1
        ;;
    esac

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

    # Pre-drain agreement gate (CHE-773). Everything the post-drain path
    # relies on is proven while $from_colour still serves, so a mismatch
    # refuses with the API up instead of discovering it with both colours
    # stopped: the incumbent's running bindings equal the rendered ports,
    # the router forwards to exactly those ports, the public route answers
    # as the incumbent's commit, and the candidate's generation renders and
    # passes nginx -t on ITS rendered ports. The incumbent's image pair and
    # commit and the pre-drain ledger are recorded for recover_incumbent.
    echo "==> pre-drain agreement gate (Compose render == running bindings == router generation)"
    if ! published_port_matches "backend-${from_colour}" 8080 "$(slot_port backend "$from_colour")" ||
      ! published_port_matches "frontend-${from_colour}" 3000 "$(slot_port frontend "$from_colour")" ||
      ! router_selects "$from_colour"; then
      echo "!! controller, Compose and router disagree on $from_colour's ports; refusing before drain — $from_colour remains active" >&2
      exit 1
    fi
    incumbent_commit="$(health_commit "$(slot_port backend "$from_colour")" || true)"
    incumbent_backend_image="$(running_image_ref "backend-${from_colour}" || true)"
    incumbent_web_image="$(running_image_ref "frontend-${from_colour}" || true)"
    incumbent_backend_image_id="$(container_image_id "backend-${from_colour}" || true)"
    incumbent_web_image_id="$(container_image_id "frontend-${from_colour}" || true)"
    incumbent_ledger="$(current_ledger_version || true)"
    if [ -z "$incumbent_commit" ] || [ -z "$incumbent_backend_image" ] || [ -z "$incumbent_web_image" ] ||
      [ -z "$incumbent_backend_image_id" ] || [ -z "$incumbent_web_image_id" ] || [ -z "$incumbent_ledger" ]; then
      echo "!! could not record $from_colour's commit/image pair/ledger (needed to prove a recovery); refusing before drain — $from_colour remains active" >&2
      exit 1
    fi
    if ! public_route_serves "$(slot_port backend "$from_colour")" "$incumbent_commit"; then
      echo "!! public route (router :$router_api_port/:$router_web_port) does not serve $from_colour commit $incumbent_commit; refusing before drain — $from_colour remains active" >&2
      exit 1
    fi
    if ! bash "$script_dir/router.sh" validate --colour "$to_colour" --state-dir "$router_state_dir" >/dev/null; then
      echo "!! candidate router generation for $to_colour failed validation; refusing before drain — $from_colour remains active" >&2
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
    if ! stop_confirmed "backend-${from_colour}" "frontend-${from_colour}"; then
      alert_outage "drain of $from_colour NOT confirmed; final gate and migration NOT run and $to_colour NOT started (never migrate under, or start beside, a possibly-live backend); $from_colour may be partially stopped — check 'docker compose ps' and follow the runbook"
      exit 1
    fi

    # Step 3: final gate immediately before the first candidate mutation —
    # strict, zero-tolerance. A denial here means $from_colour is ALREADY
    # stopped (see above): the API is down and needs manual intervention to
    # restart it, not the "colour remains active" framing that was true
    # before this ordering fix.
    echo "==> quiescence final-gate (pre-migration)"
    if ! node "$script_dir/quiescence.mjs" final-gate --database-url "$database_url" --psql-via-docker-exec "$postgres_container"; then
      recover_incumbent "quiescence final-gate denied cutover after draining $from_colour (no migration ran)"
    fi

    contract_snapshot_gate

    # Run the standalone migrator exactly once, from the candidate image,
    # against the inactive colour's (stopped) service identity — never the
    # currently-serving colour's, so the one-shot container's --no-deps
    # never risks touching a live serving container's dependency graph.
    echo "==> running migration step (one-shot, candidate image, before candidate starts)"
    if ! MULTICA_CONTRACT_ATTEMPT="$contract_attempt_id" run_migration_step "$backend_repo" "$image_tag" up; then
      # No automatic recovery here: a failed migration can leave DDL the
      # ledger does not record, so ledger proof cannot establish that
      # $from_colour is compatible with the schema actually in place.
      alert_outage "migration step failed after draining $from_colour; schema state is unproven, so neither colour was started — re-run the quiescent gate and inspect the schema before restarting anything"
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
      recover_incumbent "candidate colour=$to_colour failed to start"
    fi

    # The candidate's RUNNING containers must be the digest-verified images,
    # not a stale container Compose chose to keep.
    if ! runs_verified_image "backend-${to_colour}" "$candidate_backend_id" ||
      ! runs_verified_image "frontend-${to_colour}" "$candidate_web_id"; then
      recover_incumbent "candidate colour=$to_colour is not running the manifest-verified images"
    fi

    # The candidate's ACTUAL published bindings must equal the rendered
    # ports every later check and the router use.
    candidate_port="$(slot_port backend "$to_colour")"
    candidate_web_port="$(slot_port frontend "$to_colour")"
    if ! published_port_matches "backend-${to_colour}" 8080 "$candidate_port" ||
      ! published_port_matches "frontend-${to_colour}" 3000 "$candidate_web_port"; then
      recover_incumbent "candidate colour=$to_colour published bindings differ from the rendered ports"
    fi
    echo "==> health-checking candidate colour=$to_colour on port $candidate_port"
    if ! wait_ready_on_port "$candidate_port" 180; then
      recover_incumbent "candidate colour=$to_colour did not become ready on port $candidate_port within 180s"
    fi
    if ! wait_http_ok "http://127.0.0.1:${candidate_web_port}/" 120; then
      recover_incumbent "candidate frontend-$to_colour did not answer on port $candidate_web_port within 120s"
    fi
    if ! frontend_flags_match "$candidate_port"; then
      recover_incumbent "frontend flag verification failed for candidate colour=$to_colour"
    fi

    # Require the EXACT expected /health commit — a 200 alone only proves
    # something is listening on the port, not that it is the release just
    # migrated and started (see deploy-lib.sh's verify_health_identity).
    echo "==> verifying candidate /health identity"
    if ! verify_health_identity "$candidate_port" "$source_sha"; then
      recover_incumbent "candidate colour=$to_colour /health identity mismatch"
    fi

    # Deploy drift receipt (CHE-2396): deployed compose bytes, env key names
    # (never values) and running image digests must equal the released
    # source and manifest. Any difference fails the deploy before traffic moves.
    echo "==> deploy drift receipt"
    drift_services=()
    for svc in "backend-${to_colour}" "frontend-${to_colour}"; do
      drift_services+=(--service "${svc}=$(compose ps -q "$svc" | head -1)")
    done
    # The interpolated render holds secrets: stream it, never write it to disk.
    if ! compose config --format json | node "$script_dir/drift-receipt.mjs" check \
      --source-root "$root_dir" --compose-dir "$compose_dir" --config /dev/stdin --manifest "$manifest" \
      "${drift_services[@]}" --output "$state_dir/drift-receipt.json"; then
      recover_incumbent "deploy drift receipt: candidate colour=$to_colour differs from the released source (see $state_dir/drift-receipt.json)"
    fi

    # Activate the complete router generation. router.sh's own `nginx -t`
    # gate means an invalid config never reaches the running router — this
    # is the "prepare, validate, atomically replace, reload" sequence from
    # the accepted architecture, delegated wholesale rather than
    # reimplemented here.
    echo "==> activating router generation for colour=$to_colour"
    if ! bash "$script_dir/router.sh" select --colour "$to_colour" --state-dir "$router_state_dir"; then
      recover_incumbent "router generation switch failed; candidate colour=$to_colour is running but NOT receiving traffic"
    fi
    if ! router_selects "$to_colour" || ! public_route_serves "$candidate_port" "$source_sha"; then
      recover_incumbent "router was switched but the public route does not read back as $to_colour commit $source_sha"
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

    if ! resolve_slot_ports; then
      echo "!! slot ports unresolved; refusing rollback before drain" >&2
      exit 1
    fi
    incumbent_commit="$(health_commit "$(slot_port backend "$from_colour")" || true)"
    incumbent_backend_image="$(running_image_ref "backend-${from_colour}" || true)"
    incumbent_web_image="$(running_image_ref "frontend-${from_colour}" || true)"
    incumbent_backend_image_id="$(container_image_id "backend-${from_colour}" || true)"
    incumbent_web_image_id="$(container_image_id "frontend-${from_colour}" || true)"
    incumbent_ledger="$(current_ledger_version || true)"

    # CHE-7486 N5b-2: with a contract applied, prove the exact stopped
    # containers first. Refuses (exit 3) before anything is drained.
    rollback_contract_binding

    # Drain and stop the failing candidate BEFORE the strict quiescence
    # final-gate runs — same root cause and same fix as the `cutover)`
    # branch above (CHE-655): $from_colour is the failing candidate here,
    # and its own connection pool holds idle sessions for as long as it
    # keeps running, so the gate could never admit while it still serves.
    # Without this, the automated rollback refuses unconditionally and
    # leaves the ALREADY-FAILING candidate as the only thing "active" —
    # exactly the outage rollback exists to end.
    echo "==> draining and stopping failing candidate colour=$from_colour (required before the quiescence final-gate can ever admit)"
    if ! stop_confirmed "backend-${from_colour}" "frontend-${from_colour}"; then
      echo "!! drain of failing candidate $from_colour NOT confirmed; final gate NOT run and $to_colour NOT started (never two backends on one DB) — MANUAL INTERVENTION REQUIRED" >&2
      exit 1
    fi

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

    backend_port="$(slot_port backend "$to_colour")"
    echo "==> starting retained predecessor colour=$to_colour without migrations"
    if [ "$rollback_bound" -eq 1 ]; then
      # A contract is applied: start the proven stopped containers themselves.
      # `compose up` could recreate them from a re-pointed tag.
      if ! compose start "backend-${to_colour}" "frontend-${to_colour}"; then
        echo "!! failed to start retained predecessor colour=$to_colour — MANUAL INTERVENTION REQUIRED" >&2
        exit 1
      fi
      started_backend_id="$(container_image_id "backend-${to_colour}" || true)"
      started_web_id="$(container_image_id "frontend-${to_colour}" || true)"
      if [ "$started_backend_id" != "$rollback_proven_backend_id" ] || [ "$started_web_id" != "$rollback_proven_web_id" ]; then
        rollback_stop_note="stopped again"
        stop_confirmed "backend-${to_colour}" "frontend-${to_colour}" || rollback_stop_note="stopping them again was NOT confirmed"
        alert_outage "rollback started an image other than the proven one (backend ${started_backend_id:-<none>} want $rollback_proven_backend_id, frontend ${started_web_id:-<none>} want $rollback_proven_web_id); $rollback_stop_note"
        exit 1
      fi
    elif ! MULTICA_SKIP_MIGRATIONS=1 compose up -d --no-deps "backend-${to_colour}" "frontend-${to_colour}"; then
      echo "!! failed to start retained predecessor colour=$to_colour — MANUAL INTERVENTION REQUIRED" >&2
      exit 1
    fi

    if ! published_port_matches "backend-${to_colour}" 8080 "$backend_port" ||
      ! published_port_matches "frontend-${to_colour}" 3000 "$(slot_port frontend "$to_colour")" ||
      ! wait_ready_on_port "$backend_port" 120; then
      echo "!! retained predecessor colour=$to_colour did not become healthy within 120s — MANUAL INTERVENTION REQUIRED" >&2
      exit 1
    fi
    if ! frontend_flags_match "$backend_port"; then
      recover_incumbent "frontend flag verification failed for retained predecessor colour=$to_colour"
    fi

    echo "==> restoring router generation for colour=$to_colour"
    if ! bash "$script_dir/router.sh" select --colour "$to_colour" --state-dir "$router_state_dir"; then
      echo "!! router restore failed — retained predecessor colour=$to_colour is healthy but NOT receiving traffic; MANUAL INTERVENTION REQUIRED" >&2
      exit 1
    fi
    if ! router_selects "$to_colour" || ! public_route_serves "$backend_port" "$(health_commit "$backend_port")"; then
      echo "!! router restore did not read back — the public route does not serve $to_colour; MANUAL INTERVENTION REQUIRED" >&2
      exit 1
    fi

    # The retained predecessor's ACTUAL running image — never the pre-
    # rollback state file's own image_tuple, which still holds the
    # candidate's tuple: cutover-state.json only ever records the ONE
    # active colour's image_tuple at a time, and $from_colour (the failing
    # candidate) was still active when it was last written. Reading it
    # here would just copy the candidate's own record onto the colour that
    # replaced it, instead of what $to_colour actually started from
    # (CHE-678). Ask Docker what $to_colour is actually running.
    backend_image="$(running_image_ref "backend-${to_colour}")" || backend_image=""
    web_image="$(running_image_ref "frontend-${to_colour}")" || web_image=""
    if [ -z "$backend_image" ] || [ -z "$web_image" ]; then
      echo "!! could not resolve the running image for retained predecessor colour=$to_colour after starting it — refusing to record an unverified image tuple in $cutover_state_file; $to_colour is healthy and serving, but $cutover_state_file needs manual correction; MANUAL INTERVENTION REQUIRED" >&2
      exit 1
    fi
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
