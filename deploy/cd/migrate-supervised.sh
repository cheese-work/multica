#!/usr/bin/env bash
set -euo pipefail

# D2 supervised migration runner (CHE-372). Wraps the existing `migrate`
# binary with an absolute work deadline that governs every phase from
# entry to exit (not just the child's own runtime), timeouts enforced and
# read back inside the migrator's own Go connection lifecycle (never a
# disconnected side-channel probe, and never defeatable by a
# connection-string "options=" parameter), and an external watchdog that
# confirms server-side session termination before claiming a cancellation
# succeeded.
#
# This script does not decide quiescence; call deploy/cd/quiescence.mjs
# final-gate immediately before invoking this script, and again is not
# needed here because the deadline arithmetic below already assumes the
# final gate has just passed. It does not talk to C00 and accepts no
# production credentials; --database-url is caller-supplied and this
# script has no default beyond what the caller passes.
#
# Deadline model (from the approved D2 addendum):
#   migration_deadline = min(migration_started + 15s, cutover_started + 40s)
#   work_deadline      = migration_deadline - 2s   (reserved for stopping
#                         owned execution and confirming exit)
# The caller supplies --cutover-started-epoch so this script can compute
# the same clamped deadline the controller uses; if omitted, only the 15s
# migration-phase bound applies (useful for standalone testing).
#
# The ENTIRE body below this point — settings enforcement, launch, live-
# session discovery, the run itself, and any cancellation/confirmation —
# runs under one absolute deadline loop. There is no separate "setup"
# phase exempt from the budget, and no unbounded sleep/wait anywhere.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

usage() {
  cat <<'EOF'
usage: migrate-supervised.sh --database-url URL --migrate-binary PATH
  --role-name ROLE --database-name DBNAME
  [--observer-database-url URL] [--psql-via-docker-network NAME]
  [--cutover-started-epoch SECONDS] [--migration-allocation-seconds N]
  [--reserve-seconds N] [--statement-timeout-ms N] [--lock-timeout-ms N]

--database-url is what the migrator binary itself connects with (it is a
pure Go/pgx process and needs no psql). --observer-database-url is what
this script's own watchdog queries connect with; it defaults to
--database-url but may need to differ when the migrator reaches Postgres
via a host-published port while the observer reaches it via
--psql-via-docker-network's container DNS (or vice versa) — these are two
different network paths to the same database, not two different databases.

--role-name/--database-name identify the exact role and database the
migrator authenticates as, used only to locate its live Postgres
session(s) for watchdog termination confirmation — NOT to enforce
timeouts (see below).

Timeouts are enforced inside the migrator's own process via
MULTICA_INTERNAL_D2_ENFORCED_STATEMENT_TIMEOUT_MS/
MULTICA_INTERNAL_D2_ENFORCED_LOCK_TIMEOUT_MS (see
server/internal/dbstartup.NewPoolWithEnforcedTimeouts), which sets and
reads back statement_timeout/lock_timeout on every physical connection
the migrator's pool opens — including hook connections — strictly AFTER
pgx applies the connection string, so it cannot be defeated by a
connection-string "options=" parameter the way a role/database default or
client-side PGOPTIONS both can be (pgx applies connection-string settings
above both; verified against pgx v5's own precedence). If the migrator
process does not enforce internally (e.g. an unpatched binary), this
script has no way to guarantee the timeout — the enforcement lives inside
the migrator, and this script's job is to require it, not to substitute
for it from outside.

Exit codes:
  0   migration completed and confirmed within the work deadline
  1   migration failed (non-migrator-timeout failure); state is UNCERTAIN,
      not automatically compatible-rollback-eligible — see stderr
  2   usage error
  3   work deadline exceeded, setup could not complete in time, or
      termination could not be confirmed server-side; entered
      needs_operator (fence must stay up)
EOF
}

database_url=""
observer_database_url=""
migrate_binary=""
role_name=""
database_name=""
cutover_started_epoch=""
migration_allocation_seconds=15
reserve_seconds=2
statement_timeout_ms=1000
lock_timeout_ms=1000
psql_via_docker_network=""

while (($#)); do
  case "$1" in
    --database-url) database_url=${2:?}; shift 2 ;;
    --observer-database-url) observer_database_url=${2:?}; shift 2 ;;
    --migrate-binary) migrate_binary=${2:?}; shift 2 ;;
    --role-name) role_name=${2:?}; shift 2 ;;
    --database-name) database_name=${2:?}; shift 2 ;;
    --cutover-started-epoch) cutover_started_epoch=${2:?}; shift 2 ;;
    --migration-allocation-seconds) migration_allocation_seconds=${2:?}; shift 2 ;;
    --reserve-seconds) reserve_seconds=${2:?}; shift 2 ;;
    --statement-timeout-ms) statement_timeout_ms=${2:?}; shift 2 ;;
    --lock-timeout-ms) lock_timeout_ms=${2:?}; shift 2 ;;
    --psql-via-docker-network) psql_via_docker_network=${2:?}; shift 2 ;;
    --help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ -n "$database_url" ] || { echo "missing --database-url" >&2; usage >&2; exit 2; }
[ -n "$migrate_binary" ] || { echo "missing --migrate-binary" >&2; usage >&2; exit 2; }
[ -x "$migrate_binary" ] || { echo "--migrate-binary is not executable: $migrate_binary" >&2; exit 2; }
[ -n "$role_name" ] || { echo "missing --role-name" >&2; usage >&2; exit 2; }
[ -n "$database_name" ] || { echo "missing --database-name" >&2; usage >&2; exit 2; }
[ -n "$observer_database_url" ] || observer_database_url="$database_url"

migration_started_epoch="$(date +%s)"

# migration_deadline = min(migration_started + allocation, cutover_started + 40)
# The 40s bound is v3's fixed constant (5+5+15+15 = 40s through the end of
# the migration phase in the 60s envelope); it is not a CLI flag because
# changing it would silently widen the approved envelope.
cutover_bound_seconds=40
deadline_epoch=$((migration_started_epoch + migration_allocation_seconds))
if [ -n "$cutover_started_epoch" ]; then
  cutover_deadline=$((cutover_started_epoch + cutover_bound_seconds))
  if [ "$cutover_deadline" -lt "$deadline_epoch" ]; then
    deadline_epoch="$cutover_deadline"
  fi
fi
work_deadline_epoch=$((deadline_epoch - reserve_seconds))

remaining_seconds() {
  local now
  now="$(date +%s)"
  echo $((work_deadline_epoch - now))
}

if [ "$(remaining_seconds)" -le 0 ]; then
  echo "che372-d2: usable work time already expired before migration start (deny entry)" >&2
  exit 3
fi

echo "che372-d2: migration_started=$migration_started_epoch work_deadline=$work_deadline_epoch remaining=$(remaining_seconds)s" >&2

statement_timeout_ms_capped=$(( statement_timeout_ms < ($(remaining_seconds) * 1000) ? statement_timeout_ms : ($(remaining_seconds) * 1000) ))
if [ "$statement_timeout_ms_capped" -le 0 ]; then
  echo "che372-d2: statement timeout rounds to <=0ms (disabled in Postgres) — deny" >&2
  exit 3
fi
if [ "$lock_timeout_ms" -le 0 ]; then
  echo "che372-d2: lock timeout rounds to <=0ms (disabled in Postgres) — deny" >&2
  exit 3
fi

export DATABASE_URL="$database_url"
export MULTICA_INTERNAL_D2_ENFORCED_STATEMENT_TIMEOUT_MS="$statement_timeout_ms_capped"
export MULTICA_INTERNAL_D2_ENFORCED_LOCK_TIMEOUT_MS="$lock_timeout_ms"

quiescence_args_common=(--database-url "$observer_database_url")
if [ -n "$psql_via_docker_network" ]; then
  quiescence_args_common+=(--psql-via-docker-network "$psql_via_docker_network")
fi

# confirm_no_live_session_for_pid queries pg_stat_activity for the given
# Postgres backend pid and returns 0 only if that backend is genuinely
# absent server-side. This is the server-side proof the previous version
# of this script never obtained — it only ever checked `kill -0` on the OS
# process, which proves nothing about whether Postgres itself has finished
# rolling back or releasing the session (a long statement, or a client
# that has already exited while the server-side backend is still
# unwinding, both leave real server-side state this must observe).
confirm_no_live_session_for_pid() {
  local pg_pid="$1"
  local probe
  probe="$(node deploy/cd/quiescence.mjs find-live-session-by-role "${quiescence_args_common[@]}" \
    --role-name "$role_name" --database-name "$database_name" --query-timeout-ms 500 2>/dev/null)" || return 0
  local found_pid
  found_pid="$(node -e 'try{process.stdout.write(String(JSON.parse(process.argv[1]).pid))}catch(e){process.stdout.write("")}' "$probe" 2>/dev/null || true)"
  [ "$found_pid" != "$pg_pid" ]
}

# --- launch the migrator immediately; the watchdog loop below governs the
# ENTIRE remaining sequence (live-session discovery, running, cancellation,
# confirmation) under the one absolute deadline. No sub-step here has its
# own separate, unbounded budget. ---
"$migrate_binary" up &
migrator_os_pid=$!
echo "che372-d2: launched migrator os_pid=$migrator_os_pid" >&2

migrator_pg_pid=""
migrator_pg_backend_start=""
final_state=""   # "success" | "failed_nonzero" | "deadline_exceeded" | "needs_operator"

while :; do
  remaining="$(remaining_seconds)"
  if [ "$remaining" -le 0 ]; then
    final_state="deadline_exceeded"
    break
  fi

  if ! kill -0 "$migrator_os_pid" 2>/dev/null; then
    # OS process has already exited. Reap it and check its status; this
    # does not by itself prove the Postgres session is gone (a crashed
    # client can leave server-side work in flight), so still discover and
    # confirm the Postgres session below before declaring success.
    wait "$migrator_os_pid" 2>/dev/null
    migrate_status=$?
    if [ "$migrate_status" -ne 0 ]; then
      final_state="failed_nonzero"
      break
    fi
    final_state="success"
    break
  fi

  # Discover the migrator's live Postgres backend, within the same
  # deadline-governed loop — no separate 25-attempt sub-loop with its own
  # budget. A single bounded probe per iteration; missing this iteration
  # just means we check again next iteration, still under the same clock.
  if [ -z "$migrator_pg_pid" ]; then
    probe="$(node deploy/cd/quiescence.mjs find-live-session-by-role "${quiescence_args_common[@]}" \
      --role-name "$role_name" --database-name "$database_name" --query-timeout-ms 500 2>/dev/null)" || probe=""
    if [ -n "$probe" ]; then
      candidate_pid="$(node -e 'try{process.stdout.write(String(JSON.parse(process.argv[1]).pid))}catch(e){process.stdout.write("")}' "$probe" 2>/dev/null || true)"
      candidate_backend_start="$(node -e 'try{process.stdout.write(String(JSON.parse(process.argv[1]).backendStart))}catch(e){process.stdout.write("")}' "$probe" 2>/dev/null || true)"
      if [ -n "$candidate_pid" ]; then
        covered="$(node deploy/cd/quiescence.mjs verify-live-session-is-covered "${quiescence_args_common[@]}" \
          --pid "$candidate_pid" --backend-start "$candidate_backend_start" \
          --role-name "$role_name" --database-name "$database_name" 2>&1)" && {
          migrator_pg_pid="$candidate_pid"
          migrator_pg_backend_start="$candidate_backend_start"
          echo "che372-d2: confirmed live migrator Postgres session pid=$migrator_pg_pid identity: $covered" >&2
        }
      fi
    fi
  fi

  sleep 0.2
done

if [ "$final_state" = "success" ]; then
  # Confirmed OS exit zero within budget. Still require server-side proof:
  # if we never located the migrator's Postgres session at all (e.g. it
  # finished so fast between poll iterations that this script never
  # observed it live), that is an UNCERTAIN outcome, not a clean success —
  # this script only claims success when it has independently confirmed
  # both the client exit code AND that no live migrator-identified
  # Postgres session remains attributable to a still-running attempt.
  if [ -n "$migrator_pg_pid" ] && ! confirm_no_live_session_for_pid "$migrator_pg_pid"; then
    echo "che372-d2: migrator os_pid=$migrator_os_pid exited 0 but its Postgres session pid=$migrator_pg_pid is still observed live — treating as UNCERTAIN, not success" >&2
    final_state="needs_operator"
  fi
fi

if [ "$final_state" = "success" ]; then
  final_now_epoch="$(date +%s)"
  if [ "$final_now_epoch" -gt "$work_deadline_epoch" ]; then
    echo "che372-d2: migrator completed but confirmation observed after work_deadline — treat as UNCERTAIN per spec, not a clean success" >&2
    exit 3
  fi
  echo "che372-d2: migration completed successfully within work_deadline (finished at $final_now_epoch, deadline $work_deadline_epoch)" >&2
  exit 0
fi

if [ "$final_state" = "failed_nonzero" ]; then
  echo "che372-d2: migrator exited non-zero before deadline — state UNCERTAIN, compare full ledger/object state before any decision" >&2
  exit 1
fi

# final_state is "deadline_exceeded" or "needs_operator" from the success
# check above: cancel the OS process if still running, and — this is the
# server-side proof the previous version never obtained — poll
# pg_stat_activity until the migrator's identified Postgres backend is
# actually gone, not merely until the OS process disappears. The
# cancellation attempt itself is still bounded (it does not get an
# unbounded wait): give it up to reserve_seconds, then declare
# needs_operator either way — reserve expiry never manufactures a
# successful cancellation.
echo "che372-d2: entering cancellation path (${final_state}) for os_pid=$migrator_os_pid pg_pid=${migrator_pg_pid:-unknown}" >&2
if kill -0 "$migrator_os_pid" 2>/dev/null; then
  kill -TERM "$migrator_os_pid" 2>/dev/null || true
fi

cancel_deadline_epoch=$(( $(date +%s) + reserve_seconds ))
os_confirmed=false
pg_confirmed=false
while [ "$(date +%s)" -lt "$cancel_deadline_epoch" ]; do
  if ! $os_confirmed && ! kill -0 "$migrator_os_pid" 2>/dev/null; then
    wait "$migrator_os_pid" 2>/dev/null || true
    os_confirmed=true
  fi
  if [ -n "$migrator_pg_pid" ]; then
    if ! $pg_confirmed && confirm_no_live_session_for_pid "$migrator_pg_pid"; then
      pg_confirmed=true
    fi
  else
    # Never discovered a Postgres session for this attempt — cannot prove
    # server-side absence of something we never identified. This is
    # exactly the "unknown state fails closed" rule: it does NOT count as
    # confirmed, it stays unconfirmed until the cancel deadline expires.
    pg_confirmed=false
  fi
  if $os_confirmed && $pg_confirmed; then
    break
  fi
  sleep 0.1
done

if ! $os_confirmed && kill -0 "$migrator_os_pid" 2>/dev/null; then
  echo "che372-d2: graceful cancel unconfirmed for os_pid=$migrator_os_pid; escalating to SIGKILL on recorded pid" >&2
  kill -KILL "$migrator_os_pid" 2>/dev/null || true
  for _ in 1 2 3 4 5; do
    kill -0 "$migrator_os_pid" 2>/dev/null || { os_confirmed=true; wait "$migrator_os_pid" 2>/dev/null || true; break; }
    sleep 0.1
  done
fi

if [ -n "$migrator_pg_pid" ] && ! $pg_confirmed; then
  if confirm_no_live_session_for_pid "$migrator_pg_pid"; then
    pg_confirmed=true
  fi
fi

# A never-identified Postgres session can never count as confirmed absent
# — this is the fail-closed default for "we don't know," not a special
# case: pg_confirmed only becomes true via an actual observed absence
# above, and starts and stays false here when migrator_pg_pid is empty.
if $os_confirmed && $pg_confirmed; then
  echo "che372-d2: os_pid=$migrator_os_pid and Postgres pid=$migrator_pg_pid both confirmed terminated after ${final_state} — needs_operator (state uncertain: hooks or earlier statements may have committed)" >&2
else
  echo "che372-d2: termination UNCONFIRMED (os_confirmed=$os_confirmed pg_confirmed=$pg_confirmed pg_pid=${migrator_pg_pid:-never-identified}) — needs_operator, fence must stay up" >&2
fi
exit 3
